#!/bin/bash
AWS_FIREWALL_NAME=sg-0xxxxxxxxxxxxxx

# Extract IPs from AWS CLI results
aws_ips=$(aws ec2 describe-security-groups --group-ids $AWS_FIREWALL_NAME \
  --query "SecurityGroups[*].IpPermissions" --output json | \
  jq -r '.[][] | select(.FromPort == 4001 and .ToPort == 4006) | .IpRanges[].CidrIp' | \
  sed 's#/32##')

# extract IPs from firewall_ips.json
firewall_ips=$(grep -oP '\["\K[^"]+' $HOME/hl/hyperliquid_data/firewall_ips.json | cut -d'"' -f1)

# Sort and deduplicate
sorted_aws_ips=$(echo "$aws_ips" | sort -u)
sorted_firewall_ips=$(echo "$firewall_ips" | sort -u)

# Compare
aws_only=$(comm -23 <(echo "$sorted_aws_ips") <(echo "$sorted_firewall_ips"))
firewall_only=$(comm -13 <(echo "$sorted_aws_ips") <(echo "$sorted_firewall_ips"))


# Create a message
if [[ -n "$aws_only" || -n "$firewall_only" ]]; then
    message=""

    if [[ -n "$aws_only" ]]; then
        message+="IPs that exist only in AWS-firewall:\n$aws_only\n"
    fi

    if [[ -n "$firewall_only" ]]; then
        message+="\nIPs that are only on the firewall_ips.json:\n$firewall_only\n"
    fi

    if [[ -n "$message" ]]; then
        $HOME/hl-node --chain Mainnet send-slack-alert "$message"
    fi
else
    echo "Both AWS and Firewall have the same IP. No notifications are sent."
fi