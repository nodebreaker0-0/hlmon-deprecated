// validator_exporter.go
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics
var (
	validatorAddr    = flag.String("validator-address", "", "Validator address")
	consensusLogPath = flag.String("consensus-path", "", "Path to consensus logs")
	statusLogPath    = flag.String("status-path", "", "Path to status logs")
	logLevel         = flag.String("log-level", "info", "Log level")

	LastVoteRound = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "validator_vote_last_round", Help: "Last vote round"},
		[]string{"validator"},
	)
	CurrentRound = prometheus.NewGauge(
		prometheus.GaugeOpts{Name: "current_round", Help: "Current consensus round"},
	)
	AckDelaySeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "validator_heartbeat_ack_delay_seconds", Help: "Heartbeat ack delay (sec)"},
		[]string{"validator"},
	)
	DisconnectedValidator = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{Name: "validator_disconnected", Help: "Validator disconnection state"},
		[]string{"source", "target", "last_round"},
	)
)

// Internal state
var (
	heartbeatSent    sync.Map
	voteMu           sync.RWMutex
	roundMu          sync.RWMutex
	lastVoteRound    float64
	lastCurrentRound float64
	shortAddr        string
)

func main() {
	flag.Parse()
	if *validatorAddr == "" || *consensusLogPath == "" || *statusLogPath == "" {
		log.Fatal("Missing required flags")
	}

	shortAddr = shortenAddress(*validatorAddr)
	log.Printf("[INFO] validator: %s", shortAddr)

	prometheus.MustRegister(LastVoteRound, CurrentRound, AckDelaySeconds, DisconnectedValidator)

	go startTailLoop(*consensusLogPath)
	go startStatusScanner(*statusLogPath)

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":9101", nil))
}

func shortenAddress(addr string) string {
	if len(addr) < 12 {
		return addr
	}
	return addr[:8] + "..." + addr[len(addr)-4:]
}

func startTailLoop(base string) {
	for {
		path := getLatestFile(base)
		if path != "" {
			tailLog(path)
		}
		time.Sleep(10 * time.Second)
	}
}

func getLatestFile(base string) string {
	today := time.Now().Format("20060102")
	dir := filepath.Join(base, today)
	entries, err := os.ReadDir(dir)
	if err != nil {
		log.Printf("[WARN] read dir error: %v", err)
		return ""
	}
	var hours []int
	for _, e := range entries {
		if n, err := strconv.Atoi(e.Name()); err == nil {
			hours = append(hours, n)
		}
	}
	if len(hours) == 0 {
		return ""
	}
	sort.Sort(sort.Reverse(sort.IntSlice(hours)))
	return filepath.Join(dir, fmt.Sprintf("%d", hours[0]))
}

func tailLog(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[WARN] open error: %v", err)
		return
	}
	defer f.Close()
	f.Seek(0, 2)
	buf := make([]byte, 4096)
	for {
		n, err := f.Read(buf)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		lines := strings.Split(string(buf[:n]), "\n")
		for _, line := range lines {
			if line != "" {
				handleLine(line)
			}
		}
	}
}

func handleLine(line string) {
	var entry []interface{}
	if err := json.Unmarshal([]byte(line), &entry); err != nil || len(entry) < 2 {
		return
	}
	inner, ok := entry[1].([]interface{})
	if !ok || len(inner) != 2 {
		return
	}
	dir, _ := inner[0].(string)
	content, _ := inner[1].(map[string]interface{})
	if content == nil {
		return
	}
	if msg, ok := content["msg"].(map[string]interface{}); ok {
		content = msg
	}
	for key, val := range content {
		switch key {
		case "Heartbeat":
			if dir == "out" {
				handleHeartbeat(val)
			}
		case "HeartbeatAck":
			if dir == "in" {
				handleAck(val)
			}
		case "Vote":
			if dir == "out" {
				handleVote(val)
			}
		case "Block":
			if dir == "in" {
				handleBlock(val)
			}
		}
	}
}

func handleHeartbeat(val interface{}) {
	hb, _ := val.(map[string]interface{})
	valStr, _ := hb["validator"].(string)
	ridRaw, _ := hb["random_id"]
	if valStr != shortAddr && !strings.HasPrefix(valStr, shortAddr[:6]) {
		return
	}
	rid, _ := ridRaw.(json.Number)
	heartbeatSent.Store(rid.String(), time.Now().UnixNano())
}

func handleAck(val interface{}) {
	ack, _ := val.(map[string]interface{})
	ridRaw, _ := ack["random_id"].(json.Number)
	validator, _ := ack["validator"].(string)
	sentAt, ok := heartbeatSent.LoadAndDelete(ridRaw.String())
	if ok {
		delay := float64(time.Now().UnixNano()-sentAt.(int64)) / 1e9
		AckDelaySeconds.WithLabelValues(validator).Set(delay)
	}
}

func handleVote(val interface{}) {
	outer, _ := val.(map[string]interface{})
	vote, _ := outer["vote"].(map[string]interface{})
	valStr, _ := vote["validator"].(string)
	if valStr != shortAddr {
		return
	}
	roundRaw, _ := vote["round"].(json.Number)
	round, _ := roundRaw.Int64()

	voteMu.RLock()
	skip := lastVoteRound == float64(round)
	voteMu.RUnlock()
	if skip {
		return
	}
	voteMu.Lock()
	if lastVoteRound != float64(round) {
		LastVoteRound.WithLabelValues(valStr).Set(float64(round))
		lastVoteRound = float64(round)
	}
	voteMu.Unlock()
}

func handleBlock(val interface{}) {
	blockOuter, _ := val.(map[string]interface{})
	block, _ := blockOuter["Block"].(map[string]interface{})
	roundRaw, _ := block["round"].(json.Number)
	round, _ := roundRaw.Int64()

	roundMu.RLock()
	skip := lastCurrentRound == float64(round)
	roundMu.RUnlock()
	if skip {
		return
	}
	roundMu.Lock()
	if lastCurrentRound != float64(round) {
		CurrentRound.Set(float64(round))
		lastCurrentRound = float64(round)
	}
	roundMu.Unlock()
}

func startStatusScanner(base string) {
	for {
		path := getLatestFile(base)
		if path != "" {
			scanStatus(path)
		}
		time.Sleep(30 * time.Second)
	}
}

func scanStatus(path string) {
	f, err := os.Open(path)
	if err != nil {
		log.Printf("[WARN] status open error: %v", err)
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lastLine string
	for scanner.Scan() {
		lastLine = scanner.Text()
	}
	if lastLine != "" {
		handleStatusLine(lastLine)
	}
}

func handleStatusLine(line string) {
	var entry []interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &entry); err != nil || len(entry) < 2 {
		return
	}
	payload, ok := entry[1].(map[string]interface{})
	if !ok {
		return
	}
	if raw, ok := payload["disconnected_validators"]; ok {
		dis := raw.([]interface{})
		for _, item := range dis {
			pair := item.([]interface{})
			target := pair[0].(string)
			sources := pair[1].([]interface{})
			for _, s := range sources {
				srcPair := s.([]interface{})
				source := srcPair[0].(string)
				lastRound := int(srcPair[1].(float64))
				DisconnectedValidator.WithLabelValues(source, target, fmt.Sprintf("%d", lastRound)).Set(1)
			}
		}
	}
}
