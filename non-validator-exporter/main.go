package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"regexp"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// 데이터 구조체 정의
type VisorState struct {
	InitialHeight   float64  `json:"initial_height"`
	Height          float64  `json:"height"`
	ScheduledFreeze *float64 `json:"scheduled_freeze_height"`
	ConsensusTime   string   `json:"consensus_time"`
}

// Prometheus 메트릭 정의
var (
	initialHeight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "visor_initial_height",
		Help: "Initial height of the visor state",
	})
	height = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "visor_current_height",
		Help: "Current height of the visor state",
	})
	scheduledFreezeHeight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "visor_scheduled_freeze_height",
		Help: "Scheduled freeze height of the visor state (null if not scheduled)",
	})
	consensusTimestamp = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "visor_consensus_timestamp_unix",
		Help: "Consensus timestamp in Unix format",
	})
	mutex sync.Mutex
)

// normalizeTime는 시간 문자열을 RFC3339Nano 포맷에 맞게 보정합니다.
// 1. 소수점 이하 자릿수가 10자리 이상이면 9자리로 줄입니다.
// 2. 문자열 끝에 타임존 정보(Z 또는 ±HH:MM)가 없으면 "Z"를 추가합니다.
func normalizeTime(timeStr string) string {
	// 소수점 이하 10자리 이상인 경우 9자리로 줄이는 정규식 처리
	reFraction := regexp.MustCompile(`\.(\d{10,})`)
	timeStr = reFraction.ReplaceAllStringFunc(timeStr, func(match string) string {
		// match는 "." 포함 숫자들. 최대 10글자(점 포함 1+9자리)로 자릅니다.
		if len(match) > 10 {
			return match[:10]
		}
		return match
	})

	// 마지막에 타임존 정보가 있는지 검사 (Z 또는 +HH:MM 또는 -HH:MM)
	reTZ := regexp.MustCompile(`(Z|[+-]\d{2}:\d{2})$`)
	if !reTZ.MatchString(timeStr) {
		timeStr += "Z"
	}
	return timeStr
}

func updateMetrics(filePath string) {
	mutex.Lock()
	defer mutex.Unlock()

	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		log.Printf("Error reading JSON file: %v", err)
		return
	}

	var state VisorState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("Error parsing JSON: %v", err)
		return
	}

	// 메트릭 업데이트
	initialHeight.Set(state.InitialHeight)
	height.Set(state.Height)
	if state.ScheduledFreeze != nil {
		scheduledFreezeHeight.Set(*state.ScheduledFreeze)
	} else {
		scheduledFreezeHeight.Set(0)
	}

	// consensus_time 정규화 및 파싱
	normalizedTime := normalizeTime(state.ConsensusTime)
	parsedTime, err := time.Parse(time.RFC3339Nano, normalizedTime)
	if err != nil {
		log.Printf("Error parsing consensus time: %v (normalized: %s)", err, normalizedTime)
	} else {
		consensusTimestamp.Set(float64(parsedTime.Unix()))
	}

	log.Println("Metrics updated successfully.")
}

func main() {
	// 파일 경로와 포트를 인자로 받음
	filePath := flag.String("file", "visor_abci_state.json", "Path to the visor JSON file")
	port := flag.Int("port", 8080, "Port number for the exporter HTTP server")
	flag.Parse()

	prometheus.MustRegister(initialHeight)
	prometheus.MustRegister(height)
	prometheus.MustRegister(scheduledFreezeHeight)
	prometheus.MustRegister(consensusTimestamp)

	// 10초마다 JSON 파일 읽어 업데이트
	go func() {
		for {
			updateMetrics(*filePath)
			time.Sleep(10 * time.Second)
		}
	}()

	http.Handle("/metrics", promhttp.Handler())

	serverAddr := fmt.Sprintf(":%d", *port)
	log.Printf("Prometheus Exporter is running on %s, reading file: %s\n", serverAddr, *filePath)
	if err := http.ListenAndServe(serverAddr, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}
