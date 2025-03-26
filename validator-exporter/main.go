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
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Prometheus metrics
var (
	lastVoteRound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "validator_last_vote_round",
		Help: "Last vote round number for the validator",
	})
	currentRound = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_round",
		Help: "Latest current round from block messages",
	})
	heartbeatAckDelayVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "heartbeat_ack_delay_ms",
			Help: "Latest delay (ms) for heartbeat ack from each validator",
		},
		[]string{"validator"},
	)
)

// global round values protected by mutex
var (
	roundsMu         sync.Mutex
	lastVoteRoundVal int64 = 0
	currentRoundVal  int64 = 0
)

// heartbeat tracking (for outgoing heartbeat messages)
var (
	heartbeatMap      = make(map[string]time.Time)
	heartbeatMapMutex sync.Mutex
)

// lastGoodHeartbeatTime is updated when a heartbeat ack is received within 200ms.
var (
	lastGoodHeartbeatTime  time.Time
	lastGoodHeartbeatMutex sync.Mutex
)

func main() {
	// 플래그 파싱
	validatorAddrFull := flag.String("validator-address", "", "Full validator address")
	consensusPath := flag.String("consensus-path", "", "Path to consensus logs (hourly directory)")
	flag.Parse()

	if *validatorAddrFull == "" || *consensusPath == "" {
		log.Fatal("Both --validator-address and --consensus-path are required")
	}

	// 단축형 주소 생성 (예: "0xef22..d5ac")
	shortValidator := shortenAddress(*validatorAddrFull)

	// Prometheus 메트릭 등록
	prometheus.MustRegister(lastVoteRound, currentRound, heartbeatAckDelayVec)

	// HTTP server를 통해 메트릭 노출 (포트 2112)
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(":2112", nil))
	}()

	// 초기 "좋은" heartbeat ack 타임은 현재로 설정
	lastGoodHeartbeatMutex.Lock()
	lastGoodHeartbeatTime = time.Now()
	lastGoodHeartbeatMutex.Unlock()

	// 최신 로그 파일 경로 계산 (예: consensusPath/YYYYMMDD/시간)
	now := time.Now()
	dateDir := now.Format("20060102")
	hourDir := fmt.Sprintf("%d", now.Hour())
	logFilePath := filepath.Join(*consensusPath, dateDir, hourDir)
	log.Printf("Tailing log file: %s", logFilePath)

	// 라인 처리를 위한 채널 및 다수의 워커 고루틴 실행
	lineCh := make(chan string, 1000)
	const numWorkers = 4
	for i := 0; i < numWorkers; i++ {
		go func() {
			for line := range lineCh {
				processLogLine(line, shortValidator)
			}
		}()
	}

	// 파일 tailing 고루틴 (로그가 빠르게 추가됨)
	go tailFile(logFilePath, lineCh)

	// 메인 루프는 블로킹
	select {}
}

// shortenAddress returns a shortened version (first 6 and last 4 chars) of the validator address.
func shortenAddress(addr string) string {
	if len(addr) < 10 {
		return addr
	}
	return addr[:6] + ".." + addr[len(addr)-4:]
}

// tailFile opens the log file and continuously reads new lines, sending each line to lineCh.
func tailFile(filePath string, lineCh chan<- string) {
	file, err := os.Open(filePath)
	if err != nil {
		log.Printf("Error opening file %s: %v", filePath, err)
		return
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(100 * time.Millisecond)
				continue
			} else {
				log.Printf("Error reading file: %v", err)
				break
			}
		}
		// trim 후 채널로 전송
		lineCh <- line
	}
}

