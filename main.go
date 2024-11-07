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

type LogData struct {
	Timestamp      string        `json:"timestamp"`
	ValidatorEntry ValidatorData `json:"validator_data"`
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
			log.Printf("Raw JSON content: %s\n", string(rawEntry)) // Log the raw JSON content

			// Attempt to unmarshal as a single LogData object
			var logEntry LogData
			if err := json.Unmarshal(rawEntry, &logEntry); err == nil {
				// Successfully unmarshalled as a single object
				processLogEntry(logEntry, slackClient, config)
				continue
			}

			// If unmarshalling as a single object failed, try as an array of LogData
			var logEntries []LogData
			if err := json.Unmarshal(rawEntry, &logEntries); err == nil {
				// Successfully unmarshalled as an array
				for _, entry := range logEntries {
					processLogEntry(entry, slackClient, config)
				}
				continue
			}

			// If both attempts fail, log the error
			log.Printf("Error: Could not unmarshal JSON line as object or array")
		}

		file.Close()
		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}

// Function to process each log entry
func processLogEntry(logEntry LogData, slackClient *slack.Client, config Config) {
	log.Printf("Timestamp: %s\n", logEntry.Timestamp)
	if status, found := logEntry.ValidatorEntry.HeartbeatStatuses[config.ValidatorAddress]; found {
		if status.SinceLastSuccess > 40 || (status.LastAckDuration != nil && *status.LastAckDuration > 0.02) || status.LastAckDuration == nil {
			alertMessage := fmt.Sprintf("Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, status.SinceLastSuccess, status.LastAckDuration)
			sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
			sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
		}
	} else {
		alertMessage := fmt.Sprintf("HyperLiq Heartbeat status not found for validator %s", config.ValidatorAddress)
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
