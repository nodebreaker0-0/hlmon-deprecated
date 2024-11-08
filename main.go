package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/PagerDuty/go-pagerduty"
)

// Config struct defines configuration values from the TOML file.
type Config struct {
	SlackWebhookURL       string  `toml:"slack_webhook_url"`
	PagerDutyRoutingKey   string  `toml:"pagerduty_routing_key"`
	BasePath              string  `toml:"base_path"`
	ValidatorAddress      string  `toml:"validator_address"`
	CheckInterval         int     `toml:"check_interval"`
	AlertThresholdSuccess float64 `toml:"alert_threshold_success"`
	AlertThresholdAck     float64 `toml:"alert_threshold_ack"`
	LogUpdateInterval     int     `toml:"log_update_interval"`
}

// ValidatorData stores the health and heartbeat status of the validator.
type ValidatorData struct {
	HomeValidator              string                     `json:"home_validator"`
	ValidatorsMissingHeartbeat []string                   `json:"validators_missing_heartbeat"`
	HeartbeatStatuses          map[string]HeartbeatStatus `json:"heartbeat_statuses"`
}

// HeartbeatStatus holds details of the heartbeat status.
type HeartbeatStatus struct {
	SinceLastSuccess float64  `json:"since_last_success"`
	LastAckDuration  *float64 `json:"last_ack_duration"`
}

// LogArrayEntry holds a single log entry for monitoring.
type LogArrayEntry struct {
	Timestamp string        `json:"timestamp"`
	Validator ValidatorData `json:"validator_data"`
}

// sendSlackAlert sends a message to a Slack channel using a webhook.
func sendSlackAlert(webhookURL, message string) {
	payload := map[string]string{"text": message}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Failed to marshal Slack payload: %v", err)
		return
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewBuffer(payloadBytes))
	if err != nil {
		log.Printf("Failed to create Slack request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Failed to send Slack alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Slack alert returned non-200 status: %d", resp.StatusCode)
	}
}

// sendPagerDutyAlert triggers a PagerDuty incident using the provided routing key.
func sendPagerDutyAlert(routingKey, description string) {
	event := pagerduty.V2Event{
		RoutingKey: routingKey,
		Action:     "trigger",
		Payload: &pagerduty.V2Payload{
			Summary:   description,
			Source:    "validator-monitoring-script",
			Severity:  "critical",
			Component: "Validator Monitoring",
		},
	}
	_, err := pagerduty.ManageEventWithContext(context.Background(), event)
	if err != nil {
		log.Printf("PagerDuty API Error: %s\n", err)
	}
}

// UnmarshalJSON custom unmarshaller for ValidatorData to handle nested JSON array.
func (vd *ValidatorData) UnmarshalJSON(data []byte) error {
	// Create a temporary struct for the standard fields
	type Alias ValidatorData
	aux := &struct {
		HeartbeatStatuses [][]interface{} `json:"heartbeat_statuses"`
		*Alias
	}{
		Alias: (*Alias)(vd),
	}

	// Unmarshal the JSON into the auxiliary structure
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}

	// Convert HeartbeatStatuses from array to map
	vd.HeartbeatStatuses = make(map[string]HeartbeatStatus)
	for _, entry := range aux.HeartbeatStatuses {
		if len(entry) != 2 {
			continue
		}

		key, ok := entry[0].(string)
		if !ok {
			continue
		}

		valueBytes, err := json.Marshal(entry[1])
		if err != nil {
			continue
		}

		var heartbeatStatus HeartbeatStatus
		if err := json.Unmarshal(valueBytes, &heartbeatStatus); err != nil {
			continue
		}

		vd.HeartbeatStatuses[key] = heartbeatStatus
	}

	return nil
}

