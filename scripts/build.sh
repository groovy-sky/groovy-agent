#!/usr/bin/env bash
# Build both executables into ./bin.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUTPUT_DIR="${OUTPUT_DIR:-${ROOT_DIR}/bin}"

mkdir -p "$OUTPUT_DIR"
cd "$ROOT_DIR"

go build -trimpath -o "${OUTPUT_DIR}/agent" ./cmd/agent
go build -trimpath -o "${OUTPUT_DIR}/coreutils-mcp" ./cmd/coreutils-mcp

echo "built: ${OUTPUT_DIR}/agent"
echo "built: ${OUTPUT_DIR}/coreutils-mcp"
