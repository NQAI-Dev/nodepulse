#!/bin/sh
set -e

SERVER="${NODEPULSE_SERVER:-http://127.0.0.1:8080}"
NODE_ID="${NODEPULSE_NODE_ID:-$(hostname)}"

echo "==> Installing NodePulse Edge Agent..."
mkdir -p /opt/nodepulse /etc/nodepulse

cat << ENV > /etc/nodepulse/agent.env
NODEPULSE_NODE_ID=${NODE_ID}
NODEPULSE_SERVER=${SERVER}/api/v1/ingest
ENV

cat << UNIT > /etc/systemd/system/nodepulse-agent.service
[Unit]
Description=NodePulse Edge Agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
EnvironmentFile=/etc/nodepulse/agent.env
ExecStart=/opt/nodepulse/nodepulse-agent -node \${NODEPULSE_NODE_ID} -server \${NODEPULSE_SERVER}
Restart=always
RestartSec=10
LimitNOFILE=65535

[Install]
WantedBy=multi-user.target
UNIT

echo "==> Service configured. Copy binary to /opt/nodepulse/nodepulse-agent and start service:"
echo "    systemctl daemon-reload && systemctl enable --now nodepulse-agent"
