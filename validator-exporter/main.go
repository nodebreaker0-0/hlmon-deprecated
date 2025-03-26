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

// heartbeatInfo stores the send timestamp and acked validators for a heartbeat.
type heartbeatInfo struct {
	sendTime time.Time
	acks     map[string]bool
}

var (
	// Vote, Block, Heartbeat 관련 메트릭
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
	// 메트릭 업데이트 타임스탬프
	lastVoteRoundUpdateTs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "validator_last_vote_round_update_ts",
		Help: "Timestamp when validator_last_vote_round was last updated",
	})
	currentRoundUpdateTs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "current_round_update_ts",
		Help: "Timestamp when current_round was last updated",
	})
	heartbeatAckUpdateTsVec = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "heartbeat_ack_delay_update_ts",
			Help: "Timestamp when heartbeat_ack_delay_ms was last updated for each validator",
		},
		[]string{"validator"},
	)
	// 최근 두 vote 메시지 간의 round 차이
	voteRoundDiff = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vote_round_diff",
		Help: "Difference between the two most recent vote round values for the validator",
	})
)

// 전역 변수: 최근 vote round 값을 추적 (초기값 0)
var (
	lastVoteRoundVal     int64 = 0
	previousVoteRoundVal int64 = 0
)

// heartbeatMap: key는 random_id 문자열, 값은 heartbeatInfo 구조체.
var (
	heartbeatMap      = make(map[string]*heartbeatInfo)
	heartbeatMapMutex sync.Mutex
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

	// Prometheus 메트릭 등록 (vote_round_diff 메트릭 포함)
	prometheus.MustRegister(lastVoteRound, currentRound, heartbeatAckDelayVec,
		lastVoteRoundUpdateTs, currentRoundUpdateTs, heartbeatAckUpdateTsVec, voteRoundDiff)

	// HTTP server를 통해 메트릭 노출 (포트 2112)
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(":2112", nil))
	}()

	// heartbeatMap 정리(cleanup) 루틴: 1초마다 5초 이상 지난 heartbeat 항목 삭제
	go func() {
		for {
			time.Sleep(1 * time.Second)
			cleanupHeartbeatMap(5 * time.Second)
		}
	}()

	// 최신 로그 파일 경로 계산 (예: consensusPath/YYYYMMDD/시간)
	now := time.Now()
	dateDir := now.Format("20060102")
	hourDir := fmt.Sprintf("%d", now.Hour())
	logFilePath := filepath.Join(*consensusPath, dateDir, hourDir)
	log.Printf("Tailing log file: %s", logFilePath)

	// 라인 처리를 위한 채널 및 다수의 워커 고루틴 실행
	lineCh := make(chan string, 10000)
	const numWorkers = 16
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

