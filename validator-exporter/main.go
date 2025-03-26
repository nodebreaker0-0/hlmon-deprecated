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

	lru "github.com/hashicorp/golang-lru/simplelru"
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
	heartbeatCache *lru.LRU
	voteMu         sync.RWMutex
	roundMu        sync.RWMutex
	lastVoteRound  float64
	lastBlockRound float64
	shortAddr      string
)

func main() {
	flag.Parse()
	if *validatorAddr == "" || *consensusLogPath == "" || *statusLogPath == "" {
		log.Fatal("Missing required flags")
	}

	shortAddr = shortenAddress(*validatorAddr)
	log.Printf("[INFO] validator: %s", shortAddr)

	cache, _ := lru.NewLRU(5000, nil)
	heartbeatCache = cache

	prometheus.MustRegister(LastVoteRound)
	prometheus.MustRegister(CurrentRound)
	prometheus.MustRegister(AckDelaySeconds)
	prometheus.MustRegister(DisconnectedValidator)

	go tailLoop(*consensusLogPath)
	go statusLoop(*statusLogPath)

	http.Handle("/metrics", promhttp.Handler())
	log.Fatal(http.ListenAndServe(":9101", nil))
}

func shortenAddress(addr string) string {
	if len(addr) < 12 {
		return addr
	}
	return addr[:8] + "..." + addr[len(addr)-4:]
}

func tailLoop(base string) {
	for {
		path := latestLogFile(base)
		if path != "" {
			tailLog(path)
		}
		time.Sleep(10 * time.Second)
	}
}

func statusLoop(base string) {
	for {
		path := latestLogFile(base)
		if path != "" {
			processStatus(path)
		}
		time.Sleep(30 * time.Second)
	}
}

func latestLogFile(base string) string {
	today := time.Now().Format("20060102")
	dir := filepath.Join(base, today)
	dirs, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	var hours []int
	for _, d := range dirs {
		if h, err := strconv.Atoi(d.Name()); err == nil {
			hours = append(hours, h)
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
				hb := val.(map[string]interface{})
				addr, _ := hb["validator"].(string)
				id, _ := hb["random_id"].(json.Number)
				if strings.HasPrefix(addr, shortAddr[:6]) {
					heartbeatCache.Add(id.String(), time.Now().UnixNano())
				}
			}
		case "HeartbeatAck":
			if dir == "in" {
				ack := val.(map[string]interface{})
				id, _ := ack["random_id"].(json.Number)
				addr, _ := ack["validator"].(string)
				if tsRaw, ok := heartbeatCache.Get(id.String()); ok {
					delay := float64(time.Now().UnixNano()-tsRaw.(int64)) / 1e6
					AckDelaySeconds.WithLabelValues(addr).Set(delay)
					heartbeatCache.Remove(id.String())
				}
			}
		case "Vote":
			if dir == "out" {
				voteOuter := val.(map[string]interface{})
				vote := voteOuter["vote"].(map[string]interface{})
				addr, _ := vote["validator"].(string)
				if addr == shortAddr {
					roundRaw, _ := vote["round"].(json.Number)
					round, _ := roundRaw.Int64()
					voteMu.RLock()
					dup := lastVoteRound == float64(round)
					voteMu.RUnlock()
					if !dup {
						voteMu.Lock()
						LastVoteRound.WithLabelValues(addr).Set(float64(round))
						lastVoteRound = float64(round)
						voteMu.Unlock()
					}
				}
			}
		case "Block":
			if dir == "in" {
				blockOuter := val.(map[string]interface{})
				block := blockOuter["Block"].(map[string]interface{})
				roundRaw, _ := block["round"].(json.Number)
				round, _ := roundRaw.Int64()
				roundMu.RLock()
				dup := lastBlockRound == float64(round)
				roundMu.RUnlock()
				if !dup {
					roundMu.Lock()
					CurrentRound.Set(float64(round))
					lastBlockRound = float64(round)
					roundMu.Unlock()
				}
			}
		}
	}
}

func processStatus(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	var lastLine string
	for scanner.Scan() {
		lastLine = scanner.Text()
	}
	if lastLine == "" {
		return
	}
	var entry []interface{}
	if err := json.Unmarshal([]byte(lastLine), &entry); err != nil || len(entry) < 2 {
		return
	}
	payload, ok := entry[1].(map[string]interface{})
	if !ok {
		return
	}
	if raw, ok := payload["disconnected_validators"]; ok {
		arr := raw.([]interface{})
		for _, v := range arr {
			pair := v.([]interface{})
			target := pair[0].(string)
			sources := pair[1].([]interface{})
			for _, s := range sources {
				slice := s.([]interface{})
				source := slice[0].(string)
				last := int(slice[1].(float64))
				DisconnectedValidator.WithLabelValues(source, target, fmt.Sprintf("%d", last)).Set(1)
			}
		}
	}
}
