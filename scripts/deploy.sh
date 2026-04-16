#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
BINARY="$REPO_ROOT/letshare-server-2"
SERVICE_NAME="letshare.service"

cd "$REPO_ROOT"

"$SCRIPT_DIR/build.sh"

if [[ ! -f "$BINARY" ]]; then
  echo "[deploy] build failed: $BINARY not found" >&2
  exit 1
fi

echo "[deploy] restarting $SERVICE_NAME"
systemctl restart "$SERVICE_NAME"

echo "[deploy] status"
systemctl status "$SERVICE_NAME" --no-pager

echo "[deploy] logs: journalctl -u $SERVICE_NAME -n 50 --no-pager"
