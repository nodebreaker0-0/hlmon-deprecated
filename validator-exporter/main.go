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

// Constants for log levels.
const (
	LogLevelError = 1
	LogLevelWarn  = 2
	LogLevelInfo  = 3
	LogLevelDebug = 4
)

// Global log level variable.
var logLevel int

// Helper log functions.
func logDebug(format string, args ...interface{}) {
	if logLevel >= LogLevelDebug {
		log.Printf("[DEBUG] "+format, args...)
	}
}

func logInfo(format string, args ...interface{}) {
	if logLevel >= LogLevelInfo {
		log.Printf("[INFO] "+format, args...)
	}
}

func logWarn(format string, args ...interface{}) {
	if logLevel >= LogLevelWarn {
		log.Printf("[WARN] "+format, args...)
	}
}

func logError(format string, args ...interface{}) {
	if logLevel >= LogLevelError {
		log.Printf("[ERROR] "+format, args...)
	}
}

// heartbeatInfo stores the send timestamp and acked validators for a heartbeat.
type heartbeatInfo struct {
	sendTime time.Time
	acks     map[string]bool
}

var (
	// 기존 메트릭들
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

	// 새롭게 추가할 vote 시간 차이 메트릭 (초 단위)
	voteTimeDiff = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vote_time_diff_seconds",
		Help: "Time difference in seconds between the timestamp of the last vote and the current time",
	})

	// heartbeatMap: key는 random_id 문자열, 값은 heartbeatInfo 구조체.
	heartbeatMap      = make(map[string]*heartbeatInfo)
	heartbeatMapMutex sync.Mutex
)

// 마지막 vote 메시지의 타임스탬프를 저장하는 전역 변수
var lastVoteTime time.Time

func main() {
	// 플래그 파싱
	validatorAddrFull := flag.String("validator-address", "", "Full validator address")
	consensusPath := flag.String("consensus-path", "", "Path to consensus logs (hourly directory)")
	logLevelFlag := flag.Int("log-level", LogLevelInfo, "Log level (1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.Parse()

	// 로그 레벨 설정
	logLevel = *logLevelFlag

	if *validatorAddrFull == "" || *consensusPath == "" {
		log.Fatal("Both --validator-address and --consensus-path are required")
	}

	// 단축형 주소 생성 (예: "0xef22..d5ac")
	shortValidator := shortenAddress(*validatorAddrFull)

	// Prometheus 메트릭 등록 (새 메트릭 포함)
	prometheus.MustRegister(lastVoteRound, currentRound, heartbeatAckDelayVec,
		lastVoteRoundUpdateTs, currentRoundUpdateTs, heartbeatAckUpdateTsVec, voteTimeDiff)

	// vote_time_diff_seconds 갱신 고루틴
	go func() {
		for {
			time.Sleep(1 * time.Second)
			if !lastVoteTime.IsZero() {
				diff := time.Since(lastVoteTime).Seconds()
				voteTimeDiff.Set(diff)
			}
		}
	}()

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

	// 파일 tailing 고루틴: 로그파일은 매시간 새로 생성되므로, 새 파일을 자동 감지하도록 tailLogs 함수를 사용.
	go tailLogs(*consensusPath, lineCh)

	// 메인 루프는 블로킹
	select {}
}

// tailLogs continuously tails log files which are rotated hourly.
func tailLogs(basePath string, lineCh chan<- string) {
	for {
		// 현재 시간을 기준으로 로그 파일 경로 계산 (예: consensusPath/YYYYMMDD/HOUR)
		now := time.Now()
		dateDir := now.Format("20060102")
		hourDir := fmt.Sprintf("%d", now.Hour())
		filePath := filepath.Join(basePath, dateDir, hourDir)
		logInfo("Tailing log file: %s", filePath)

		file, err := os.Open(filePath)
		if err != nil {
			logError("Error opening file %s: %v", filePath, err)
			time.Sleep(10 * time.Second)
			continue
		}

		reader := bufio.NewReader(file)
		lastReadTime := time.Now()

		// 파일의 끝(EOF) 상태가 지속되면 새 파일로 전환
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					time.Sleep(100 * time.Millisecond)
					// 만약 마지막 읽은 이후 1분 이상 로그 업데이트가 없으면 파일 종료로 판단
					if time.Since(lastReadTime) > 1*time.Minute {
						logInfo("No new logs for 1 minute, switching to new file.")
						break
					}
					continue
				} else {
					logError("Error reading file %s: %v", filePath, err)
					break
				}
			}
			lastReadTime = time.Now()
			lineCh <- line
		}
		file.Close()
		// 새 파일로 전환 전에 잠시 대기
		time.Sleep(500 * time.Millisecond)
	}
}

