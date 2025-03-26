package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
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

// HeartbeatCache with TTL

type CachedHeartbeat struct {
	Value     float64
	Timestamp time.Time
}

type HeartbeatCache struct {
	sync.Mutex
	data map[string]CachedHeartbeat
	ttl  time.Duration
}

func NewHeartbeatCache(ttl time.Duration) *HeartbeatCache {
	return &HeartbeatCache{
		data: make(map[string]CachedHeartbeat),
		ttl:  ttl,
	}
}

func (c *HeartbeatCache) Set(key string, val float64) {
	c.Lock()
	defer c.Unlock()
	c.data[key] = CachedHeartbeat{val, time.Now()}
}

func (c *HeartbeatCache) Get(key string) (float64, bool) {
	c.Lock()
	defer c.Unlock()
	entry, exists := c.data[key]
	if !exists || time.Since(entry.Timestamp) > c.ttl {
		delete(c.data, key)
		return 0, false
	}
	return entry.Value, true
}

var (
	consensusPath    = flag.String("consensus-path", "", "Path to consensus logs")
	statusPath       = flag.String("status-path", "", "Path to status logs")
	validatorAddress = flag.String("validator-address", "", "Validator address")
	logLevel         = flag.String("log-level", "info", "Log level")

	LastVoteRound         = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_vote_last_round", Help: "Last round this validator voted in"}, []string{"validator"})
	CurrentRound          = prometheus.NewGauge(prometheus.GaugeOpts{Name: "current_round", Help: "Most recent consensus round from blocks"})
	DisconnectedValidator = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_disconnected", Help: "1 if disconnected"}, []string{"source", "target", "last_round"})
	AckDelaySeconds       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_heartbeat_ack_delay_seconds", Help: "Heartbeat ack delay (sec)"}, []string{"validator"})
)

var (
	heartbeatCache *HeartbeatCache
	currentRoundMu sync.Mutex
	shortAddress   string
)

func logDebug(format string, args ...interface{}) {
	if *logLevel == "debug" {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	if *logLevel == "debug" || *logLevel == "info" {
		log.Printf("[INFO] "+format, args...)
	}
}

func logWarn(format string, args ...interface{}) {
	log.Printf("[WARN] "+format, args...)
}

func shortenAddress(addr string) string {
	if len(addr) < 10 {
		return addr
	}
	return addr[:8] + "..." + addr[len(addr)-4:]
}

func getLatestHourlyFile(basePath string) string {
	today := time.Now().Format("20060102")
	fullDir := filepath.Join(basePath, today)
	entries, err := os.ReadDir(fullDir)
	if err != nil {
		logWarn("Failed to read dir: %v", err)
		return ""
	}
	var nums []int
	for _, e := range entries {
		if i, err := strconv.Atoi(e.Name()); err == nil {
			nums = append(nums, i)
		}
	}
	if len(nums) == 0 {
		return ""
	}
	sort.Sort(sort.Reverse(sort.IntSlice(nums)))
	return filepath.Join(fullDir, fmt.Sprintf("%d", nums[0]))
}

func TailLogFile(path string, callback func(string)) {
	file, err := os.Open(path)
	if err != nil {
		logWarn("Tail open error: %v", err)
		return
	}
	defer file.Close()
	file.Seek(0, io.SeekEnd)
	reader := bufio.NewReader(file)
	logInfo("Tailing file: %s", path)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		callback(line)
	}
}

func HandleConsensusLine(line string) {
	logDebug("Raw line: %s", line)
	decoder := json.NewDecoder(strings.NewReader(strings.TrimSpace(line)))
	decoder.UseNumber()

	var entry []interface{}
	if err := decoder.Decode(&entry); err != nil || len(entry) < 2 {
		logWarn("Invalid consensus line: %v | Error: %v", line, err)
		return
	}
	inner, ok := entry[1].([]interface{})
	if !ok || len(inner) != 2 {
		logWarn("Unexpected consensus format: %v", entry)
		return
	}
	direction, _ := inner[0].(string)
	content, ok := inner[1].(map[string]interface{})
	if !ok {
		logWarn("Invalid content structure: %v", inner[1])
		return
	}
	if rawMsg, exists := content["msg"]; exists {
		if nested, ok := rawMsg.(map[string]interface{}); ok {
			content = nested
		}
	}
	now := float64(time.Now().Unix())
	for key, value := range content {
		switch key {
		case "Heartbeat":
			if direction == "out" {
				if hb, ok := value.(map[string]interface{}); ok {
					val, _ := hb["validator"].(string)
					ridRaw, ok := hb["random_id"]
					if strings.HasPrefix(val, shortAddress[:6]) && strings.HasSuffix(val, shortAddress[len(shortAddress)-4:]) && ok {
						if rid, ok := ridRaw.(json.Number); ok {
							heartbeatCache.Set(rid.String(), now)
						}
					}
				}
			}
		case "HeartbeatAck":
			if direction == "in" {
				if ack, ok := value.(map[string]interface{}); ok {
					validator, _ := ack["validator"].(string)
					ridRaw, ok := ack["random_id"]
					if ok {
						if rid, ok := ridRaw.(json.Number); ok {
							if sent, ok := heartbeatCache.Get(rid.String()); ok {
								delay := now - sent
								AckDelaySeconds.WithLabelValues(validator).Set(delay)
							} else {
								logDebug("Ack received but no matching heartbeat for rid=%s", rid.String())
							}
						}
					}
				}
			}
		case "Vote":
			if direction == "out" {
				vc, ok := value.(map[string]interface{})
				if !ok {
					logWarn("Heartbeat container type assertion failed: %v", value)
					return
				}
				if vote, ok := vc["vote"].(map[string]interface{}); ok {
					validator, _ := vote["validator"].(string)
					if validator == shortAddress {
						if round, ok := vote["round"].(json.Number); ok {
							if r, err := round.Int64(); err == nil {
								LastVoteRound.WithLabelValues(validator).Set(float64(r))
							}
						}
					}
				}
			}
		case "Block":
			if bc, ok := value.(map[string]interface{}); ok {
				if bd, ok := bc["Block"].(map[string]interface{}); ok {
					if round, ok := bd["round"].(json.Number); ok {
						if r, err := round.Int64(); err == nil {
							currentRoundMu.Lock()
							CurrentRound.Set(float64(r))
							currentRoundMu.Unlock()
						}
					}
				}
			}
		}
	}
}

func main() {
	flag.Parse()
	if *validatorAddress == "" || *consensusPath == "" || *statusPath == "" {
		log.Fatal("All --validator-address, --consensus-path, and --status-path are required")
	}

	heartbeatCache = NewHeartbeatCache(10 * time.Second)
	shortAddress = shortenAddress(*validatorAddress)
	logInfo("Using shortened validator address: %s", shortAddress)

	prometheus.MustRegister(LastVoteRound)
	prometheus.MustRegister(CurrentRound)
	prometheus.MustRegister(DisconnectedValidator)
	prometheus.MustRegister(AckDelaySeconds)

	go func() {
		for {
			path := getLatestHourlyFile(*consensusPath)
			if path != "" {
				TailLogFile(path, HandleConsensusLine)
			} else {
				logWarn("No consensus file found at %s", *consensusPath)
			}
			time.Sleep(10 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	logInfo("🚀 Starting Validator Exporter on :9101")
	http.ListenAndServe(":9101", nil)
}
