#!/usr/bin/env bash
set -euo pipefail

# Save the container's original stdin on fd 3 before any subshell or
# redirection may replace fd 0 (e.g. the curl readiness loop), so the agent is
# started with the stdin the container was launched with.  The agent itself is
# a one-shot CLI (it takes the prompt from argv, not stdin); this only keeps
# stdin intact for the tools it spawns and for interactive `docker run -i`
# usage.
exec 3<&0

# Ensure the output directory exists so result JSON files can always be written.
mkdir -p "${AGENT_OUTPUT_DIR:-/output}"

LLAMA_SERVER_HOST="${LLAMA_SERVER_HOST:-127.0.0.1}"
LLAMA_SERVER_PORT="${LLAMA_SERVER_PORT:-8080}"
LLAMA_MODEL_FILE="${LLAMA_MODEL_FILE:-Phi-4-mini-instruct.Q8_0.gguf}"
LLAMA_MODEL_PATH="${LLAMA_MODEL_PATH:-/models/${LLAMA_MODEL_FILE}}"
LLAMA_MODEL_NAME="${LLAMA_MODEL_NAME:-Phi-4-mini-instruct}"
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

# `groovy-agent` is a one-shot CLI: it requires a positional prompt and exits
# with a usage error without one.  Mirror the Go flag package's parsing rules
# closely enough to tell whether the container command contains a positional
# prompt, so a no-argument `docker run` can serve the llama-server API instead
# of failing with that usage error.
#
# Every agent flag takes a value, so `-flag value` consumes the next argument
# unless the value is inlined as `-flag=value`.  `--` terminates flag parsing.
# The flag list below must be kept in sync with the flags registered in
# cmd/agent/main.go: an unlisted value-taking flag would make its value look
# like a positional prompt.

# The agent joins its positional arguments and trims them, so whitespace-only
# arguments are not a usable prompt either.
positional_is_prompt() {
  local joined="$*"
  [[ -n "${joined//[[:space:]]/}" ]]
}

has_positional_prompt() {
  while (( $# > 0 )); do
    case "$1" in
      --)
        shift
        positional_is_prompt "$@"
        return $?
        ;;
      -*=*)
        shift
        ;;
      -llama-url|--llama-url|-model|--model|-mcp-command|--mcp-command|-workspace|--workspace)
        shift
        if (( $# > 0 )); then
          shift
        fi
        ;;
      -*)
        # Unrecognized flag: deliberately select one-shot mode (rather than
        # reporting a prompt) so the agent parses the flag and reports the
        # error itself.
        return 0
        ;;
      *)
        positional_is_prompt "$@"
        return $?
        ;;
    esac
  done
  return 1
}

if has_positional_prompt "$@"; then
  agent_mode="oneshot"
else
  agent_mode="serve"
fi

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

if [[ "$agent_mode" == "serve" ]]; then
  # No positional prompt was supplied, so there is nothing for the one-shot
  # agent to do.  Keep llama-server running as an OpenAI-compatible API server
  # instead of exiting with the agent's missing-prompt usage error.
  echo "no prompt argument supplied: serving llama-server only" >&2
  echo "OpenAI-compatible API: ${OPENAI_BASE_URL} (model: ${OPENAI_MODEL})" >&2
  echo "publish it with 'docker run -p 8080:8080 -e LLAMA_SERVER_HOST=0.0.0.0 ...'" >&2
  echo "to run a one-shot agent request instead, append a prompt, e.g." >&2
  echo "  docker run --rm ... groovy-agent:local --workspace /output \"what is today's date?\"" >&2
  set +e
  wait "$llama_pid"
  status=$?
  # A trapped signal interrupts `wait` (status > 128) before llama-server has
  # actually exited; keep waiting for its real exit status after the trap has
  # forwarded the signal.
  while (( status > 128 )) && kill -0 "$llama_pid" 2>/dev/null; do
    wait "$llama_pid"
    status=$?
  done
  set -e
  exit "$status"
fi

agent_args=(
  --llama-url "http://${LLAMA_SERVER_HOST}:${LLAMA_SERVER_PORT}"
  --model "$LLAMA_MODEL_NAME"
  --mcp-command /usr/local/bin/coreutils-mcp
  "$@"
)

/usr/local/bin/groovy-agent "${agent_args[@]}" <&3 &
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
