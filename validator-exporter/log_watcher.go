package main

import (
	"bufio"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type LogLine struct {
	Timestamp time.Time
	Message   string
}

func startLogWatcher(shortAddr, basePath string) {
	go func() {
		for {
			latestDir := findLatestLogDir(basePath)
			if latestDir == "" {
				log.Println("[watcher] No latest log dir found, retrying...")
				time.Sleep(3 * time.Second)
				continue
			}

			log.Println("[watcher] Using latest dir:", latestDir)

			for hour := 0; hour < 24; hour++ {
				filePath := filepath.Join(latestDir, strconv.Itoa(hour))
				if _, err := os.Stat(filePath); err == nil {
					log.Printf("[tail] Starting tail on file: %s", filePath)
					go tailFile(filePath, shortAddr)
				}
			}

			time.Sleep(30 * time.Second)
		}
	}()
}

func findLatestLogDir(base string) string {
	dateDirs, err := os.ReadDir(base)
	if err != nil {
		log.Println("[watcher] Failed to read base path:", err)
		return ""
	}

	latest := ""
	sort.Slice(dateDirs, func(i, j int) bool {
		return dateDirs[i].Name() > dateDirs[j].Name()
	})

	for _, dateDir := range dateDirs {
		path := filepath.Join(base, dateDir.Name())
		hourDirs, err := os.ReadDir(path)
		if err != nil || len(hourDirs) == 0 {
			continue
		}
		latest = path
		break
	}
	return latest
}

func tailFile(path, shortAddr string) {
	file, err := os.Open(path)
	if err != nil {
		log.Println("[tail] Error opening file:", err)
		return
	}
	defer file.Close()

	_, _ = file.Seek(0, io.SeekEnd) // move to end
	reader := bufio.NewReader(file)

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				time.Sleep(100 * time.Millisecond)
				continue
			} else {
				log.Println("[tail] Read error:", err)
				return
			}
		}

		trimmed := strings.TrimSpace(line)
		log.Printf("[parse] Raw log: %s", trimmed)
		go parseLogLine(trimmed, shortAddr)
	}
}

func parseLogLine(line, shortAddr string) {
	if !strings.Contains(line, shortAddr) && !strings.Contains(line, "Block") {
		return // 관심 없는 validator + current round용 Block 제외
	}

	var raw []any
	if err := json.Unmarshal([]byte(line), &raw); err != nil || len(raw) < 2 {
		log.Println("[parse] Failed to parse JSON line:", err)
		return
	}
	tsStr, ok := raw[0].(string)
	if !ok {
		log.Println("[parse] Invalid timestamp in log line")
		return
	}
	ts, err := time.Parse(time.RFC3339Nano, tsStr)
	if err != nil {
		log.Println("[parse] Timestamp parse error:", err)
		return
	}

	data, _ := json.Marshal(raw[1])
	msgStr := string(data)

	if strings.Contains(msgStr, "\"Heartbeat\":{\"validator\":") {
		log.Println("[parse] Detected Heartbeat message")
		handleHeartbeatSentFromLine(msgStr, ts, shortAddr)
	} else if strings.Contains(msgStr, "\"HeartbeatAck\"") {
		log.Println("[parse] Detected HeartbeatAck message")
		handleHeartbeatAckFromLine(msgStr, ts, shortAddr)
	} else if strings.Contains(msgStr, "\"Vote\":") {
		log.Println("[parse] Detected Vote message")
		handleVoteFromLine(msgStr, shortAddr)
	} else if strings.Contains(msgStr, "\"Block\":") {
		log.Println("[parse] Detected Block message")
		handleCurrentRoundFromLine(msgStr)
	}
}
