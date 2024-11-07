package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/PagerDuty/go-pagerduty"
	"github.com/slack-go/slack"
)

type Config struct {
	SlackToken         string `toml:"slack_token"`
	SlackChannel       string `toml:"slack_channel"`
	PagerDutyAPIKey    string `toml:"pagerduty_api_key"`
	PagerDutyServiceID string `toml:"pagerduty_service_id"`
	BasePath           string `toml:"base_path"`
	ValidatorAddress   string `toml:"validator_address"`
	CheckInterval      int    `toml:"check_interval"`
}

type ValidatorData struct {
	HomeValidator              string                     `json:"home_validator"`
	ValidatorsMissingHeartbeat []string                   `json:"validators_missing_heartbeat"`
	HeartbeatStatuses          map[string]HeartbeatStatus `json:"heartbeat_statuses"`
}

type HeartbeatStatus struct {
	SinceLastSuccess float64  `json:"since_last_success"`
	LastAckDuration  *float64 `json:"last_ack_duration"`
}

type LogArrayEntry struct {
	Timestamp string        `json:"timestamp"`
	Validator ValidatorData `json:"validator_data"`
}

func sendSlackAlert(api *slack.Client, channel, message string) {
	_, _, err := api.PostMessage(
		channel,
		slack.MsgOptionText(message, false),
	)
	if err != nil {
		log.Printf("Slack API Error: %s\n", err)
	}
}

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
func main() {
	var config Config
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading configuration: %s\n", err)
	}

	slackClient := slack.New(config.SlackToken)

	latestLogFile, err := findLatestLogFile(config.BasePath)
	if err != nil {
		log.Fatalf("Failed to find latest log file: %v", err)
	}
	log.Printf("Reading latest log file: %s\n", latestLogFile)

	for {
		file, err := os.Open(latestLogFile)
		if err != nil {
			log.Printf("Error opening log file: %s\n", err)
			time.Sleep(10 * time.Second)
			continue
		}

		decoder := json.NewDecoder(file)
		for {
			var rawEntry json.RawMessage
			if err := decoder.Decode(&rawEntry); err != nil {
				if err.Error() == "EOF" {
					break
				}
				log.Printf("Error decoding JSON line: %s\n", err)
				continue
			}

			// Attempt to unmarshal as an array containing a timestamp and data
			var logArray []interface{}
			if err := json.Unmarshal(rawEntry, &logArray); err == nil && len(logArray) == 2 {
				timestamp, ok := logArray[len(logArray)-1].(string)
				if !ok {
					log.Printf("Error: Expected timestamp as first element, got: %v", logArray[len(logArray)-1])
					continue
				}

				validatorDataBytes, err := json.Marshal(logArray[len(logArray)-1])
				if err != nil {
					log.Printf("Error marshaling validator data: %s", err)
					continue
				}

				var validatorData ValidatorData
				if err := json.Unmarshal(validatorDataBytes, &validatorData); err != nil {
					log.Printf("Error decoding validator data: %s", err)
					continue
				}

				logEntry := LogArrayEntry{
					Timestamp: timestamp,
					Validator: validatorData,
				}

				// Process the log entry as usual
				processLogEntry(logEntry, slackClient, config)
				continue
			}

			log.Printf("Error: Could not unmarshal JSON line as expected array")
		}

		file.Close()
		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}

func processLogEntry(logEntry LogArrayEntry, slackClient *slack.Client, config Config) {
	log.Printf("Timestamp: %s\n", logEntry.Timestamp)
	if status, found := logEntry.Validator.HeartbeatStatuses[config.ValidatorAddress]; found {
		if status.SinceLastSuccess > 40 || (status.LastAckDuration != nil && *status.LastAckDuration > 0.02) || status.LastAckDuration == nil {
			alertMessage := fmt.Sprintf("Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, status.SinceLastSuccess, status.LastAckDuration)
			sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
			sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
		}
	} else if status.SinceLastSuccess <= 0 || *status.LastAckDuration <= 0 {
		alertMessage := fmt.Sprintf("HyperLiq Heartbeat status not found for validator %s", config.ValidatorAddress)
		sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
		sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
	} else if _, isString1 := status.SinceLastSuccess.(string); isString1 || (status.LastAckDuration != nil && _, isString2 := (*status.LastAckDuration).(string); isString2){
		alertMessage := fmt.Sprintf("HyperLiq Heartbeat status for validator %s is not a number", config.ValidatorAddress)
		sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
		sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
	}
}

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

	latestDir := dirs[0]
	for _, dir := range dirs {
		if dir > latestDir {
			latestDir = dir
		}
	}

	return fmt.Sprintf("%s/%s", basePath, latestDir), nil
}

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

	latestFile := files[0]
	for _, file := range files {
		if file > latestFile {
			latestFile = file
		}
	}

	return fmt.Sprintf("%s/%s", dirPath, latestFile), nil
}