// cleanupHeartbeatMap removes heartbeat entries older than the given duration.
func cleanupHeartbeatMap(threshold time.Duration) {
	heartbeatMapMutex.Lock()
	defer heartbeatMapMutex.Unlock()
	now := time.Now()
	for key, info := range heartbeatMap {
		if now.Sub(info.sendTime) > threshold {
			log.Printf("[Cleanup] Removing heartbeat with random_id=%s, age=%v", key, now.Sub(info.sendTime))
			delete(heartbeatMap, key)
		}
	}
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
	//   "2025-03-26T11:13:15.591117076",
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

	// timestamp 파싱 (타임존 정보 없이 "2006-01-02T15:04:05.999999999" 포맷 사용)
	timestampStr, ok := logEntry[0].(string)
	if !ok {
		return
	}
	timestamp, err := time.Parse("2006-01-02T15:04:05.999999999", timestampStr)
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
						// 이전 vote round 값 업데이트
						previousVoteRoundVal = lastVoteRoundVal
						lastVoteRoundVal = int64(roundVal)
						lastVoteRound.Set(float64(lastVoteRoundVal))
						lastVoteRoundUpdateTs.Set(float64(time.Now().Unix()))
						if previousVoteRoundVal != 0 {
							diff := lastVoteRoundVal - previousVoteRoundVal
							voteRoundDiff.Set(float64(diff))
							log.Printf("[Vote] New vote round=%d, previous=%d, diff=%d", lastVoteRoundVal, previousVoteRoundVal, diff)
						} else {
							log.Printf("[Vote] First vote round=%d", lastVoteRoundVal)
						}
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
				currentRound.Set(float64(int64(roundVal)))
				currentRoundUpdateTs.Set(float64(time.Now().Unix()))
			}
		}
	}

	// --- Heartbeat 메시지 처리 (우리의 out 메시지) ---
	if heartbeatVal, exists := msgObj["Heartbeat"]; exists && direction == "out" {
		hbMap, ok := heartbeatVal.(map[string]interface{})
		if ok {
			validator, ok := hbMap["validator"].(string)
			if ok && validator == shortValidator {
				// random_id만 키로 사용
				randID, ok1 := hbMap["random_id"].(float64)
				if ok1 {
					key := fmt.Sprintf("%d", int64(randID))
					heartbeatMapMutex.Lock()
					heartbeatMap[key] = &heartbeatInfo{
						sendTime: timestamp,
						acks:     make(map[string]bool),
					}
					heartbeatMapMutex.Unlock()
					log.Printf("[Heartbeat Out] Registered heartbeat: random_id=%s, timestamp=%v", key, timestamp)
				} else {
					log.Printf("[Heartbeat Out] Missing random_id field")
				}
			}
		}
	}

	// --- HeartbeatAck 메시지 처리 ---
	// HeartbeatAck 메시지는 두 가지 형태로 올 수 있음:
	// 1. msgObj["HeartbeatAck"]
	// 2. msgObj["msg"] 안에 {"HeartbeatAck":{...}}
	var ackContent interface{}
	if val, exists := msgObj["HeartbeatAck"]; exists {
		ackContent = val
	} else if msgInner, exists := msgObj["msg"]; exists {
		if innerMap, ok := msgInner.(map[string]interface{}); ok {
			if val, exists := innerMap["HeartbeatAck"]; exists {
				ackContent = val
			}
		}
	}
	if ackContent != nil && direction == "in" {
		ackMap, ok := ackContent.(map[string]interface{})
		if !ok {
			log.Printf("[HeartbeatAck In] Ack content not a map")
			return
		}
		// random_id만 매칭 (round 무시)
		randID, ok1 := ackMap["random_id"].(float64)
		if ok1 {
			key := fmt.Sprintf("%d", int64(randID))
			heartbeatMapMutex.Lock()
			hbInfo, exists := heartbeatMap[key]
			if exists {
				src, okSrc := msgObj["source"].(string)
				if !okSrc {
					log.Printf("[HeartbeatAck In] Source field missing for ack: random_id=%s", key)
					heartbeatMapMutex.Unlock()
					return
				}
				// 중복 ack인지 체크 (같은 validator가 이미 응답했는지)
				if _, alreadyAcked := hbInfo.acks[src]; alreadyAcked {
					log.Printf("[HeartbeatAck In] Duplicate ack from validator=%s for random_id=%s", src, key)
				} else {
					delay := time.Since(hbInfo.sendTime)
					heartbeatAckDelayVec.WithLabelValues(src).Set(float64(delay.Milliseconds()))
					heartbeatAckUpdateTsVec.WithLabelValues(src).Set(float64(time.Now().Unix()))
					hbInfo.acks[src] = true
					log.Printf("[HeartbeatAck In] Matched heartbeat: random_id=%s, source=%s, delay=%v", key, src, delay)
				}
			} else {
				log.Printf("[HeartbeatAck In] No matching heartbeat found for random_id=%s", key)
			}
			heartbeatMapMutex.Unlock()
		} else {
			log.Printf("[HeartbeatAck In] Missing random_id field in ack")
		}
	}
}