// processLogLine parses a single JSON log line and updates Prometheus metrics accordingly.
func processLogLine(line string, shortValidator string) {
	// 로그 라인은 JSON 배열로 되어 있음:
	// [
	//   "2025-03-26T08:29:04.657435334",
	//   ["in" 또는 "out", { ... }]
	// ]
	var logEntry []interface{}
	if err := json.Unmarshal([]byte(line), &logEntry); err != nil {
		log.Printf("Error unmarshaling line: %v", err)
		return
	}
	if len(logEntry) < 2 {
		return
	}
	timestampStr, ok := logEntry[0].(string)
	if !ok {
		return
	}
	timestamp, err := time.Parse(time.RFC3339Nano, timestampStr)
	if err != nil {
		log.Printf("Error parsing timestamp: %v", err)
		return
	}

	// 두번째 요소는 [direction, message object]
	details, ok := logEntry[1].([]interface{})
	if !ok || len(details) < 2 {
		return
	}
	direction, ok := details[0].(string)
	if !ok {
		return
	}
	msgObj, ok := details[1].(map[string]interface{})
	if !ok {
		return
	}

	// --- Vote 메시지 처리 ---
	if voteVal, exists := msgObj["Vote"]; exists {
		voteMap, ok := voteVal.(map[string]interface{})
		if ok {
			voteData, ok := voteMap["vote"].(map[string]interface{})
			if ok {
				validator, ok := voteData["validator"].(string)
				if ok && validator == shortValidator {
					if roundVal, ok := voteData["round"].(float64); ok {
						lastVoteRound.Set(roundVal)
						roundsMu.Lock()
						lastVoteRoundVal = int64(roundVal)
						roundsMu.Unlock()
					}
				}
			}
		}
	}

	// --- Block 메시지 처리 (현재 round 업데이트) ---
	if blockVal, exists := msgObj["Block"]; exists {
		blockMap, ok := blockVal.(map[string]interface{})
		if ok {
			if roundVal, ok := blockMap["round"].(float64); ok {
				currentRound.Set(roundVal)
				roundsMu.Lock()
				currentRoundVal = int64(roundVal)
				roundsMu.Unlock()
			}
		}
	}

	// --- Heartbeat 메시지 처리 (우리의 out 메시지) ---
	if heartbeatVal, exists := msgObj["Heartbeat"]; exists && direction == "out" {
		hbMap, ok := heartbeatVal.(map[string]interface{})
		if ok {
			validator, ok := hbMap["validator"].(string)
			if ok && validator == shortValidator {
				randID, ok1 := hbMap["random_id"].(float64)
				roundVal, ok2 := hbMap["round"].(float64)
				if ok1 && ok2 {
					key := fmt.Sprintf("%d-%d", int64(randID), int64(roundVal))
					heartbeatMapMutex.Lock()
					heartbeatMap[key] = timestamp
					heartbeatMapMutex.Unlock()
				}
			}
		}
	}

	// --- HeartbeatAck 메시지 처리 ---
	// 여러 벨리데이터가 ack를 보내므로, 메트릭은 ack를 보낸 validator별로 딜레이를 기록한다.
	if ackVal, exists := msgObj["HeartbeatAck"]; exists && direction == "in" {
		ackMap, ok := ackVal.(map[string]interface{})
		if ok {
			randID, ok1 := ackMap["random_id"].(float64)
			roundVal, ok2 := ackMap["round"].(float64)
			if ok1 && ok2 {
				key := fmt.Sprintf("%d-%d", int64(randID), int64(roundVal))
				heartbeatMapMutex.Lock()
				sendTime, exists := heartbeatMap[key]
				if exists {
					delay := timestamp.Sub(sendTime)
					// ack를 보낸 벨리데이터는 로그의 "source" 필드에서 확인
					src, okSrc := msgObj["source"].(string)
					if okSrc {
						heartbeatAckDelayVec.WithLabelValues(src).Set(float64(delay.Milliseconds()))
					}
					delete(heartbeatMap, key)
					// 200ms 이하인 경우 "좋은" heartbeat ack로 갱신
					if delay <= 200*time.Millisecond {
						lastGoodHeartbeatMutex.Lock()
						lastGoodHeartbeatTime = timestamp
						lastGoodHeartbeatMutex.Unlock()
					}
				}
				heartbeatMapMutex.Unlock()
			}
		}
	}
}
