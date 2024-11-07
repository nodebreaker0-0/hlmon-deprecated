package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
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

// Function to send a Slack alert
func sendSlackAlert(config Config, message string) {
	api := slack.New(config.SlackToken)
	_, _, err := api.PostMessage(
		config.SlackChannel,
		slack.MsgOptionText(message, false),
	)
	if err != nil {
		fmt.Printf("Slack API Error: %s\n", err)
	}
}

// Function to send a PagerDuty alert
func sendPagerDutyAlert(config Config, description string) {
	event := pagerduty.V2Event{
		RoutingKey: config.PagerDutyAPIKey,
		Action:     "trigger",
		Payload: &pagerduty.V2Payload{
			Summary:   description,
			Source:    "validator-monitoring-script",
			Severity:  "critical",
			Component: "Validator Monitoring",
		},
	}
	_, err := pagerduty.ManageEvent(event)
	if err != nil {
		fmt.Printf("PagerDuty API Error: %s\n", err)
	}
}

func main() {
	// Load configuration from TOML file
	var config Config
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		fmt.Printf("Error loading configuration: %s\n", err)
		return
	}

	// Find the latest log file
	latestLogFile, err := findLatestLogFile(config.BasePath)
	if err != nil {
		log.Fatalf("Failed to find latest log file: %v", err)
	}

	fmt.Printf("Reading latest log file: %s\n", latestLogFile)

	for {
		file, err := os.Open(latestLogFile)
		if err != nil {
			fmt.Printf("Error opening log file: %s\n", err)
			continue
		}

		bytes, err := ioutil.ReadAll(file)
		if err != nil {
			fmt.Printf("Error reading log file: %s\n", err)
			file.Close()
			continue
		}
		file.Close()

		var data []interface{}
		err = json.Unmarshal(bytes, &data)
		if err != nil {
			fmt.Printf("Error parsing JSON: %s\n", err)
			continue
		}

		if len(data) == 2 {
			if heartbeatStatuses, ok := data[1].(map[string]interface{})["heartbeat_statuses"].(map[string]interface{}); ok {
				if status, found := heartbeatStatuses[config.ValidatorAddress]; found {
					statusMap := status.(map[string]interface{})
					sinceLastSuccess, ok1 := statusMap["since_last_success"].(float64)
					lastAckDuration, ok2 := statusMap["last_ack_duration"].(float64)

					// Check the thresholds and send alerts if necessary
					if !ok1 || !ok2 || sinceLastSuccess > 40 || lastAckDuration > 0.02 {
						alertMessage := fmt.Sprintf("Alert for validator %s:\nsince_last_success = %v, last_ack_duration = %v", config.ValidatorAddress, sinceLastSuccess, lastAckDuration)
						sendSlackAlert(config, alertMessage)
						sendPagerDutyAlert(config, alertMessage)
					}
				}
			}
		}

		// Wait for 10 seconds before reading the file again
		time.Sleep(10 * time.Second)
	}
}

func findLatestLogFile(basePath string) (string, error) {
	// Find the latest date directory
	latestDateDir, err := findLatestDir(basePath)
	if err != nil {
		return "", fmt.Errorf("Failed to find latest date directory: %w", err)
	}

	// Find the latest hour directory
	latestHourDir, err := findLatestDir(latestDateDir)
	if err != nil {
		return "", fmt.Errorf("Failed to find latest hour directory: %w", err)
	}

	// Find the latest log file
	latestLogFile, err := findLatestFile(latestHourDir)
	if err != nil {
		return "", fmt.Errorf("Failed to find latest log file: %w", err)
	}

	return latestLogFile, nil
}

func findLatestDir(basePath string) (string, error) {
	entries, err := ioutil.ReadDir(basePath)
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
		return "", fmt.Errorf("No directories found in %s", basePath)
	}

	// Sort directory names in descending order to select the latest directory
	sort.Slice(dirs, func(i, j int) bool {
		iNum, _ := strconv.Atoi(dirs[i])
		jNum, _ := strconv.Atoi(dirs[j])
		return iNum > jNum
	})

	return filepath.Join(basePath, dirs[0]), nil
}

func findLatestFile(dirPath string) (string, error) {
	entries, err := ioutil.ReadDir(dirPath)
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
		return "", fmt.Errorf("No files found in %s", dirPath)
	}

	// Interpret file names as numbers and sort in descending order
	sort.Slice(files, func(i, j int) bool {
		iNum, _ := strconv.Atoi(files[i])
		jNum, _ := strconv.Atoi(files[j])
		return iNum > jNum
	})

	return filepath.Join(dirPath, files[0]), nil
}
