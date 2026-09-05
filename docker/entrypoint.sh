#!/usr/bin/env bash
set -euo pipefail

# Start llama-server with the PLAN.md profile, wait until it is healthy, then
# run the agent once with the container command as the prompt.

LLAMA_SERVER_HOST="${LLAMA_SERVER_HOST:-127.0.0.1}"
LLAMA_SERVER_PORT="${LLAMA_SERVER_PORT:-8080}"
LLAMA_MODEL_FILE="${LLAMA_MODEL_FILE:-qwen2.5-1.5b-instruct-q4_k_m.gguf}"
LLAMA_MODEL_PATH="${LLAMA_MODEL_PATH:-/models/${LLAMA_MODEL_FILE}}"
LLAMA_MODEL_NAME="${LLAMA_MODEL_NAME:-local-qwen2.5}"
LLAMA_CTX_SIZE="${LLAMA_CTX_SIZE:-4096}"
LLAMA_BATCH_SIZE="${LLAMA_BATCH_SIZE:-128}"
LLAMA_THREADS="${LLAMA_THREADS:-0}"
LLAMA_N_GPU_LAYERS="${LLAMA_N_GPU_LAYERS:-0}"
LLAMA_STARTUP_TIMEOUT="${LLAMA_STARTUP_TIMEOUT:-180}"
LLAMA_EXTRA_ARGS="${LLAMA_EXTRA_ARGS:-}"
AGENT_WORKSPACE="${AGENT_WORKSPACE:-/workspace}"
AGENT_MCP_COMMAND="${AGENT_MCP_COMMAND:-/usr/local/bin/coreutils-mcp}"

if [[ $# -eq 0 ]]; then
  echo "usage: <image> \"<prompt>\"" >&2
  exit 2
fi

if [[ ! -f "$LLAMA_MODEL_PATH" ]]; then
  echo "model file not found: $LLAMA_MODEL_PATH" >&2
  echo "set LLAMA_MODEL_PATH or mount /models with ${LLAMA_MODEL_FILE}" >&2
  exit 1
fi

mkdir -p "$AGENT_WORKSPACE"

if [[ "$LLAMA_THREADS" == "0" ]]; then
  LLAMA_THREADS="$(nproc)"
fi

llama_args=(
  --jinja
  --host "$LLAMA_SERVER_HOST"
  --port "$LLAMA_SERVER_PORT"
  --model "$LLAMA_MODEL_PATH"
  --alias "$LLAMA_MODEL_NAME"
  --ctx-size "$LLAMA_CTX_SIZE"
  --batch-size "$LLAMA_BATCH_SIZE"
  --parallel 1
  --threads "$LLAMA_THREADS"
  --threads-batch "$LLAMA_THREADS"
  --n-gpu-layers "$LLAMA_N_GPU_LAYERS"
)

if [[ -n "$LLAMA_EXTRA_ARGS" ]]; then
  read -r -a extra_args <<< "$LLAMA_EXTRA_ARGS"
  llama_args+=("${extra_args[@]}")
fi

# llama-server diagnostics must never contaminate the agent's stdout answer.
/opt/llama/llama-server "${llama_args[@]}" >&2 &
llama_pid=$!

shutdown() {
  kill -TERM "$llama_pid" 2>/dev/null || true
}
trap shutdown INT TERM EXIT

deadline=$((SECONDS + LLAMA_STARTUP_TIMEOUT))
while true; do
  if curl -fsS "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/health" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$llama_pid" 2>/dev/null; then
    echo "llama-server exited before becoming ready" >&2
    exit 1
  fi
  if (( SECONDS >= deadline )); then
    echo "llama-server did not become ready within ${LLAMA_STARTUP_TIMEOUT}s" >&2
    exit 1
  fi
  sleep 1
done

set +e
/usr/local/bin/agent \
  --llama-url "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}" \
  --model "$LLAMA_MODEL_NAME" \
  --mcp-command "$AGENT_MCP_COMMAND" \
  --workspace "$AGENT_WORKSPACE" \
  "$@"
status=$?
set -e

exit "$status"
