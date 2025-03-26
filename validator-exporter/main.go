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
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	consensusPath    = flag.String("consensus-path", "", "Base path for consensus log files (required)")
	statusPath       = flag.String("status-path", "", "Base path for status log files (required)")
	validatorAddress = flag.String("validator-address", "", "Your validator address (required)")
	logLevel         = flag.String("log-level", "info", "Log level: debug, info, warn")

	LastVoteRound         = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_vote_last_round", Help: "Last round number this validator voted in"}, []string{"validator"})
	CurrentRound          = prometheus.NewGauge(prometheus.GaugeOpts{Name: "current_round", Help: "Most recent consensus round observed from block messages"})
	DisconnectedValidator = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_disconnected", Help: "Whether a validator is disconnected from peer (1=disconnected)"}, []string{"source", "target", "last_round"})
	AckDelaySeconds       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_heartbeat_ack_delay_seconds", Help: "Time in seconds between sending a heartbeat and receiving ack, per validator"}, []string{"validator"})
)

var (
	heartbeatSent  = make(map[string]float64)
	heartbeatMu    sync.Mutex
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

func storeHeartbeat(rid string, timestamp float64) {
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	heartbeatSent[rid] = timestamp
}

func loadAndDeleteHeartbeat(rid string) (float64, bool) {
	heartbeatMu.Lock()
	defer heartbeatMu.Unlock()
	time, ok := heartbeatSent[rid]
	if ok {
		delete(heartbeatSent, rid)
	}
	return time, ok
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
		logWarn("Invalid consensus content structure: %v", inner[1])
		return
	}
	if rawMsg, exists := content["msg"]; exists {
		if nestedMsg, ok := rawMsg.(map[string]interface{}); ok {
			content = nestedMsg
		}
	}

	now := float64(time.Now().Unix())
	logDebug("Direction: %s | Keys: %v", direction, reflect.ValueOf(content).MapKeys())

	for key, value := range content {
		switch key {
		case "Heartbeat":
			if direction == "out" {
				hb, ok := value.(map[string]interface{})
				if ok {
					val, _ := hb["validator"].(string)
					ridRaw := hb["random_id"]
					if strings.HasPrefix(val, shortAddress[:6]) && strings.HasSuffix(val, shortAddress[len(shortAddress)-4:]) {
						if ridNum, ok := ridRaw.(json.Number); ok {
							rid := ridNum.String()
							storeHeartbeat(rid, now)
							logDebug("Stored heartbeat rid=%s", rid)
						}
					}
				}
			}
		case "HeartbeatAck":
			if direction == "in" {
				ack, ok := value.(map[string]interface{})
				if ok {
					ridRaw := ack["random_id"]
					validator, _ := ack["validator"].(string)
					if ridNum, ok := ridRaw.(json.Number); ok {
						rid := ridNum.String()
						if sent, ok := loadAndDeleteHeartbeat(rid); ok {
							delay := now - sent
							AckDelaySeconds.WithLabelValues(validator).Set(delay)
							logDebug("Ack received for rid=%s, delay=%.2fs", rid, delay)
						} else {
							logDebug("Ack received but no matching heartbeat for rid=%s", rid)
						}
					}
				}
			}
		case "Vote":
			if direction == "out" {
				vote := value.(map[string]interface{})["vote"].(map[string]interface{})
				validator := vote["validator"].(string)
				if validator == shortAddress {
					if round, ok := vote["round"].(json.Number); ok {
						if r, err := round.Int64(); err == nil {
							LastVoteRound.WithLabelValues(validator).Set(float64(r))
						}
					}
				}
			}
		case "Block":
			block := value.(map[string]interface{})["Block"].(map[string]interface{})
			if round, ok := block["round"].(json.Number); ok {
				if r, err := round.Int64(); err == nil {
					currentRoundMu.Lock()
					CurrentRound.Set(float64(r))
					currentRoundMu.Unlock()
				}
			}
		}
	}
}

func main() {
	flag.Parse()
	if *validatorAddress == "" || *consensusPath == "" || *statusPath == "" {
		log.Fatal("All flags are required")
	}
	shortAddress = shortenAddress(*validatorAddress)
	logInfo("Using short address: %s", shortAddress)

	prometheus.MustRegister(LastVoteRound)
	prometheus.MustRegister(CurrentRound)
	prometheus.MustRegister(DisconnectedValidator)
	prometheus.MustRegister(AckDelaySeconds)

	go func() {
		for {
			path := getLatestHourlyFile(*consensusPath)
			if path != "" {
				TailLogFile(path, HandleConsensusLine)
			}
			time.Sleep(10 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	logInfo("🚀 Starting on :9101")
	http.ListenAndServe(":9101", nil)
}
