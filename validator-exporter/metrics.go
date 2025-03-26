package main

import (
	"sync/atomic"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	heartbeatAckDelayByPeer = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "heartbeat_ack_delay_ms",
			Help: "Delay in ms between sent heartbeat and ack received, by peer",
		},
		[]string{"peer"},
	)

	lastVotedRoundGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "last_voted_round",
			Help: "Last round this validator voted in",
		},
		func() float64 { return float64(lastVotedRound.Get()) },
	)

	currentRoundGauge = prometheus.NewGaugeFunc(
		prometheus.GaugeOpts{
			Name: "current_round",
			Help: "Current consensus round",
		},
		func() float64 { return float64(currentRound.Get()) },
	)
)

func registerMetrics() {
	prometheus.MustRegister(heartbeatAckDelayByPeer)
	prometheus.MustRegister(lastVotedRoundGauge)
	prometheus.MustRegister(currentRoundGauge)
}

// AtomicGauge safely holds int64 values with atomic operations
type AtomicGauge struct {
	v atomic.Int64
}

func NewAtomicGauge() *AtomicGauge {
	return &AtomicGauge{}
}

func (g *AtomicGauge) Set(val int64) {
	g.v.Store(val)
}

func (g *AtomicGauge) Get() int64 {
	return g.v.Load()
}
