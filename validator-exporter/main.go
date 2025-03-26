package main

import (
	"flag"
	"log"
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	validatorAddress  string
	consensusPath     string
	validatorFullAddr string
)

func main() {
	flag.StringVar(&validatorAddress, "validator-address", "", "Full validator address (e.g. 0xef22f260eec3b7d1edebe53359f5ca584c18d5ac)")
	flag.StringVar(&consensusPath, "consensus-path", "", "Path to consensus logs root directory")
	flag.Parse()

	if validatorAddress == "" || consensusPath == "" {
		log.Fatal("Both --validator-address and --consensus-path must be specified")
	}

	// 축약형 주소 (0xef22..d5ac) 만들기
	shortAddr := shrinkAddress(validatorAddress)
	validatorFullAddr = validatorAddress

	log.Printf("Monitoring validator: %s (%s)", validatorAddress, shortAddr)

	// Prometheus 메트릭 등록
	registerMetrics()

	// 로그 워처 시작 (병렬 처리 + 레이스컨디션 안전 고려됨)
	go startLogWatcher(shortAddr, consensusPath)

	// 메트릭 서버 시작
	http.Handle("/metrics", promhttp.Handler())
	log.Println("Serving metrics at :2112/metrics")
	http.ListenAndServe(":2112", nil)
}

func shrinkAddress(full string) string {
	if len(full) < 10 {
		return full
	}
	return full[:6] + ".." + full[len(full)-4:]
}
