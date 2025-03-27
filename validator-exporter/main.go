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
	//--------------------------------------------------
	// 기존/추가 메트릭들
	//--------------------------------------------------

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

	// Vote 시간 차이(초)
	voteTimeDiff = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "vote_time_diff_seconds",
		Help: "Time difference in seconds between the timestamp of the last vote and the current time",
	})

	// Status 로그 관련
	myValidatorJailed = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "my_validator_jailed",
		Help: "1 if my validator is jailed, 0 otherwise",
	})
	myValidatorMissingHeartbeat = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "my_validator_missing_heartbeat",
		Help: "1 if my validator is in validators_missing_heartbeat, 0 otherwise",
	})
	myValidatorSinceLastSuccess = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "my_validator_since_last_success",
		Help: "Heartbeat since_last_success from status logs for my validator",
	})
	myValidatorLastAckDuration = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "my_validator_last_ack_duration",
		Help: "Heartbeat last_ack_duration from status logs for my validator",
	})
	disconnectedValidatorGauge = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "disconnected_validator",
			Help: "1 if a validator is disconnected from me, 0 otherwise",
		},
		[]string{"validator"},
	)

	// 마지막으로 읽은 로그 라인의 "로그 타임스탬프"(= JSON 첫 칸)
	lastConsensusLogReadTS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "last_consensus_log_read_ts",
		Help: "Timestamp (unix) of the last consensus log line's own timestamp",
	})
	lastStatusLogReadTS = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "last_status_log_read_ts",
		Help: "Timestamp (unix) of the last status log line's own timestamp",
	})

	//--------------------------------------------------
	// 기타 전역
	//--------------------------------------------------
	heartbeatMap      = make(map[string]*heartbeatInfo)
	heartbeatMapMutex sync.Mutex

	lastVoteTime time.Time

	disconnectedSet      = make(map[string]bool)
	disconnectedSetMutex sync.Mutex
)

func main() {
	//--------------------------------------------------
	// 플래그 파싱
	//--------------------------------------------------
	validatorAddrFull := flag.String("validator-address", "", "Full validator address")
	consensusPath := flag.String("consensus-path", "", "Path to consensus logs (hourly directory)")
	statusPath := flag.String("status-path", "", "Path to status logs (hourly directory)")
	logLevelFlag := flag.Int("log-level", LogLevelInfo, "Log level (1=ERROR, 2=WARN, 3=INFO, 4=DEBUG)")
	flag.Parse()

	logLevel = *logLevelFlag

	if *validatorAddrFull == "" || *consensusPath == "" || *statusPath == "" {
		log.Fatal("All of --validator-address, --consensus-path, and --status-path are required")
	}

	shortValidator := shortenAddress(*validatorAddrFull)

	//--------------------------------------------------
	// Prometheus 메트릭 등록
	//--------------------------------------------------
	prometheus.MustRegister(
		lastVoteRound, currentRound, heartbeatAckDelayVec,
		lastVoteRoundUpdateTs, currentRoundUpdateTs, heartbeatAckUpdateTsVec, voteTimeDiff,

		myValidatorJailed, myValidatorMissingHeartbeat,
		myValidatorSinceLastSuccess, myValidatorLastAckDuration,
		disconnectedValidatorGauge,

		lastConsensusLogReadTS, lastStatusLogReadTS,
	)

	//--------------------------------------------------
	// vote_time_diff_seconds 갱신
	//--------------------------------------------------
	go func() {
		for {
			time.Sleep(1 * time.Second)
			if !lastVoteTime.IsZero() {
				diff := time.Since(lastVoteTime).Seconds()
				voteTimeDiff.Set(diff)
			}
		}
	}()

	//--------------------------------------------------
	// HTTP 서버
	//--------------------------------------------------
	go func() {
		http.Handle("/metrics", promhttp.Handler())
		log.Fatal(http.ListenAndServe(":2112", nil))
	}()

	//--------------------------------------------------
	// heartbeatMap cleanup
	//--------------------------------------------------
	go func() {
		for {
			time.Sleep(1 * time.Second)
			cleanupHeartbeatMap(5 * time.Second)
		}
	}()

	//--------------------------------------------------
	// 컨센서스 로그 처리
	//--------------------------------------------------
	consensusLineCh := make(chan string, 10000)
	const numConsensusWorkers = 16
	for i := 0; i < numConsensusWorkers; i++ {
		go func() {
			for line := range consensusLineCh {
				processLogLine(line, shortValidator)
			}
		}()
	}
	go tailLogs(*consensusPath, consensusLineCh)

	//--------------------------------------------------
	// 스테이터스 로그 처리
	//--------------------------------------------------
	statusLineCh := make(chan string, 1000)
	const numStatusWorkers = 4
	for i := 0; i < numStatusWorkers; i++ {
		go func() {
			for line := range statusLineCh {
				processStatusLogLine(line, *validatorAddrFull)
			}
		}()
	}
	go tailStatusLogs(*statusPath, statusLineCh)

	//--------------------------------------------------
	// 메인 루프 블로킹
	//--------------------------------------------------
	select {}
}

