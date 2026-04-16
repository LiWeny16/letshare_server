#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
REPO_ROOT=$(cd -- "$SCRIPT_DIR/.." && pwd)
OUTPUT="$REPO_ROOT/letshare-server-2"

cd "$REPO_ROOT"

echo "[build] repo root: $REPO_ROOT"
echo "[build] output: $OUTPUT"

go build -o "$OUTPUT" cmd/server/main.go

echo "[build] done"
