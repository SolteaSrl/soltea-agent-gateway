#!/usr/bin/env bash
# Cross-compila l'agent-runner per Windows (amd64) da qualsiasi host con Go.
set -euo pipefail

cd "$(dirname "$0")/.."
OUT="${1:-dist/agent-runner.exe}"
mkdir -p "$(dirname "$OUT")"

# Versione iniettata via ldflags. Default = quella dichiarata in internal/version,
# sovrascrivibile passando VERSION=x.y.z (allinearla al tag della release).
VERPKG="github.com/marcelloobertisolte-lab/soltea-agent-gateway/agent-runner/internal/version"
DEFAULT_VER="$(sed -n 's/.*Runner = "\(.*\)".*/\1/p' internal/version/version.go)"
VERSION="${VERSION:-$DEFAULT_VER}"

echo "Build -> $OUT (windows/amd64) runner v$VERSION"
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X ${VERPKG}.Runner=${VERSION}" -o "$OUT" .

echo "Fatto: $OUT (v$VERSION)"
ls -la "$OUT"
