#!/usr/bin/env bash
set -euo pipefail

cd "$(dirname "$0")"

echo "[1/5] Stopping existing dockpilot processes..."
pkill dockpilot >/dev/null 2>&1 || true
pkill -f 'go run -ldflags=-linkmode=external \.' >/dev/null 2>&1 || true
sleep 1

echo "[2/5] Preparing launch (external linker mode)..."
go build -ldflags='-linkmode external' -o dockpilot .

echo "[3/5] Starting dockpilot..."
nohup go run -ldflags=-linkmode=external . > dockpilot.log 2>&1 < /dev/null &
PID=$!
sleep 1

echo "[4/5] Process check..."
if ps -p "$PID" >/dev/null 2>&1; then
  echo "dockpilot started with PID $PID"
else
  echo "dockpilot failed to start"
  tail -n 40 dockpilot.log || true
  exit 1
fi

echo "[5/5] Health check (expect 401 if auth is enabled)..."
HTTP_CODE="000"
for _ in {1..10}; do
  HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8090/ || true)
  if [[ "$HTTP_CODE" != "000" ]]; then
    break
  fi
  sleep 1
done
echo "HTTP status: ${HTTP_CODE:-000}"
if [[ "$HTTP_CODE" == "000" ]]; then
  echo "dockpilot did not become reachable on :8090"
  tail -n 60 dockpilot.log || true
  exit 1
fi

echo "Recent logs:"
tail -n 20 dockpilot.log || true
