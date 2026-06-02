#!/usr/bin/env bash
# Cross-compila l'agent-runner per Windows (amd64) da qualsiasi host con Go.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${1:-dist/agent-runner.exe}"
mkdir -p "$(dirname "$OUT")"

echo "Build -> $OUT (windows/amd64)"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$OUT" .

echo "Fatto: $OUT"
ls -la "$OUT"
