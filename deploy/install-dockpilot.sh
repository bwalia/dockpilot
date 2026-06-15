#!/usr/bin/env bash
#
# install-dockpilot.sh — install/refresh DockPilot as a systemd service on a host.
#
# Designed to run ON the target machine (e.g. from a self-hosted GitHub Actions
# runner). Idempotent: bootstraps the unit + env/auth files on first run and just
# swaps the binary + restarts on subsequent runs.
#
# Usage: install-dockpilot.sh <env-name> <path-to-built-binary>
#   env-name   informational label baked into the unit description (int|test|...)
#   binary     path to the freshly built linux/amd64 dockpilot binary
#
set -euo pipefail

ENV_NAME="${1:?usage: install-dockpilot.sh <env-name> <binary>}"
BIN_SRC="${2:?usage: install-dockpilot.sh <env-name> <binary>}"
PORT="${DOCKPILOT_PORT:-8090}"

APP_USER="$(id -un)"
APP_HOME="$HOME"
# Install to a standard system path to avoid colliding with a $HOME/dockpilot
# directory (e.g. a source checkout) that exists on some hosts.
BIN_DST="/usr/local/bin/dockpilot"
ENV_FILE="$APP_HOME/dockpilot.env"
AUTH_FILE="$APP_HOME/dockpilot.auth"
UNIT="/etc/systemd/system/dockpilot.service"

echo "==> Installing DockPilot ($ENV_NAME) as user '$APP_USER' to $BIN_DST"
test -f "$BIN_SRC" || { echo "ERROR: binary not found at $BIN_SRC"; exit 1; }

# 1. Stop any running instance (systemd-managed or a stray manual process) so the
#    port is free before we (re)start. pkill -x matches the exact process name, so
#    it never touches the actions-runner ('runsvc.sh' / 'Runner.Listener').
sudo systemctl stop dockpilot 2>/dev/null || true
sudo pkill -x dockpilot 2>/dev/null || true
sudo pkill -x dockpilot-linux 2>/dev/null || true
sleep 1

# 2. Install the new binary (sudo: /usr/local/bin), keeping a rollback copy.
[ -f "$BIN_DST" ] && sudo cp -f "$BIN_DST" "${BIN_DST}-prev" || true
sudo install -m 0755 "$BIN_SRC" "$BIN_DST"

# 3. Seed env file on first install (kept across deploys afterwards).
if [ ! -f "$ENV_FILE" ]; then
  echo "==> Seeding $ENV_FILE"
  cat > "$ENV_FILE" <<EOF
AUTH_FILE=$AUTH_FILE
ADDR=:$PORT
OLLAMA_BASE_URL=http://192.168.1.177:11434/v1
OLLAMA_MODEL=llama3
EOF
fi

# 4. Seed a default auth file on first install. CHANGE THIS on real hosts.
if [ ! -f "$AUTH_FILE" ]; then
  echo "==> Seeding default $AUTH_FILE (change the credentials!)"
  echo "admin:DockPilot123!" > "$AUTH_FILE"
  chmod 600 "$AUTH_FILE"
fi

# 5. Write/refresh the systemd unit. Group is intentionally omitted: with User=
#    set, systemd applies the user's supplementary groups (incl. 'docker') from
#    the system db, so Docker socket access works without assuming the group name.
echo "==> Writing $UNIT"
sudo tee "$UNIT" >/dev/null <<EOF
[Unit]
Description=DockPilot - Docker Cockpit ($ENV_NAME)
Documentation=https://github.com/bwalia/dockpilot
After=network-online.target docker.service
Wants=network-online.target docker.service

[Service]
Type=simple
User=$APP_USER
WorkingDirectory=$APP_HOME
EnvironmentFile=$ENV_FILE
Environment=PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
ExecStart=$BIN_DST
Restart=always
RestartSec=3
StartLimitIntervalSec=0

[Install]
WantedBy=multi-user.target
EOF

# 6. Reload, enable for boot, and (re)start.
sudo systemctl daemon-reload
sudo systemctl enable dockpilot >/dev/null 2>&1 || true
sudo systemctl restart dockpilot
sleep 3

echo "==> is-active: $(systemctl is-active dockpilot)  is-enabled: $(systemctl is-enabled dockpilot 2>/dev/null)  PID: $(systemctl show -p MainPID --value dockpilot)"
systemctl is-active --quiet dockpilot || { echo "ERROR: service not active"; sudo journalctl -u dockpilot --no-pager -n 30; exit 1; }
echo "==> DockPilot ($ENV_NAME) installed and running on :$PORT"
