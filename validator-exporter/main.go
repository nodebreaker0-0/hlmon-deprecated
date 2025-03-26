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
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

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

var (
	heartbeatCache *lru.Cache
	voteCache      *lru.Cache
	roundCache     *lru.Cache
	shortAddr      string
)

func main() {
	flag.Parse()
	if *validatorAddr == "" || *consensusLogPath == "" || *statusLogPath == "" {
		log.Fatal("Missing required flags")
	}

	shortAddr = shortenAddress(*validatorAddr)
	log.Printf("[INFO] validator: %s", shortAddr)

	prometheus.MustRegister(LastVoteRound, CurrentRound, AckDelaySeconds, DisconnectedValidator)

	heartbeatCache, _ = lru.New(5000)
	voteCache, _ = lru.New(5000)
	roundCache, _ = lru.New(1000)

	go startTailLoop(*consensusLogPath)
	go startStatusScan(*statusLogPath)

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

func startStatusScan(base string) {
	for {
		path := getLatestFile(base)
		if path != "" {
			scanStatus(path)
		}
		time.Sleep(30 * time.Second)
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
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		handleLine(strings.TrimSpace(line))
	}
}

func scanStatus(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	var lastLine string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		lastLine = scanner.Text()
	}
	if lastLine != "" {
		handleStatus(lastLine)
	}
}

func handleStatus(line string) {
	var entry []interface{}
	if err := json.Unmarshal([]byte(line), &entry); err != nil || len(entry) < 2 {
		return
	}
	payload, ok := entry[1].(map[string]interface{})
	if !ok {
		return
	}
	if raw, ok := payload["disconnected_validators"].([]interface{}); ok {
		for _, item := range raw {
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
	heartbeatCache.Add(rid.String(), time.Now().UnixNano())
}

func handleAck(val interface{}) {
	ack, _ := val.(map[string]interface{})
	ridRaw, _ := ack["random_id"].(json.Number)
	validator, _ := ack["validator"].(string)
	if sentAtRaw, ok := heartbeatCache.Get(ridRaw.String()); ok {
		delay := float64(time.Now().UnixNano()-sentAtRaw.(int64)) / 1e6
		AckDelaySeconds.WithLabelValues(validator).Set(delay)
		heartbeatCache.Remove(ridRaw.String())
	}
}

func handleVote(val interface{}) {
	outer, _ := val.(map[string]interface{})
	vote, _ := outer["vote"].(map[string]interface{})
	valStr, _ := vote["validator"].(string)
	roundRaw, _ := vote["round"].(json.Number)
	round, _ := roundRaw.Int64()

	if valStr != shortAddr {
		return
	}
	key := fmt.Sprintf("%s-%d", valStr, round)
	if _, ok := voteCache.Get(key); ok {
		return
	}
	voteCache.Add(key, struct{}{})
	LastVoteRound.WithLabelValues(valStr).Set(float64(round))
}

func handleBlock(val interface{}) {
	blockOuter, _ := val.(map[string]interface{})
	block, _ := blockOuter["Block"].(map[string]interface{})
	roundRaw, _ := block["round"].(json.Number)
	round, _ := roundRaw.Int64()

	if _, ok := roundCache.Get(round); ok {
		return
	}
	roundCache.Add(round, struct{}{})
	CurrentRound.Set(float64(round))
}
