#!/usr/bin/env bash
# Deterministic container-level smoke test for the runtime Docker/Podman image.
#
# This validates packaging/wiring regressions without requiring model
# inference or downloading the GGUF model at test runtime:
#
#   1. The compiled `/usr/local/bin/groovy-agent` binary is present in the
#      final image and its built-in `date` coreutil works.
#   2. `docker/entrypoint.sh` starts llama-server, waits for it to become
#      healthy, and forwards the container command to `groovy-agent`
#      correctly -- both when the first argument is already `run`/`agent`
#      and when it needs to be prepended with `agent`.
#
# Test 2 replaces `llama-server` and `groovy-agent` inside the container with
# small deterministic stubs (a Python HTTP server that answers /health, and a
# script that records argv) so no real LLM inference happens, no model
# download is required, and no llama-server is ever exposed outside the
# container (no `-p`/published ports are used).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-groovy-agent:smoke-test}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-docker}"

WORK_DIR="$(mktemp -d)"
cleanup() {
  status=$?
  # Best-effort: stop/remove any smoke-test container left running and drop
  # the scratch directory used for stub binaries and captured output.
  "$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  rm -rf "$WORK_DIR"
  exit "$status"
}
trap cleanup EXIT INT TERM

CONTAINER_NAME="groovy-agent-smoke-$$"

echo "==> Building runtime image (no model download)"
DOCKER_BUILDKIT=1 "$CONTAINER_ENGINE" build \
  --target runtime \
  --build-arg DOWNLOAD_MODEL=0 \
  -t "$IMAGE_NAME" \
  "$ROOT_DIR"

echo "==> Verifying compiled binary + built-in 'date' coreutil"
date_output="$("$CONTAINER_ENGINE" run --rm --entrypoint /usr/local/bin/groovy-agent "$IMAGE_NAME" date -u +%Y-%m-%dT%H:%M:%SZ)"
if ! [[ "$date_output" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]]; then
  echo "FAIL: unexpected 'date' coreutil output: $date_output" >&2
  exit 1
fi
echo "    date coreutil OK: $date_output"

mkdir -p "$WORK_DIR/output"

cat > "$WORK_DIR/stub-llama-server" <<'EOF'
#!/usr/bin/env python3
"""Deterministic stand-in for llama-server used by the smoke test.

Ignores all CLI args and serves a minimal HTTP server that answers /health
and /v1/models with HTTP 200 so docker/entrypoint.sh's readiness loop
succeeds without requiring a real model, GPU, or CPU inference.
"""
import http.server
import os

host = os.environ.get("LLAMA_SERVER_HOST", "127.0.0.1")
port = int(os.environ.get("LLAMA_SERVER_PORT", "8080"))


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path in ("/health", "/v1/models"):
            self.send_response(200)
            self.end_headers()
            self.wfile.write(b"{}")
        else:
            self.send_response(404)
            self.end_headers()

    def log_message(self, fmt, *args):
        pass


http.server.HTTPServer((host, port), Handler).serve_forever()
EOF

cat > "$WORK_DIR/stub-groovy-agent" <<'EOF'
#!/usr/bin/env bash
# Deterministic stand-in for groovy-agent that records the argv forwarded by
# docker/entrypoint.sh instead of performing real LLM inference.
printf '%s\n' "$@" > /output/forward-log.txt
exit 0
EOF

chmod +x "$WORK_DIR/stub-llama-server" "$WORK_DIR/stub-groovy-agent"
touch "$WORK_DIR/fake-model.gguf"

run_forwarding_case() {
  local case_name="$1"
  shift
  rm -f "$WORK_DIR/output/forward-log.txt"
  echo "==> Verifying entrypoint startup/command forwarding ($case_name)"
  "$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  timeout 60 "$CONTAINER_ENGINE" run --rm \
    --name "$CONTAINER_NAME" \
    -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
    -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
    -v "$WORK_DIR/fake-model.gguf:/models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf:ro" \
    -v "$WORK_DIR/output:/output" \
    -e LLAMA_STARTUP_TIMEOUT=15 \
    "$IMAGE_NAME" "$@"

  if [[ ! -f "$WORK_DIR/output/forward-log.txt" ]]; then
    echo "FAIL: entrypoint did not forward to groovy-agent ($case_name)" >&2
    exit 1
  fi
  echo "    forwarded argv: $(tr '\n' ' ' < "$WORK_DIR/output/forward-log.txt")"
}

# Case 1: first argument is already "run" -> forwarded verbatim.
run_forwarding_case "explicit 'run' subcommand" run -p "test" --workspace /output --yolo
if ! head -n1 "$WORK_DIR/output/forward-log.txt" | grep -qx "run"; then
  echo "FAIL: expected 'run' as first forwarded argument" >&2
  exit 1
fi

# Case 2: bare flags -> entrypoint prepends "agent".
run_forwarding_case "bare flags requiring 'agent' prefix" --workspace /output --yolo
if ! head -n1 "$WORK_DIR/output/forward-log.txt" | grep -qx "agent"; then
  echo "FAIL: expected 'agent' to be prepended to forwarded arguments" >&2
  exit 1
fi

echo "==> Container smoke test passed"
