package main

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

var heartbeatSent sync.Map // random_id -> sent time

var (
	lastVotedRound = NewAtomicGauge()
	currentRound   = NewAtomicGauge()
)

type Heartbeat struct {
	Validator string `json:"validator"`
	Round     int64  `json:"round"`
	RandomID  int64  `json:"random_id"`
}

type HeartbeatAck struct {
	Validator string `json:"validator"`
	Round     int64  `json:"round"`
	RandomID  int64  `json:"random_id"`
}

func handleHeartbeatSentFromLine(data string, ts time.Time, shortAddr string) {
	if !strings.Contains(data, shortAddr) {
		return
	}

	var wrapper map[string]map[string]Heartbeat
	if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
		return
	}
	for _, inner := range wrapper {
		hb := inner["Heartbeat"]
		heartbeatSent.Store(hb.RandomID, ts)
		break
	}
}

func handleHeartbeatAckFromLine(data string, ts time.Time, shortAddr string) {
	if !strings.Contains(data, shortAddr) {
		return
	}

	var wrapper map[string]map[string]HeartbeatAck
	if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
		return
	}
	for _, inner := range wrapper {
		hba := inner["HeartbeatAck"]
		if sentTimeRaw, ok := heartbeatSent.Load(hba.RandomID); ok {
			sentTime := sentTimeRaw.(time.Time)
			delay := ts.Sub(sentTime).Milliseconds()
			heartbeatAckDelay.Observe(float64(delay))
			heartbeatSent.Delete(hba.RandomID)
		}
		break
	}
}

func handleVoteFromLine(data string, shortAddr string) {
	if !strings.Contains(data, shortAddr) {
		return
	}

	var raw map[string]map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}

	for _, inner := range raw {
		vote, ok := inner["Vote"]
		if !ok {
			continue
		}
		voteData, ok := vote["vote"].(map[string]interface{})
		if !ok {
			continue
		}
		round, ok := voteData["round"].(float64)
		if ok {
			lastVotedRound.Set(int64(round))
		}
	}
}

func handleCurrentRoundFromLine(data string) {
	var raw map[string]map[string]interface{}
	if err := json.Unmarshal([]byte(data), &raw); err != nil {
		return
	}

	for _, msg := range raw {
		block, ok := msg["msg"].(map[string]interface{})
		if !ok || block["Block"] == nil {
			continue
		}
		blockData := block["Block"].(map[string]interface{})
		round, ok := blockData["round"].(float64)
		if ok {
			currentRound.Set(int64(round))
		}
	}
}