// tailLogs continuously tails log files which are rotated hourly (consensus logs).
func tailLogs(basePath string, lineCh chan<- string) {
	for {
		now := time.Now()
		dateDir := now.Format("20060102")
		hourDir := fmt.Sprintf("%d", now.Hour())
		filePath := filepath.Join(basePath, dateDir, hourDir)

		logInfo("Tailing consensus log file: %s", filePath)
		file, err := os.Open(filePath)
		if err != nil {
			logError("Error opening consensus file %s: %v", filePath, err)
			time.Sleep(10 * time.Second)
			continue
		}

		reader := bufio.NewReader(file)
		lastReadTime := time.Now()

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					time.Sleep(100 * time.Millisecond)
					if time.Since(lastReadTime) > 1*time.Minute {
						logInfo("No new consensus logs for 1 minute, switching to new file.")
						break
					}
					continue
				} else {
					logError("Error reading consensus file %s: %v", filePath, err)
					break
				}
			}
			lastReadTime = time.Now()
			lineCh <- line
		}
		file.Close()
		time.Sleep(500 * time.Millisecond)
	}
}

// tailStatusLogs continuously tails log files which are rotated hourly (status logs).
func tailStatusLogs(basePath string, lineCh chan<- string) {
	for {
		now := time.Now()
		dateDir := now.Format("20060102")
		hourDir := fmt.Sprintf("%d", now.Hour())
		filePath := filepath.Join(basePath, dateDir, hourDir)

		logInfo("Tailing status log file: %s", filePath)
		file, err := os.Open(filePath)
		if err != nil {
			logError("Error opening status file %s: %v", filePath, err)
			time.Sleep(10 * time.Second)
			continue
		}

		reader := bufio.NewReader(file)
		lastReadTime := time.Now()

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				if err == io.EOF {
					time.Sleep(100 * time.Millisecond)
					if time.Since(lastReadTime) > 1*time.Minute {
						logInfo("No new status logs for 1 minute, switching to new file.")
						break
					}
					continue
				} else {
					logError("Error reading status file %s: %v", filePath, err)
					break
				}
			}
			lastReadTime = time.Now()
			lineCh <- line
		}
		file.Close()
		time.Sleep(500 * time.Millisecond)
	}
}

// shortenAddress returns a shortened version (first 6 and last 4 chars) of the validator address.
func shortenAddress(addr string) string {
	if len(addr) < 10 {
		return addr
	}
	return addr[:6] + ".." + addr[len(addr)-4:]
}

// cleanupHeartbeatMap removes heartbeat entries older than the given duration.
func cleanupHeartbeatMap(threshold time.Duration) {
	heartbeatMapMutex.Lock()
	defer heartbeatMapMutex.Unlock()
	now := time.Now()
	for key, info := range heartbeatMap {
		if now.Sub(info.sendTime) > threshold {
			logDebug("[Cleanup] Removing heartbeat random_id=%s, age=%v", key, now.Sub(info.sendTime))
			delete(heartbeatMap, key)
		}
	}
}

