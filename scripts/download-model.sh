#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${MODEL_DIR:-${ROOT_DIR}/artifacts/models}"
MODEL_FILENAME="${MODEL_FILENAME:-Phi-4-mini-instruct.Q8_0.gguf}"
MODEL_URL="${MODEL_URL:-https://huggingface.co/unsloth/Phi-4-mini-instruct-GGUF/resolve/main/${MODEL_FILENAME}}"

mkdir -p "$MODEL_DIR"
tmp_file="${MODEL_DIR}/${MODEL_FILENAME}.part"
final_file="${MODEL_DIR}/${MODEL_FILENAME}"

curl_args=(
  -fL
  --retry 5
  --retry-delay 2
  --retry-all-errors
)

if [[ -n "${HF_TOKEN:-}" ]]; then
  curl_args+=(
    --oauth2-bearer "$HF_TOKEN"
  )
fi

curl_args+=(
  "$MODEL_URL"
  -o
  "$tmp_file"
)

curl "${curl_args[@]}"
mv "$tmp_file" "$final_file"

echo "model downloaded: $final_file"