// cleanupHeartbeatMap removes heartbeat entries older than the given duration.
func cleanupHeartbeatMap(threshold time.Duration) {
	heartbeatMapMutex.Lock()
	defer heartbeatMapMutex.Unlock()
	now := time.Now()
	for key, info := range heartbeatMap {
		if now.Sub(info.sendTime) > threshold {
			logDebug("[Cleanup] Removing heartbeat with random_id=%s, age=%v", key, now.Sub(info.sendTime))
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

// processLogLine parses a single JSON log line and updates Prometheus metrics accordingly.
func processLogLine(line string, shortValidator string) {
	var logEntry []interface{}
	if err := json.Unmarshal([]byte(line), &logEntry); err != nil {
		logError("Error unmarshaling line: %v", err)
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
		logError("Error parsing timestamp: %v", err)
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
						lastVoteTime = timestamp
						lastVoteRound.Set(float64(int64(roundVal)))
						lastVoteRoundUpdateTs.Set(float64(time.Now().Unix()))
						logDebug("[Vote] Received vote: round=%d, timestamp=%v", int64(roundVal), timestamp)
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
				randID, ok1 := hbMap["random_id"].(float64)
				if ok1 {
					key := fmt.Sprintf("%d", int64(randID))
					heartbeatMapMutex.Lock()
					heartbeatMap[key] = &heartbeatInfo{
						sendTime: timestamp,
						acks:     make(map[string]bool),
					}
					heartbeatMapMutex.Unlock()
					logDebug("[Heartbeat Out] Registered heartbeat: random_id=%s, timestamp=%v", key, timestamp)
				} else {
					logWarn("[Heartbeat Out] Missing random_id field")
				}
			}
		}
	}

	// --- HeartbeatAck 메시지 처리 ---
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
			logWarn("[HeartbeatAck In] Ack content not a map")
			return
		}
		randID, ok1 := ackMap["random_id"].(float64)
		if ok1 {
			key := fmt.Sprintf("%d", int64(randID))
			heartbeatMapMutex.Lock()
			hbInfo, exists := heartbeatMap[key]
			if exists {
				src, okSrc := msgObj["source"].(string)
				if !okSrc {
					logWarn("[HeartbeatAck In] Source field missing for ack: random_id=%s", key)
					heartbeatMapMutex.Unlock()
					return
				}
				if _, alreadyAcked := hbInfo.acks[src]; alreadyAcked {
					logDebug("[HeartbeatAck In] Duplicate ack from validator=%s for random_id=%s", src, key)
				} else {
					delay := time.Since(hbInfo.sendTime)
					heartbeatAckDelayVec.WithLabelValues(src).Set(float64(delay.Milliseconds()))
					heartbeatAckUpdateTsVec.WithLabelValues(src).Set(float64(time.Now().Unix()))
					hbInfo.acks[src] = true
					logDebug("[HeartbeatAck In] Matched heartbeat: random_id=%s, source=%s, delay=%v", key, src, delay)
				}
			} else {
				logDebug("[HeartbeatAck In] No matching heartbeat found for random_id=%s", key)
			}
			heartbeatMapMutex.Unlock()
		} else {
			logWarn("[HeartbeatAck In] Missing random_id field in ack")
		}
	}
}
