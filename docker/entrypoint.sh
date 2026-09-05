#!/usr/bin/env bash
set -euo pipefail

# Save the container's original stdin before any subshell or redirection may
# replace fd 0 (e.g. the curl health-check loop).  The agent REPL needs it.
exec 3<&0

# Ensure the output directory exists so result JSON files can always be written.
mkdir -p "${AGENT_OUTPUT_DIR:-/output}"

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

# Sampling/generation guardrails. These defaults exist to prevent small
# quantized models from spiraling into degenerate token repetition (e.g. an
# assistant turn that repeats the same "Final Answer" paragraph until the
# tool/time budget is exhausted, without ever issuing the required tool
# call). They are applied unconditionally as llama-server startup flags, so
# they take effect even for requests that do not set sampling params
# themselves, and can still be overridden per-request by the client or
# widened/replaced via LLAMA_EXTRA_ARGS below.
#
# - LLAMA_REPEAT_PENALTY: penalize sampling tokens that already appeared
#   recently; the primary defense against repetition loops.
# - LLAMA_REPEAT_LAST_N: how many recent tokens the repeat penalty considers.
# - LLAMA_PREDICT_LIMIT: hard cap (in tokens) on a single generation, so a
#   degenerate loop is cut off quickly instead of running for the entire
#   request timeout. -1 (llama.cpp's "unbounded" default) is accepted to
#   opt back out.
LLAMA_REPEAT_PENALTY="${LLAMA_REPEAT_PENALTY:-1.3}"
LLAMA_REPEAT_LAST_N="${LLAMA_REPEAT_LAST_N:-256}"
LLAMA_PREDICT_LIMIT="${LLAMA_PREDICT_LIMIT:-1024}"

if [[ ! -f "$LLAMA_MODEL_PATH" ]]; then
  echo "model file not found: $LLAMA_MODEL_PATH" >&2
  echo "set LLAMA_MODEL_PATH or mount /models with ${LLAMA_MODEL_FILE}" >&2
  exit 1
fi

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
  --threads "$LLAMA_THREADS"
  --n-gpu-layers "$LLAMA_N_GPU_LAYERS"
  --repeat-penalty "$LLAMA_REPEAT_PENALTY"
  --repeat-last-n "$LLAMA_REPEAT_LAST_N"
  --n-predict "$LLAMA_PREDICT_LIMIT"
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

# Forward subcommand: if the first argument is "agent" or "run", pass all args
# directly. Otherwise prepend "agent" so bare extra flags (like --workspace)
# still reach agent mode.
if [[ "${1:-}" == "agent" || "${1:-}" == "run" ]]; then
  /usr/local/bin/groovy-agent "$@" <&3 &
else
  /usr/local/bin/groovy-agent agent "$@" <&3 &
fi
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
