# Validator Monitoring Script

## Overview

This script monitors a validator's health and alerts through Slack and PagerDuty if certain thresholds are exceeded. It's useful for keeping track of validator performance and ensuring timely actions to maintain network integrity.

## Features

- **Heartbeat Status Monitoring**: Tracks the status of validator heartbeats to ensure they are responding appropriately.
- **Slack Alerts**: Sends alerts to a Slack channel if a validator exceeds set thresholds.
- **PagerDuty Alerts**: Triggers PagerDuty incidents when critical thresholds are breached.

## Requirements

- Go (Golang) version 1.15 or higher.
- Configuration file (`config.toml`) with proper credentials for Slack Webhook and PagerDuty routing key.

## Configuration

To run the script, you need a configuration file (`config.toml`) with the following fields:

```toml
executeUnjail = false # do you want to automatically attempt to unjail and restart the hl-visor service?
slack_webhook_url = "<Your Slack Webhook URL>"
slack_enabled = false # alert in slack?
pagerduty_routing_key = "<Your PagerDuty Routing Key>"
pagerduty_enabled = false # run pagerduty alerts
base_path = "<Path to log files>"
validator_address = "<Validator Address>"
check_interval = 60  # Interval in seconds
alert_threshold_success = 300.0  # Threshold in seconds for since_last_success
alert_threshold_ack = 60.0  # Threshold in seconds for last_ack_duration
telegram_api_key = ""  # obtained through telegram @BotFather user
telegram_rx_chat_ids = [] # your userID which has run "/start" in the chat that the bot that was created in
telegram_enabled = false # whether to use this alert
```

## How to Run

1. Clone this repository.
2. Make sure Go is installed and set up on your system.
3. Create a `config.toml` file with the necessary fields.
4. Run the script with the command:
   ```sh
   go run main.go
   ```

## Functions and Their Purpose

- **sendTelegramAlert**: Sends an alert to a specified Telegram ChatId using a bot.
- **sendSlackAlert**: Sends an alert to a specified Slack channel.
- **sendPagerDutyAlert**: Sends an alert to PagerDuty using the routing key.
- **UnmarshalJSON** (for `ValidatorData`): Handles the custom unmarshalling of JSON data for heartbeat statuses.
- **findLatestLogFile**: Finds the latest log file in the specified directory to process.
- **findLatestDir** and **findLatestFile**: Helper functions that locate the most recent directory and log file, respectively.

## Example Workflow

1. The script finds the most recent log file in the specified directory.
2. It checks the log file for heartbeat status data.
3. If a validator's heartbeat status exceeds thresholds, alerts are sent to Slack and PagerDuty.

## Alerts

Alerts are generated under the following conditions:
- **since_last_success** exceeds `alert_threshold_success`.
- **last_ack_duration** exceeds `alert_threshold_ack` or is not available.

## Customization

- **Slack and PagerDuty Integration**: Replace the webhook URL and routing key in the `config.toml` file with your own.
- **Telegram Integration**: Create a Bot User by messaging the @BotFather Telegram Account, populate the token and the target chatID to `config.toml`.
- **Thresholds**: Adjust `alert_threshold_success` and `alert_threshold_ack` in the config file to suit your needs.

## License

This project is licensed under the MIT License.

## Disclaimer

This script is intended for educational purposes and for use in monitoring validators on a testnet or mainnet. Ensure that sensitive credentials are handled securely.