// main function reads configuration, processes the latest log file, and monitors.
func main() {
	var config Config
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading configuration: %s\n", err)
	}

	var lastLogTimestamp time.Time

	for {
		latestLogFile, err := findLatestLogFile(config.BasePath)
		if err != nil {
			log.Fatalf("Failed to find latest log file: %v", err)
		}
		println(latestLogFile)

		file, err := os.Open(latestLogFile)
		if err != nil {
			log.Printf("Error opening log file: %s\n", err)
			time.Sleep(30 * time.Second)
			continue
		}

		decoder := json.NewDecoder(file)
		var lastRawEntry json.RawMessage
		for {
			var rawEntry json.RawMessage
			if err := decoder.Decode(&rawEntry); err != nil {
				if err.Error() == "EOF" {
					break
				}
				log.Printf("Error decoding JSON line: %s\n", err)
				continue
			}
			lastRawEntry = rawEntry
		}

		if lastRawEntry != nil {
			// Attempt to unmarshal as an array containing a timestamp and data
			var logArray []interface{}
			if err := json.Unmarshal(lastRawEntry, &logArray); err == nil && len(logArray) == 2 {
				// Get only the last element
				timestamp, ok := logArray[0].(string)
				if !ok {
					log.Printf("Error: Expected timestamp as first element, got: %v", logArray[0])
					continue
				}

				validatorDataBytes, err := json.Marshal(logArray[1])
				if err != nil {
					log.Printf("Error marshaling validator data: %s", err)
					continue
				}

				var validatorData ValidatorData
				if err := json.Unmarshal(validatorDataBytes, &validatorData); err != nil {
					log.Printf("Error decoding validator data: %s", err)
					continue
				}

				// Create the log entry with the last element
				logEntry := LogArrayEntry{
					Timestamp: timestamp,
					Validator: validatorData,
				}

				// Process the last log entry only
				processLogEntry(logEntry, config)

				// Update last log timestamp
				parsedTimestamp, err := time.Parse("2006-01-02T15:04:05.999999999", timestamp)
				if err != nil {
					log.Printf("Error parsing timestamp: %s", err)
					continue
				}
				lastLogTimestamp = parsedTimestamp
			} else {
				log.Printf("Error: Could not unmarshal JSON line as expected array")
			}
		}

		file.Close()

		// Check if the log file has not been updated for more than 5 minutes
		if !lastLogTimestamp.IsZero() && time.Since(lastLogTimestamp) > time.Duration(config.LogUpdateInterval)*time.Second {
			alertMessage := fmt.Sprintf("Alert for HyperLiq validator: no updates for 5 minutes, the hl-visor (or consensus) should be considered dead.")
			sendSlackAlert(config.SlackWebhookURL, alertMessage)
			sendPagerDutyAlert(config.PagerDutyRoutingKey, alertMessage)
			log.Println(alertMessage)
		}

		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}
func formatLastAckDuration(d *float64) string {
	if d == nil {
		return "N/A" // 기본값 또는 설명 메시지
	}
	return fmt.Sprintf("%.6f", *d) // 소수점 6자리까지 출력
}

// processLogEntry checks if thresholds are exceeded and triggers alerts accordingly.
func processLogEntry(logEntry LogArrayEntry, config Config) {
	log.Printf("Timestamp: %s\n", logEntry.Timestamp)
	if status, found := logEntry.Validator.HeartbeatStatuses[config.ValidatorAddress]; found {
		lastAckDurationStr := formatLastAckDuration(status.LastAckDuration)

		if status.SinceLastSuccess > config.AlertThresholdSuccess ||
			(status.LastAckDuration != nil && *status.LastAckDuration > config.AlertThresholdAck) ||
			status.LastAckDuration == nil {

			alertMessage := fmt.Sprintf(
				"Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %s (Value Exceeded)",
				config.ValidatorAddress, status.SinceLastSuccess, lastAckDurationStr,
			)
			sendSlackAlert(config.SlackWebhookURL, alertMessage)
			sendPagerDutyAlert(config.PagerDutyRoutingKey, alertMessage)
			log.Println(alertMessage)
		}
	} else {
		alertMessage := fmt.Sprintf("Validator %s not found in heartbeat statuses.", config.ValidatorAddress)
		sendSlackAlert(config.SlackWebhookURL, alertMessage)
		sendPagerDutyAlert(config.PagerDutyRoutingKey, alertMessage)
		log.Printf("Validator %s not found in heartbeat statuses.", config.ValidatorAddress)
	}
}

// findLatestLogFile finds the latest log file to be processed.
func findLatestLogFile(basePath string) (string, error) {
	latestDateDir, err := findLatestDir(basePath)
	if err != nil {
		return "", fmt.Errorf("failed to find latest date directory: %w", err)
	}

	latestLogFile, err := findLatestFile(latestDateDir)
	if err != nil {
		return "", fmt.Errorf("failed to find latest log file: %w", err)
	}

	return latestLogFile, nil
}

// findLatestDir returns the latest directory based on lexicographical order.
func findLatestDir(basePath string) (string, error) {
	entries, err := os.ReadDir(basePath)
	if err != nil {
		return "", err
	}

	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}

	if len(dirs) == 0 {
		return "", fmt.Errorf("no directories found in %s", basePath)
	}

	// Sort directories in lexicographical order to ensure latest date is last
	sort.Slice(dirs, func(i, j int) bool {
		return dirs[i] < dirs[j]
	})

	latestDir := dirs[len(dirs)-1]

	return fmt.Sprintf("%s/%s", basePath, latestDir), nil
}

// findLatestFile returns the latest log file within a directory.
func findLatestFile(dirPath string) (string, error) {
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		return "", err
	}

	var files []string
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, entry.Name())
		}
	}

	if len(files) == 0 {
		return "", fmt.Errorf("no files found in %s", dirPath)
	}

	// Sort files to ensure correct order
	sort.Slice(files, func(i, j int) bool {
		iInt, errI := strconv.Atoi(files[i])
		jInt, errJ := strconv.Atoi(files[j])
		if errI == nil && errJ == nil {
			return iInt < jInt
		}
		return files[i] < files[j]
	})

	latestFile := files[len(files)-1]

	return fmt.Sprintf("%s/%s", dirPath, latestFile), nil
}
