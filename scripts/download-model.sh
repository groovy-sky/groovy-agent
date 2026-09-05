#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODEL_DIR="${MODEL_DIR:-${ROOT_DIR}/artifacts/models}"
MODEL_FILENAME="${MODEL_FILENAME:-qwen2.5-1.5b-instruct-q4_k_m.gguf}"
MODEL_URL="${MODEL_URL:-https://huggingface.co/Qwen/Qwen2.5-1.5B-Instruct-GGUF/resolve/main/${MODEL_FILENAME}}"

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
