#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-groovy-agent:local}"
OUTPUT_PATH="${OUTPUT_PATH:-${ROOT_DIR}/output/groovy-agent.tar}"
DOWNLOAD_MODEL_AT_BUILD="${DOWNLOAD_MODEL_AT_BUILD:-0}"

mkdir -p "$(dirname "$OUTPUT_PATH")"

build_args=(
  --tag "$IMAGE_NAME"
  "$ROOT_DIR"
)

if [[ "$DOWNLOAD_MODEL_AT_BUILD" == "1" ]]; then
  build_args=(
    --build-arg DOWNLOAD_MODEL=1
  --build-arg MODEL_URL="${MODEL_URL:-https://huggingface.co/unsloth/Phi-4-mini-instruct-GGUF/resolve/main/Phi-4-mini-instruct.Q8_0.gguf}"
    "${build_args[@]}"
  )
  if [[ -n "${HF_TOKEN:-}" ]]; then
    build_args=(--secret id=hf_token,env=HF_TOKEN "${build_args[@]}")
  fi
fi

DOCKER_BUILDKIT="${DOCKER_BUILDKIT:-1}" docker build "${build_args[@]}"
docker save "$IMAGE_NAME" -o "$OUTPUT_PATH"

echo "docker image artifact saved to: $OUTPUT_PATH"
