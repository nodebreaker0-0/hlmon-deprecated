package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
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

// JSON 파일을 읽고 메트릭을 업데이트하는 함수
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
		scheduledFreezeHeight.Set(0) // NULL 값일 경우 0으로 설정
	}

	// Consensus Time을 Unix Timestamp로 변환
	parsedTime, err := time.Parse(time.RFC3339Nano, state.ConsensusTime)
	if err != nil {
		log.Printf("Error parsing consensus time: %v", err)
	} else {
		consensusTimestamp.Set(float64(parsedTime.Unix()))
	}

	log.Println("Metrics updated successfully.")
}

func main() {
	// 실행 시 사용할 파일 경로 및 포트 플래그 추가
	filePath := flag.String("file", "visor_abci_state.json", "Path to the visor JSON file")
	port := flag.Int("port", 8080, "Port number for the exporter HTTP server")
	flag.Parse()

	// Prometheus에 메트릭 등록
	prometheus.MustRegister(initialHeight)
	prometheus.MustRegister(height)
	prometheus.MustRegister(scheduledFreezeHeight)
	prometheus.MustRegister(consensusTimestamp)

	// 10초마다 JSON 파일 읽기
	go func() {
		for {
			updateMetrics(*filePath)
			time.Sleep(10 * time.Second)
		}
	}()

	// `/metrics` 엔드포인트 제공
	http.Handle("/metrics", promhttp.Handler())

	// HTTP 서버 시작
	serverAddr := fmt.Sprintf(":%d", *port)
	log.Printf("Prometheus Exporter is running on %s, reading file: %s\n", serverAddr, *filePath)

	if err := http.ListenAndServe(serverAddr, nil); err != nil {
		log.Fatalf("Error starting HTTP server: %v", err)
	}
}
