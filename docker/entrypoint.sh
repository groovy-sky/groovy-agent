#!/usr/bin/env bash
set -euo pipefail

# Save the container's original stdin before any subshell or redirection may
# replace fd 0 (e.g. the curl health-check loop).  The agent REPL needs it.
exec 3<&0

LLAMA_SERVER_HOST="${LLAMA_SERVER_HOST:-127.0.0.1}"
LLAMA_SERVER_PORT="${LLAMA_SERVER_PORT:-8080}"
LLAMA_MODEL_FILE="${LLAMA_MODEL_FILE:-Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf}"
LLAMA_MODEL_PATH="${LLAMA_MODEL_PATH:-/models/${LLAMA_MODEL_FILE}}"
LLAMA_MODEL_NAME="${LLAMA_MODEL_NAME:-Qwen2.5-Coder-7B-Instruct-Q4_K_M}"
LLAMA_CTX_SIZE="${LLAMA_CTX_SIZE:-8192}"
LLAMA_THREADS="${LLAMA_THREADS:-0}"
LLAMA_N_GPU_LAYERS="${LLAMA_N_GPU_LAYERS:-0}"
LLAMA_STARTUP_TIMEOUT="${LLAMA_STARTUP_TIMEOUT:-180}"
LLAMA_EXTRA_ARGS="${LLAMA_EXTRA_ARGS:-}"

if [[ ! -f "$LLAMA_MODEL_PATH" ]]; then
  echo "model file not found: $LLAMA_MODEL_PATH" >&2
  echo "set LLAMA_MODEL_PATH or mount /models with ${LLAMA_MODEL_FILE}" >&2
  exit 1
fi

if [[ "$LLAMA_THREADS" == "0" ]]; then
  LLAMA_THREADS="$(nproc)"
fi

llama_args=(
  --host "$LLAMA_SERVER_HOST"
  --port "$LLAMA_SERVER_PORT"
  --model "$LLAMA_MODEL_PATH"
  --alias "$LLAMA_MODEL_NAME"
  --ctx-size "$LLAMA_CTX_SIZE"
  --threads "$LLAMA_THREADS"
  --n-gpu-layers "$LLAMA_N_GPU_LAYERS"
)

if [[ -n "$LLAMA_EXTRA_ARGS" ]]; then
  read -r -a extra_args <<< "$LLAMA_EXTRA_ARGS"
  llama_args+=("${extra_args[@]}")
fi

/opt/llama/llama-server "${llama_args[@]}" &
llama_pid=$!
agent_pid=""

shutdown() {
  kill -TERM "$llama_pid" 2>/dev/null || true
  if [[ -n "$agent_pid" ]]; then
    kill -TERM "$agent_pid" 2>/dev/null || true
  fi
}
trap shutdown INT TERM

deadline=$((SECONDS + LLAMA_STARTUP_TIMEOUT))
while true; do
  if curl -fsS "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/health" >/dev/null 2>&1 || \
     curl -fsS "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/v1/models" >/dev/null 2>&1; then
    break
  fi
  if ! kill -0 "$llama_pid" 2>/dev/null; then
    wait "$llama_pid" || true
    echo "llama-server exited before becoming ready" >&2
    exit 1
  fi
  if (( SECONDS >= deadline )); then
    echo "llama-server did not become ready within ${LLAMA_STARTUP_TIMEOUT}s" >&2
    kill -TERM "$llama_pid" 2>/dev/null || true
    wait "$llama_pid" || true
    exit 1
  fi
  sleep 1
done

export OPENAI_BASE_URL="${OPENAI_BASE_URL:-http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}/v1}"
export OPENAI_MODEL="${OPENAI_MODEL:-$LLAMA_MODEL_NAME}"
export OPENAI_API_KEY="${OPENAI_API_KEY:-local-llama}"

# Redirect the saved original stdin (fd 3) explicitly so the REPL keeps the
# container's interactive stdin even in this non-interactive script context
# (background jobs otherwise get /dev/null as stdin).
/usr/local/bin/groovy-agent agent "$@" <&3 &
agent_pid=$!

while true; do
  if ! kill -0 "$llama_pid" 2>/dev/null; then
    wait "$llama_pid" || true
    kill -TERM "$agent_pid" 2>/dev/null || true
    wait "$agent_pid" || true
    echo "llama-server exited unexpectedly" >&2
    exit 1
  fi
  if ! kill -0 "$agent_pid" 2>/dev/null; then
    set +e
    wait "$agent_pid"
    status=$?
    set -e
    kill -TERM "$llama_pid" 2>/dev/null || true
    wait "$llama_pid" || true
    exit "$status"
  fi
  sleep 1
done
