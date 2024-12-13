package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/BurntSushi/toml"
)

// Config struct holds the necessary configuration values.
type Config struct {
	URL             string `toml:"URL"`
	SlackWebhookURL string `toml:"SlackWebhookURL"`
	CheckInterval   int    `toml:"CheckInterval"`
	HlVisorService  string `toml:"HlVisorService"`
	HlVisorPath     string `toml:"HlVisorPath"`
	HlNodePath      string `toml:"HlNodePath"`
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
		log.Printf("Last-Modified: %s", lastModified)
		message := fmt.Sprintf(":red_circle: The file at %s has been updated. New Last-Modified: %s", url, modified)
		log.Println(message)
		sendSlackAlert(config.SlackWebhookURL, message)
		executeUpdateWithChildProcessManagement()
	}

	lastModified = modified
}

// executeUpdateWithChildProcessManagement handles the hl-visor and hl-node update safely.
func executeUpdateWithChildProcessManagement() {
	log.Println("Starting update process...")

	// Step 1: Stop hl-visor and ensure hl-node is properly terminated
	if err := stopHlvisorWithChildProcess(); err != nil {
		log.Printf("Failed to stop hl-visor and its child process: %v", err)
		return
	}

	// Step 2: Download the new hl-visor binary
	tempBinaryPath := config.HlVisorPath + "-new"
	if err := downloadBinary(tempBinaryPath); err != nil {
		log.Printf("Failed to download new hl-visor binary: %v", err)
		return
	}

	// Step 3: Validate and replace the binary
	if err := validateAndReplaceBinary(tempBinaryPath); err != nil {
		log.Printf("Failed to validate or replace hl-visor binary: %v", err)
		return
	}

	// Step 4: Restart hl-visor and ensure hl-node restarts correctly
	if err := restartHlvisorAndCheckChildProcess(); err != nil {
		log.Printf("Failed to restart hl-visor or its child process: %v", err)
		return
	}

	log.Println("Update completed successfully.")
	// Step 5: Send Slack alert with the updated binary timestamps
	if err := sendUpdateConfirmationSlackAlert(); err != nil {
		log.Printf("Failed to send update confirmation Slack alert: %v", err)
	}
}

// sendUpdateConfirmationSlackAlert sends a Slack alert with the updated binary timestamps.
func sendUpdateConfirmationSlackAlert() error {
	// Execute ls -al hl-visor
	cmdVisor := exec.Command("/bin/sh", "-c", fmt.Sprintf("ls -al %s", config.HlVisorPath))
	visorOutput, err := cmdVisor.CombinedOutput()
	if err != nil {
		log.Printf("Failed to execute ls command for hl-visor: %v, output: %s", err, visorOutput)
		return err
	}

	// Execute ls -al hl-node
	cmdNode := exec.Command("/bin/sh", "-c", fmt.Sprintf("ls -al %s", config.HlNodePath))
	nodeOutput, err := cmdNode.CombinedOutput()
	if err != nil {
		log.Printf("Failed to execute ls command for hl-node: %v, output: %s", err, nodeOutput)
		return err
	}

	// Construct the Slack message
	message := fmt.Sprintf(
		":large_green_circle: Update completed successfully.\n\nUpdated binary timestamps:\n\nhl-visor:\n%s\n\nhl-node:\n%s",
		string(visorOutput),
		string(nodeOutput),
	)

	// Send the Slack alert
	sendSlackAlert(config.SlackWebhookURL, message)
	return nil
}

func stopHlvisorWithChildProcess() error {
	cmdStop := exec.Command("/bin/sh", "-c", fmt.Sprintf("sudo service %s stop", config.HlVisorService))
	if output, err := cmdStop.CombinedOutput(); err != nil {
		log.Printf("Failed to stop hl-visor: %v, output: %s", err, output)
		return err
	}
	log.Println("hl-visor stopped successfully.")

	if err := waitForProcessTermination("hl-node", 10*time.Second); err != nil {
		log.Printf("hl-node did not terminate gracefully: %v", err)
		return err
	}
	log.Println("hl-node terminated successfully.")
	return nil
}

func downloadBinary(path string) error {
	cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("curl https://binaries.hyperliquid.xyz/Testnet/hl-visor > %s", path))
	if output, err := cmd.CombinedOutput(); err != nil {
		log.Printf("Failed to download binary: %v, output: %s", err, output)
		return err
	}
	if err := os.Chmod(path, 0775); err != nil {
		log.Printf("Failed to set executable permissions: %v", err)
		return err
	}
	log.Println("Binary downloaded successfully.")
	return nil
}

func validateAndReplaceBinary(tempBinaryPath string) error {
	var output []byte

	// Step 1: Validate the binary
	cmdValidate := exec.Command("/bin/sh", "-c", fmt.Sprintf("%s --version", tempBinaryPath))
	output, err := cmdValidate.CombinedOutput()
	if err != nil {
		log.Printf("Binary validation failed: %v, output: %s", err, string(output))
		return err
	}
	log.Printf("Binary validated: %s", string(output))

	// Step 2: Replace the binary
	cmdReplace := exec.Command("/bin/sh", "-c", fmt.Sprintf("mv %s %s", tempBinaryPath, config.HlVisorPath))
	output, err = cmdReplace.CombinedOutput()
	if err != nil {
		log.Printf("Failed to replace binary: %v, output: %s", err, string(output))
		return err
	}
	log.Println("Binary replaced successfully.")
	return nil
}

func restartHlvisorAndCheckChildProcess() error {
	cmdRestart := exec.Command("/bin/sh", "-c", fmt.Sprintf("sudo service %s restart", config.HlVisorService))
	if output, err := cmdRestart.CombinedOutput(); err != nil {
		log.Printf("Failed to restart hl-visor: %v, output: %s", err, output)
		return err
	}
	log.Println("hl-visor restarted successfully.")

	if err := waitForProcess("hl-node", 10*time.Second); err != nil {
		log.Printf("hl-node did not start properly: %v", err)
		return err
	}
	log.Println("hl-node restarted successfully.")
	return nil
}

func waitForProcess(processName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("pgrep %s", processName))
		if output, err := cmd.CombinedOutput(); err == nil && len(output) > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process %s did not start within the timeout", processName)
}

func waitForProcessTermination(processName string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		cmd := exec.Command("/bin/sh", "-c", fmt.Sprintf("pgrep %s", processName))
		if output, err := cmd.CombinedOutput(); err != nil || len(output) == 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("process %s did not terminate within the timeout", processName)
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
