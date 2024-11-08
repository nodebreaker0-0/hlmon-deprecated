package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/BurntSushi/toml"
)

// Config struct holds the necessary configuration values.
type Config struct {
	URL             string `toml:"URL"`
	SlackWebhookURL string `toml:"SlackWebhookURL"`
	CheckInterval   int    `toml:"CheckInterval"`
}

var config Config
var lastModified string

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

// checkForUpdate checks if the Last-Modified header has changed.
func checkForUpdate(url string) {
	client := &http.Client{}
	req, err := http.NewRequest("HEAD", url, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		return
	}

	resp, err := client.Do(req)
	if err != nil {
		log.Printf("Error performing request: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("Non-200 response code: %d", resp.StatusCode)
		return
	}

	modified := resp.Header.Get("Last-Modified")
	if modified == "" {
		log.Println("Last-Modified header not present in response.")
		return
	}

	if lastModified != "" && lastModified != modified {
		message := fmt.Sprintf("The file at %s has been updated. New Last-Modified: %s", url, modified)
		log.Println(message)
		sendSlackAlert(config.SlackWebhookURL, message)
	}

	lastModified = modified
	log.Printf("Last-Modified: %s", lastModified)
}

func main() {
	if _, err := toml.DecodeFile("config.toml", &config); err != nil {
		log.Fatalf("Error loading configuration: %s", err)
	}

	for {
		checkForUpdate(config.URL)
		time.Sleep(time.Duration(config.CheckInterval) * time.Second)
	}
}
