# Firewall Check Script

This repository contains a script to compare IP addresses between an AWS security group and a local `firewall_ips.json` file, and send a Slack alert if there are discrepancies.

## File Structure

- `firewall_check.sh`: A script to compare IP addresses between an AWS security group and a local `firewall_ips.json` file, and send a Slack alert if there are discrepancies.

## Usage

### Prerequisites

- AWS CLI must be installed.
- `jq` must be installed.
- `hl-node` must be installed.

### Configuration

1. Set the `AWS_FIREWALL_NAME` environment variable. This variable represents the AWS security group ID.
    ```bash
    export AWS_FIREWALL_NAME=sg-0xxxxxxxxxxxxxx
    ```

2. Place the `firewall_ips.json` file in the `$HOME/hl/hyperliquid_data/` directory.

### Execution

Run the script to compare IP addresses between the AWS security group and the local `firewall_ips.json` file.
```bash
./firewall_check.sh