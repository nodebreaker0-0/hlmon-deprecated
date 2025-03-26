package main

import (
	"encoding/json"
	"log"
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
		log.Printf("[hb] Stored heartbeat: random_id=%d round=%d", hb.RandomID, hb.Round)
		break
	}
}

func handleHeartbeatAckFromLine(data string, ts time.Time, shortAddr string) {
	var wrapper map[string]map[string]HeartbeatAck
	if err := json.Unmarshal([]byte(data), &wrapper); err != nil {
		return
	}
	for _, inner := range wrapper {
		hba := inner["HeartbeatAck"]
		if sentTimeRaw, ok := heartbeatSent.Load(hba.RandomID); ok {
			sentTime := sentTimeRaw.(time.Time)
			delay := ts.Sub(sentTime).Milliseconds()
			peer := hba.Validator
			heartbeatAckDelayByPeer.WithLabelValues(peer).Set(float64(delay))
			heartbeatSent.Delete(hba.RandomID)
			log.Printf("[ack] Ack delay from %s: %dms (random_id=%d)", peer, delay, hba.RandomID)
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
			log.Printf("[vote] Round: %d", int64(round))
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
			log.Printf("[round] Current round: %d", int64(round))
		}
	}
}
