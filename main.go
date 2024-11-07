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

// Config structure for TOML configuration file
type Config struct {
	SlackToken         string `toml:"slack_token"`
	SlackChannel       string `toml:"slack_channel"`
	PagerDutyAPIKey    string `toml:"pagerduty_api_key"`
	PagerDutyServiceID string `toml:"pagerduty_service_id"`
	BasePath           string `toml:"base_path"`
	ValidatorAddress   string `toml:"validator_address"`
	CheckInterval      int    `toml:"check_interval"`
}

// Validator structures
type ValidatorStake struct {
	Address string `json:"address"`
	Stake   int    `json:"stake"`
}

type HeartbeatStatus struct {
	SinceLastSuccess float64  `json:"since_last_success"`
	LastAckDuration  *float64 `json:"last_ack_duration"`
}

type DisconnectedValidator struct {
	ValidatorAddress string `json:"validator_address"`
	Disconnections   []struct {
		PeerAddress string `json:"peer_address"`
		Round       int    `json:"round"`
	} `json:"disconnections"`
}

type ValidatorData struct {
	HomeValidator              string                     `json:"home_validator"`
	Round                      int                        `json:"round"`
	CurrentStakes              []ValidatorStake           `json:"current_stakes"`
	CurrentJailedValidators    []string                   `json:"current_jailed_validators"`
	NextProposers              []string                   `json:"next_proposers"`
	ValidatorsMissingHeartbeat []string                   `json:"validators_missing_heartbeat"`
	DisconnectedValidators     []DisconnectedValidator    `json:"disconnected_validators"`
	HeartbeatStatuses          map[string]HeartbeatStatus `json:"heartbeat_statuses"`
}

type LogData struct {
	Timestamp      string        `json:"timestamp"`
	ValidatorEntry ValidatorData `json:"validator_data"`
}

// Function to send a Slack alert
func sendSlackAlert(api *slack.Client, channel, message string) {
	_, _, err := api.PostMessage(
		channel,
		slack.MsgOptionText(message, false),
	)
	if err != nil {
		log.Printf("Slack API Error: %s\n", err)
	}
}

// Function to send a PagerDuty alert
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
	// Load configuration from TOML file
	var config Config
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading configuration: %s\n", err)
	}

	// Initialize Slack client
	slackClient := slack.New(config.SlackToken)

	// Keep trying to find the latest log file in a loop
	var latestLogFile string
	for {
		var err error
		latestLogFile, err = findLatestLogFile(config.BasePath)
		if err != nil {
			log.Printf("Failed to find latest log file: %v", err)
			log.Printf("Retrying in 10 seconds...\n")
			time.Sleep(10 * time.Second)
			continue
		}
		break
	}
	log.Printf("Reading latest log file: %s\n", latestLogFile)

	for {
		data, err := os.ReadFile(latestLogFile)
		if err != nil {
			log.Printf("Error reading log file: %s\n", err)
			continue
		}

		var logEntries []LogData
		if err := json.Unmarshal(data, &logEntries); err != nil {
			log.Printf("Error parsing JSON: %s\n", err)
			continue
		}

		if len(logEntries) > 0 {
			// Parse the last entry in the log
			lastEntry := logEntries[len(logEntries)-1]

			log.Printf("Timestamp: %s\n", lastEntry.Timestamp)

			// Check the heartbeat status of the given validator
			if status, found := lastEntry.ValidatorEntry.HeartbeatStatuses[config.ValidatorAddress]; found {
				// Check the thresholds and send alerts if necessary
				if status.SinceLastSuccess > 40 || (status.LastAckDuration != nil && *status.LastAckDuration > 0.02) || status.LastAckDuration == nil {
					alertMessage := fmt.Sprintf("Alert for HyperLiq validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, status.SinceLastSuccess, status.LastAckDuration)
					sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
					sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
				}
			} else if status.SinceLastSuccess <= 0 || status.LastAckDuration == nil || *status.LastAckDuration <= 0 {
				alertMessage := fmt.Sprintf("HyperLiq Heartbeat status not found for validator %s", config.ValidatorAddress)
				sendSlackAlert(slackClient, config.SlackChannel, alertMessage)
				sendPagerDutyAlert(config.PagerDutyAPIKey, alertMessage)
			}
		}

		// Wait for 10 seconds before reading the file again
		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}

func findLatestLogFile(basePath string) (string, error) {
	// Find the latest date directory
	latestDateDir, err := findLatestDir(basePath)
	if err != nil {
		return "", fmt.Errorf("failed to find latest date directory: %w", err)
	}

	// Find the latest log file
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
	println(entries)
	var dirs []string
	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, entry.Name())
		}
	}
	println(dirs)
	if len(dirs) == 0 {
		return "", fmt.Errorf("no directories found in %s", basePath)
	}

	latestDir := dirs[0]
	for _, dir := range dirs {
		if dir > latestDir {
			latestDir = dir
		}
	}
	println(latestDir)
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