// processLogLine parses a single JSON log line from consensus logs,
// and updates relevant metrics. (including lastConsensusLogReadTS)
func processLogLine(line string, shortValidator string) {
	var logEntry []interface{}
	if err := json.Unmarshal([]byte(line), &logEntry); err != nil {
		logError("Error unmarshaling consensus line: %v", err)
		return
	}
	if len(logEntry) < 2 {
		return
	}

	// 첫 번째 필드 = 로그 자체 타임스탬프
	timestampStr, ok := logEntry[0].(string)
	if !ok {
		return
	}
	parsedTime, err := time.Parse("2006-01-02T15:04:05.999999999", timestampStr)
	if err != nil {
		logDebug("Error parsing consensus timestamp: %v", err)
		return
	}

	// -> 여기서 메트릭 업데이트 (로그가 가진 타임스탬프)
	lastConsensusLogReadTS.Set(float64(parsedTime.Unix()))

	// 두 번째 필드 = 실제 내용
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

	// Vote 처리
	if voteVal, exists := msgObj["Vote"]; exists {
		voteMap, ok := voteVal.(map[string]interface{})
		if ok {
			voteData, ok := voteMap["vote"].(map[string]interface{})
			if ok {
				validator, ok := voteData["validator"].(string)
				if ok && validator == shortValidator {
					if roundVal, ok := voteData["round"].(float64); ok {
						// lastVoteTime 갱신
						lastVoteTime = parsedTime
						lastVoteRound.Set(roundVal)
						lastVoteRoundUpdateTs.Set(float64(time.Now().Unix()))

						logDebug("[Vote] Received vote: round=%d, timestamp=%v", int64(roundVal), parsedTime)
					}
				}
			}
		}
	}

	// Block 처리 (현재 round)
	if blockVal, exists := msgObj["Block"]; exists {
		blockMap, ok := blockVal.(map[string]interface{})
		if ok {
			if roundVal, ok := blockMap["round"].(float64); ok {
				currentRound.Set(roundVal)
				currentRoundUpdateTs.Set(float64(time.Now().Unix()))
			}
		}
	}

	// Heartbeat out 처리
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
						sendTime: parsedTime,
						acks:     make(map[string]bool),
					}
					heartbeatMapMutex.Unlock()

					logDebug("[Heartbeat Out] Registered heartbeat: random_id=%s, ts=%v", key, parsedTime)
				} else {
					logWarn("[Heartbeat Out] Missing random_id field")
				}
			}
		}
	}

	// HeartbeatAck in 처리
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
					logWarn("[HeartbeatAck In] Source field missing for ack random_id=%s", key)
					heartbeatMapMutex.Unlock()
					return
				}
				if _, alreadyAcked := hbInfo.acks[src]; alreadyAcked {
					logDebug("[HeartbeatAck In] Duplicate ack from validator=%s random_id=%s", src, key)
				} else {
					delay := time.Since(hbInfo.sendTime)
					heartbeatAckDelayVec.WithLabelValues(src).Set(float64(delay.Milliseconds()))
					heartbeatAckUpdateTsVec.WithLabelValues(src).Set(float64(time.Now().Unix()))
					hbInfo.acks[src] = true

					logDebug("[HeartbeatAck In] Matched heartbeat random_id=%s source=%s delay=%v", key, src, delay)
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

// processStatusLogLine parses a single JSON log line from status logs.
// and updates relevant metrics. (including lastStatusLogReadTS)
func processStatusLogLine(line string, myValidatorAddr string) {
	var logEntry []interface{}
	if err := json.Unmarshal([]byte(line), &logEntry); err != nil {
		logError("Error unmarshaling status line: %v", err)
		return
	}
	if len(logEntry) < 2 {
		return
	}

	// 첫 번째 필드 = 로그 자체 타임스탬프
	timestampStr, ok := logEntry[0].(string)
	if !ok {
		return
	}
	parsedTime, err := time.Parse("2006-01-02T15:04:05.999999999", timestampStr)
	if err != nil {
		logDebug("Error parsing status timestamp: %v", err)
		return
	}

	// -> 여기서 메트릭 업데이트 (로그가 가진 타임스탬프)
	lastStatusLogReadTS.Set(float64(parsedTime.Unix()))

	// 두 번째 필드 = status 정보
	bodyMap, ok := logEntry[1].(map[string]interface{})
	if !ok {
		return
	}

	// home_validator 체크
	homeVal, ok := bodyMap["home_validator"].(string)
	if !ok {
		return
	}
	if homeVal != myValidatorAddr {
		return
	}

	// current_jailed_validators
	jailedList, _ := bodyMap["current_jailed_validators"].([]interface{})
	isJailed := 0.0
	for _, v := range jailedList {
		if vStr, ok := v.(string); ok {
			if vStr == myValidatorAddr {
				isJailed = 1.0
				break
			}
		}
	}
	myValidatorJailed.Set(isJailed)

	// validators_missing_heartbeat
	missingHBList, _ := bodyMap["validators_missing_heartbeat"].([]interface{})
	isMissingHB := 0.0
	for _, v := range missingHBList {
		if vStr, ok := v.(string); ok {
			if vStr == myValidatorAddr {
				isMissingHB = 1.0
				break
			}
		}
	}
	myValidatorMissingHeartbeat.Set(isMissingHB)

	// heartbeat_statuses => [ [ "valAddr", { since_last_success, last_ack_duration } ], ... ]
	hsArr, _ := bodyMap["heartbeat_statuses"].([]interface{})
	for _, item := range hsArr {
		entry, ok := item.([]interface{})
		if !ok || len(entry) < 2 {
			continue
		}
		valAddr, ok1 := entry[0].(string)
		infoMap, ok2 := entry[1].(map[string]interface{})
		if !ok1 || !ok2 {
			continue
		}
		if valAddr == myValidatorAddr {
			// since_last_success
			if s, ok := infoMap["since_last_success"].(float64); ok {
				myValidatorSinceLastSuccess.Set(s)
			} else {
				myValidatorSinceLastSuccess.Set(0)
			}
			// last_ack_duration
			if d, ok := infoMap["last_ack_duration"].(float64); ok {
				myValidatorLastAckDuration.Set(d)
			} else {
				myValidatorLastAckDuration.Set(0)
			}
			break
		}
	}

	// disconnected_validators => [ [ "내주소", [ ["상대주소", <round>], ... ] ], ... ]
	discArr, _ := bodyMap["disconnected_validators"].([]interface{})
	newSet := make(map[string]bool)

	for _, item := range discArr {
		sub, ok := item.([]interface{})
		if !ok || len(sub) < 2 {
			continue
		}
		valAddr, ok := sub[0].(string)
		if !ok {
			continue
		}
		if valAddr == myValidatorAddr {
			detailList, ok := sub[1].([]interface{})
			if !ok {
				continue
			}
			for _, d := range detailList {
				dArr, ok := d.([]interface{})
				if !ok || len(dArr) < 1 {
					continue
				}
				peerAddr, ok := dArr[0].(string)
				if !ok {
					continue
				}
				newSet[peerAddr] = true
			}
		}
	}

	disconnectedSetMutex.Lock()
	// 1) 기존에 있었는데 이번에 사라진 것 => 0
	for oldAddr := range disconnectedSet {
		if _, stillThere := newSet[oldAddr]; !stillThere {
			disconnectedValidatorGauge.WithLabelValues(oldAddr).Set(0)
		}
	}
	// 2) 새로 생긴 것 => 1
	for newAddr := range newSet {
		disconnectedValidatorGauge.WithLabelValues(newAddr).Set(1)
	}
	disconnectedSet = newSet
	disconnectedSetMutex.Unlock()
}
