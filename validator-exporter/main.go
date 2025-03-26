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

var (
	consensusPath    = flag.String("consensus-path", "", "Base path for consensus log files (required)")
	statusPath       = flag.String("status-path", "", "Base path for status log files (required)")
	validatorAddress = flag.String("validator-address", "", "Your validator address (required)")

	LastVoteRound = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "validator_vote_last_round",
			Help: "Last round number this validator voted in",
		},
		[]string{"validator"},
	)

	CurrentRound = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "current_round",
			Help: "Most recent consensus round observed from block messages",
		},
	)

	DisconnectedValidator = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "validator_disconnected",
			Help: "Whether a validator is disconnected from peer (1=disconnected)",
		},
		[]string{"source", "target", "last_round"},
	)

	AckDelaySeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "validator_heartbeat_ack_delay_seconds",
			Help: "Time in seconds between sending a heartbeat and receiving ack, per validator",
		},
		[]string{"validator"},
	)
)

var heartbeatSent sync.Map
var delayedSince sync.Map
var currentRoundMu sync.Mutex
var shortAddress string

func init() {
	prometheus.MustRegister(LastVoteRound)
	prometheus.MustRegister(CurrentRound)
	prometheus.MustRegister(DisconnectedValidator)
	prometheus.MustRegister(AckDelaySeconds)
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
		log.Printf("Failed to read dir: %v", err)
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
		log.Printf("Tail open error: %v", err)
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	file.Seek(0, io.SeekEnd)
	log.Printf("Tailing file: %s", path)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			time.Sleep(100 * time.Millisecond) // wait for more data
			continue
		}
		callback(line)
	}
}

func HandleConsensusLine(line string) {
	var entry []interface{}
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &entry); err != nil || len(entry) < 2 {
		log.Printf("Invalid consensus line: %v", err)
		return
	}
	inner, ok := entry[1].([]interface{})
	if !ok || len(inner) != 2 {
		log.Printf("Unexpected consensus format: %v", entry)
		return
	}
	direction, _ := inner[0].(string)
	content, ok := inner[1].(map[string]interface{})
	if !ok {
		log.Printf("Invalid consensus content: %v", inner[1])
		return
	}
	now := float64(time.Now().Unix())

	for key, value := range content {
		switch key {
		case "Heartbeat":
			if direction == "out" {
				hb, ok := value.(map[string]interface{})["Heartbeat"].(map[string]interface{})
				if ok && hb["validator"].(string) == shortAddress {
					rid := fmt.Sprintf("%.0f", hb["random_id"].(float64))
					heartbeatSent.Store(rid, now)
				}
			}
		case "HeartbeatAck":
			hb, ok := value.(map[string]interface{})["heartbeat_ack"].(map[string]interface{})
			if !ok {
				log.Printf("Invalid HeartbeatAck format: %v", value)
				continue
			}
			validator := hb["validator"].(string)
			rid := fmt.Sprintf("%.0f", hb["random_id"].(float64))
			if direction == "in" {
				if sent, ok := heartbeatSent.Load(rid); ok {
					delay := now - sent.(float64)
					AckDelaySeconds.WithLabelValues(validator).Set(delay)
					if delay > 0.2 {
						if _, exists := delayedSince.Load(validator); !exists {
							delayedSince.Store(validator, now)
						}
					} else {
						delayedSince.Delete(validator)
					}
					heartbeatSent.Delete(rid)
				}
			}
		case "Vote":
			if direction == "out" {
				vote, ok := value.(map[string]interface{})["vote"].(map[string]interface{})
				if !ok {
					log.Printf("Invalid vote format: %v", value)
					continue
				}
				validator := vote["validator"].(string)
				if validator == shortAddress {
					round := vote["round"].(float64)
					LastVoteRound.WithLabelValues(validator).Set(round)
				}
			}
		case "Block":
			msg, ok := value.(map[string]interface{})["Block"].(map[string]interface{})
			if ok {
				if r, ok := msg["round"].(float64); ok {
					currentRoundMu.Lock()
					CurrentRound.Set(r)
					currentRoundMu.Unlock()
				}
			}
		}
	}
}

func ScanStatusFile(path string) {
	file, err := os.Open(path)
	if err != nil {
		log.Printf("Status file open error: %v", err)
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
	log.Println("🚀 Starting Validator Exporter on :9101")

	if *validatorAddress == "" {
		log.Fatal("--validator-address is required")
	}
	if *consensusPath == "" {
		log.Fatal("--consensus-path is required")
	}
	if *statusPath == "" {
		log.Fatal("--status-path is required")
	}

	shortAddress = shortenAddress(*validatorAddress)

	go func() {
		for {
			path := getLatestHourlyFile(*consensusPath)
			if path != "" {
				TailLogFile(path, HandleConsensusLine)
			}
			time.Sleep(10 * time.Second)
		}
	}()

	go func() {
		for {
			path := getLatestHourlyFile(*statusPath)
			if path != "" {
				ScanStatusFile(path)
			}
			time.Sleep(60 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())
	http.ListenAndServe(":9101", nil)
}
