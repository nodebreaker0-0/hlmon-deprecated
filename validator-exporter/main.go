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
	consensusPath         = flag.String("consensus-path", "", "Base path for consensus log files (required)")
	statusPath            = flag.String("status-path", "", "Base path for status log files (required)")
	validatorAddress      = flag.String("validator-address", "", "Your validator address (required)")
	logLevel              = flag.String("log-level", "info", "Log level: debug, info, warn")
	LastVoteRound         = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_vote_last_round", Help: "Last round number this validator voted in"}, []string{"validator"})
	CurrentRound          = prometheus.NewGauge(prometheus.GaugeOpts{Name: "current_round", Help: "Most recent consensus round observed from block messages"})
	DisconnectedValidator = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_disconnected", Help: "Whether a validator is disconnected from peer (1=disconnected)"}, []string{"source", "target", "last_round"})
	AckDelaySeconds       = prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "validator_heartbeat_ack_delay_seconds", Help: "Time in seconds between sending a heartbeat and receiving ack, per validator"}, []string{"validator"})
)

var (
	heartbeatSent  sync.Map
	delayedSince   sync.Map
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
			logDebug("Found Heartbeat, direction=%s", direction)
			if direction == "out" {
				hb, ok := value.(map[string]interface{})["Heartbeat"].(map[string]interface{})
				if ok && hb["validator"].(string) == shortAddress {
					rid := hb["random_id"].(json.Number).String()
					heartbeatSent.Store(rid, now)
				}
			}

		case "HeartbeatAck":
			logDebug("Found HeartbeatAck, direction=%s", direction)
			if direction == "in" {
				ackWrap, ok := value.(map[string]interface{})
				if !ok {
					logWarn("Invalid ack wrapper: %v", value)
					continue
				}
				ack, ok := ackWrap["HeartbeatAck"].(map[string]interface{})
				if !ok {
					logWarn("Missing HeartbeatAck field in in-msg: %v", ackWrap)
					continue
				}
				rid := ack["random_id"].(json.Number).String()
				validator := ack["validator"].(string)
				if sent, ok := heartbeatSent.Load(rid); ok {
					delay := now - sent.(float64)
					AckDelaySeconds.WithLabelValues(validator).Set(delay)
					heartbeatSent.Delete(rid)
				}
			}

		case "Vote":
			logDebug("Found Vote, direction=%s", direction)
			if direction == "out" {
				vote, ok := value.(map[string]interface{})["vote"].(map[string]interface{})
				if !ok {
					logWarn("Invalid vote format: %v", value)
					continue
				}
				validator := vote["validator"].(string)
				if validator == shortAddress {
					round := vote["round"].(json.Number)
					r, _ := round.Int64()
					LastVoteRound.WithLabelValues(validator).Set(float64(r))
				}
			}
		case "Block":
			logDebug("Found Block")
			blockContainer, ok := value.(map[string]interface{})
			if !ok {
				logWarn("Block container invalid: %v", value)
				break
			}
			blockData, ok := blockContainer["Block"].(map[string]interface{})
			if !ok {
				logWarn("Missing Block field: %v", blockContainer)
				break
			}
			round, ok := blockData["round"].(json.Number)
			if !ok {
				logWarn("Block round missing or not number: %v", blockData)
				break
			}
			r, _ := round.Int64()
			currentRoundMu.Lock()
			CurrentRound.Set(float64(r))
			currentRoundMu.Unlock()
		default:
			logDebug("Unknown key in consensus content: %s", key)
		}
	}
}

func ScanStatusFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		logWarn("Status file open error: %v", err)
		return
	}
	defer file.Close()

	var lastLine string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lastLine = scanner.Text()
	}
	if lastLine != "" {
		HandleStatusLine(lastLine)
	}
}

func HandleStatusLine(line string) {
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

func main() {
	flag.Parse()

	if *validatorAddress == "" || *consensusPath == "" || *statusPath == "" {
		log.Fatal("All flags --validator-address, --consensus-path, and --status-path are required")
	}

	shortAddress = shortenAddress(*validatorAddress)
	logInfo("Using shortened validator address: %s", shortAddress)

	prometheus.MustRegister(LastVoteRound)
	prometheus.MustRegister(CurrentRound)
	prometheus.MustRegister(DisconnectedValidator)
	prometheus.MustRegister(AckDelaySeconds)
	logInfo("✅ Prometheus metrics registered")

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

	go func() {
		for {
			path := getLatestHourlyFile(*statusPath)
			if path != "" {
				ScanStatusFile(path)
			} else {
				logWarn("No status file found at %s", *statusPath)
			}
			time.Sleep(60 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	logInfo("🚀 Starting Validator Exporter on :9101")
	http.ListenAndServe(":9101", nil)
}
