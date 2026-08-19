#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-go-core-mcp-agent:local}"
OUTPUT_PATH="${OUTPUT_PATH:-${ROOT_DIR}/output/go-core-mcp-agent.tar}"
DOWNLOAD_MODEL_AT_BUILD="${DOWNLOAD_MODEL_AT_BUILD:-0}"

mkdir -p "$(dirname "$OUTPUT_PATH")"

build_args=(
  --tag "$IMAGE_NAME"
  "$ROOT_DIR"
)

if [[ "$DOWNLOAD_MODEL_AT_BUILD" == "1" ]]; then
  build_args=(
    --build-arg DOWNLOAD_MODEL=1
    --build-arg MODEL_URL="${MODEL_URL:-https://huggingface.co/unsloth/Qwen2.5-Coder-7B-Instruct-128K-GGUF/resolve/main/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf}"
    "${build_args[@]}"
  )
  if [[ -n "${HF_TOKEN:-}" ]]; then
    build_args=(--secret id=hf_token,env=HF_TOKEN "${build_args[@]}")
  fi
fi

DOCKER_BUILDKIT="${DOCKER_BUILDKIT:-1}" docker build "${build_args[@]}"
docker save "$IMAGE_NAME" -o "$OUTPUT_PATH"

echo "docker image artifact saved to: $OUTPUT_PATH"
