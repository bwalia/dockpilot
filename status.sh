#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "== Dockpilot Status =="
echo

echo "[1/4] Processes"
if pgrep -fl dockpilot >/dev/null 2>&1; then
  pgrep -fl dockpilot
else
  echo "No dockpilot process found"
fi

echo

echo "[2/4] Listener :8090"
if lsof -nP -iTCP:8090 -sTCP:LISTEN >/dev/null 2>&1; then
  lsof -nP -iTCP:8090 -sTCP:LISTEN
else
  echo "Nothing listening on :8090"
fi

echo

echo "[3/4] HTTP check"
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8090/ || true)
echo "HTTP status: ${HTTP_CODE:-000}"

echo

echo "[4/4] Recent logs"
if [[ -f dockpilot.log ]]; then
  tail -n 50 dockpilot.log || true
else
  echo "dockpilot.log not found"
fi
