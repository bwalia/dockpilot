#!/usr/bin/env bash
# macOS counterpart to install-dockpilot.sh: builds DockPilot and runs it as a
# per-user LaunchAgent (org.dockpilot) that starts at login and restarts on exit.
# Env comes from ./dockpilot.env in the repo root, sourced at launch.
#
# Usage: deploy/install-launchd.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
LABEL="org.dockpilot"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"
GO="$(command -v go || echo /opt/homebrew/bin/go)"

[[ -f "$ROOT/dockpilot.env" ]] || { echo "missing $ROOT/dockpilot.env" >&2; exit 1; }

echo "==> Building"
(cd "$ROOT" && "$GO" build -ldflags='-linkmode external' -o dockpilot .)

mkdir -p "$HOME/Library/LaunchAgents"
cat > "$PLIST" <<PL
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$LABEL</string>
  <key>ProgramArguments</key>
  <array>
    <string>/bin/bash</string><string>-c</string>
    <string>set -a; . ./dockpilot.env; set +a; exec ./dockpilot</string>
  </array>
  <key>WorkingDirectory</key><string>$ROOT</string>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ThrottleInterval</key><integer>10</integer>
  <key>StandardOutPath</key><string>$ROOT/dockpilot.log</string>
  <key>StandardErrorPath</key><string>$ROOT/dockpilot.log</string>
</dict>
</plist>
PL

echo "==> (Re)loading $LABEL"
launchctl bootout "gui/$(id -u)/$LABEL" 2>/dev/null || true
launchctl bootstrap "gui/$(id -u)" "$PLIST"
launchctl enable "gui/$(id -u)/$LABEL"

for _ in {1..15}; do
  code=$(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8090/ || true)
  [[ "$code" != "000" ]] && { echo "==> DockPilot up on :8090 (HTTP $code)"; exit 0; }
  sleep 1
done
echo "DockPilot did not come up; last log lines:" >&2
tail -n 30 "$ROOT/dockpilot.log" >&2
exit 1
