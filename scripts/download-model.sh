#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${MODEL_DIR:-${ROOT_DIR}/artifacts/models}"
MODEL_FILENAME="${MODEL_FILENAME:-Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf}"
MODEL_URL="${MODEL_URL:-https://huggingface.co/unsloth/Qwen2.5-Coder-7B-Instruct-128K-GGUF/resolve/main/${MODEL_FILENAME}}"

mkdir -p "$MODEL_DIR"
tmp_file="${MODEL_DIR}/${MODEL_FILENAME}.part"
final_file="${MODEL_DIR}/${MODEL_FILENAME}"

curl_args=(
  -fL
  --retry 5
  --retry-delay 2
  --retry-all-errors
  "$MODEL_URL"
  -o
  "$tmp_file"
)

if [[ -n "${HF_TOKEN:-}" ]]; then
  curl_args=(
    -fL
    --retry 5
    --retry-delay 2
    --retry-all-errors
    --oauth2-bearer "$HF_TOKEN"
    "$MODEL_URL"
    -o
    "$tmp_file"
  )
fi

curl "${curl_args[@]}"
mv "$tmp_file" "$final_file"

echo "model downloaded: $final_file"
