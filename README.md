# Validator Monitoring Script

This project is a Go-based validator monitoring script that reads log files to monitor the health of blockchain validators and sends alerts to Slack and PagerDuty if certain thresholds are breached.

## Features
- Reads log files to extract validator metrics.
- Monitors specific metrics such as `since_last_success` and `last_ack_duration` for the validator.
- Sends alerts to Slack and PagerDuty when thresholds are exceeded.
- Supports reading configuration values from a TOML file.
- Automatically identifies the latest log files based on the directory structure.

## Prerequisites
- Go 1.16 or higher
- Slack App with a valid API token
- PagerDuty account with an API key

## Installation
1. Clone this repository:
   ```sh
   git clone https://github.com/yourusername/validator-monitoring.git
   cd validator-monitoring
   ```
2. Install the necessary dependencies using `go mod`:
   ```sh
   go mod tidy
   ```

## Configuration
Create a `config.toml` file in the root directory with the following content:

```toml
# Slack configuration
slack_token = "YOUR_SLACK_TOKEN"
slack_channel = "#alerts"

# PagerDuty configuration
pagerduty_api_key = "YOUR_PAGERDUTY_API_KEY"
pagerduty_service_id = "YOUR_PAGERDUTY_SERVICE_ID"

# Log file configuration
base_path = "/home/ubuntu/hl/data/node_logs/status/hourly"

# Validator address to monitor
validator_address = "0xef22f260eec3b7d1edebe53359f5ca584c18d5ac"
```
Replace the placeholder values with your actual credentials and configuration values.

## Usage
To run the script, use the following command:
```sh
go run main.go
```
The script will:
- Load the configuration from `config.toml`.
- Locate the latest log files based on directory names.
- Monitor the `since_last_success` and `last_ack_duration` metrics for the specified validator.
- Send alerts to Slack and PagerDuty when thresholds are breached.

## Alert Conditions
The script sends alerts if any of the following conditions are met:
- `since_last_success` exceeds 40 seconds.
- `last_ack_duration` exceeds 0.02 seconds.
- If either of these values is missing or invalid.

## Directory Structure
The script expects the logs to be stored in a directory structure like:
```
base_path/YYYYMMDD/HH/
```
Where `YYYYMMDD` represents the date and `HH` represents the hour. The script will automatically select the latest available log file.

## Dependencies
- [slack-go/slack](https://github.com/slack-go/slack): Used for sending Slack notifications.
- [PagerDuty/go-pagerduty](https://github.com/PagerDuty/go-pagerduty): Used for sending PagerDuty alerts.
- [BurntSushi/toml](https://github.com/BurntSushi/toml): Used for parsing the TOML configuration file.

## License
This project is licensed under the MIT License.

## Contributing
Pull requests are welcome. For major changes, please open an issue first to discuss what you would like to change.

## Contact
For any questions or issues, please reach out via GitHub or open an issue in the repository.

