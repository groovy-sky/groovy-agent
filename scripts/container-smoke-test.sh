#!/usr/bin/env bash
# Deterministic container-level smoke test for the runtime Docker/Podman image.
#
# This validates packaging/wiring regressions without requiring model
# inference or downloading the GGUF model at test runtime:
#
#   1. The final image keeps this repo's container entrypoint
#      (`/usr/local/bin/entrypoint.sh`) as its effective ENTRYPOINT (not the
#      upstream llama.cpp base image entrypoint), and still contains the
#      compiled `/usr/local/bin/groovy-agent`, `/usr/local/bin/coreutils-mcp`,
#      and `/usr/local/bin/webutils-mcp` binaries.
#   1a. The runtime image defaults to a dedicated non-root user with writable
#      output/XDG directories, exports a concrete Chrome/Chromium-compatible
#      executable plus default container-compatible browser flags for
#      `webutils-mcp`, and can start that browser headlessly without relying on
#      an Ubuntu Snap wrapper path such as `chromium-browser`.
#   2. `docker/entrypoint.sh` starts llama-server, waits for it to become
#      healthy, and forwards the container command to `groovy-agent`
#      with the bundled MCP server configured.
#   3. Without a positional prompt, `docker/entrypoint.sh` does not invoke
#      `groovy-agent` (which is a one-shot CLI and would fail with a usage
#      error) and instead keeps llama-server serving its API until stopped.
#   4. When `EXTERNAL_LLAMA_URL` is set, the entrypoint skips the bundled
#      `llama-server`, does not require a local model file, still forwards the
#      bundled MCP command to one-shot `groovy-agent`, and rejects no-prompt
#      serve mode with a clear diagnostic.
#   5. The real `llama-server` binary spawns the bundled `coreutils-mcp` over
#      stdio from the entrypoint's `--mcp-servers-json` registration and
#      discovers its tools (this happens before the model is loaded, so a
#      placeholder model file is enough), and is also started with
#      `--ui-mcp-proxy` (mirroring groovy-sky/local-ai's
#      `LLAMA_ARG_UI_MCP_PROXY=true`) so its Web UI can reach further,
#      browser-added MCP servers.
#   6. `docker run ... mcp` serves the bundled coreutils MCP tool set over the
#      MCP Streamable HTTP transport, independently of llama-server (which is
#      not started in this mode), and completes a real `initialize` /
#      `notifications/initialized` / `tools/list` / `tools/call` (`pwd`)
#      exchange against the actual `coreutils-mcp` binary.
#   7. The bundled tool-aware chat templates make the real `llama-server`
#      binary report `chat_template_caps.supports_tools`/
#      `supports_tool_calls: true` at `GET /props`, and its `/cors-proxy`
#      endpoint confirms `--ui-mcp-proxy` is actually active, using a tiny
#      committed placeholder GGUF (scripts/testdata/chat-template-smoke-model.gguf)
#      so no ~4GB model download or real text-generation inference is required.
#      The default Phi-4 ChatML template is checked directly, and the bundled
#      Gemma template is checked by explicitly selecting it and rendering a
#      conversation through llama.cpp's `/apply-template` endpoint to verify
#      Gemma turn markers plus `<tool_call>` / `<tool_response>` formatting.
#
# Test 2 replaces `llama-server` and `groovy-agent` inside the container with
# small deterministic stubs (a Python HTTP server that answers /health, and a
# script that records argv) so no real LLM inference happens, no model
# download is required, and no llama-server is ever exposed outside the
# container (no `-p`/published ports are used). Tests 4 and 5 use the real
# `coreutils-mcp` binary (no model/GPU/CPU inference is involved); test 5 talks to
# it with `docker exec` so its HTTP port is never published outside the
# container either. Test 6 loads the real `llama-server` binary (also reached
# only via `docker exec`, never a published port); its placeholder GGUF has a
# valid tiny architecture/tokenizer so the server starts and answers
# `/props`, but randomly initialized weights, so it is never used to
# generate text. The Gemma-specific check below still does **not** prove that
# real Gemma weights will choose to emit a structured tool call or that the
# full MCP/webutils round-trip works end to end; it only proves that the
# bundled Gemma template is accepted by the real runtime, advertises tool-call
# support at `/props`, and renders the expected prompt structure at
# `/apply-template`. Maintainers should still manually verify the published
# `ghcr.io/groovy-sky/groovy-agent:gemma-4-e2b-it` image with:
#   - `curl http://127.0.0.1:8080/props`
#   - `curl http://127.0.0.1:8080/tools`
#   - `curl http://127.0.0.1:8080/v1/chat/completions ...`
# and confirm `finish_reason: "tool_calls"` plus no literal `<|im_end|>`
# leakage in assistant text.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-groovy-agent:smoke-test}"
CONTAINER_ENGINE="${CONTAINER_ENGINE:-docker}"
EXPECTED_UID="${EXPECTED_UID:-10001}"
EXPECTED_GID="${EXPECTED_GID:-10001}"

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

echo "==> Verifying compiled binaries"
"$CONTAINER_ENGINE" run --rm --entrypoint /bin/sh "$IMAGE_NAME" -c \
  'test -x /usr/local/bin/groovy-agent && test -x /usr/local/bin/coreutils-mcp && test -x /usr/local/bin/webutils-mcp'
echo "    groovy-agent, coreutils-mcp, and webutils-mcp binaries OK"

echo "==> Verifying default non-root runtime user and writable paths"
if [[ "$("$CONTAINER_ENGINE" inspect --format '{{.Config.User}}' "$IMAGE_NAME")" != "${EXPECTED_UID}:${EXPECTED_GID}" ]]; then
  echo "FAIL: expected image default user ${EXPECTED_UID}:${EXPECTED_GID}" >&2
  exit 1
fi
"$CONTAINER_ENGINE" run --rm --entrypoint /bin/sh "$IMAGE_NAME" -c "
  test \"\$(id -u)\" = \"$EXPECTED_UID\" &&
  test \"\$(id -g)\" = \"$EXPECTED_GID\" &&
  test \"\$HOME\" = \"/home/groovy-agent\" &&
  test -w \"\$HOME\" &&
  test -w \"\$XDG_CONFIG_HOME\" &&
  test -w \"\$XDG_CACHE_HOME\" &&
  test -w \"\$XDG_DATA_HOME\" &&
  test -w \"\$XDG_RUNTIME_DIR\" &&
  test -w /output &&
  touch /output/non-root-smoke &&
  touch \"\$XDG_CACHE_HOME\"/non-root-smoke &&
  touch \"\$XDG_RUNTIME_DIR\"/non-root-smoke
"
echo "    image defaults to uid:gid ${EXPECTED_UID}:${EXPECTED_GID} with writable HOME/XDG/output paths"

echo "==> Verifying bundled browser path, default flags, sandbox payload, and headless launch"
"$CONTAINER_ENGINE" run --rm --entrypoint /bin/sh "$IMAGE_NAME" -c '
  test "${WEBUTILS_CHROME_EXECUTABLE:-}" = "/usr/bin/chromium" &&
  test "${WEBUTILS_CHROME_ARGS:-}" = "--no-sandbox --disable-dev-shm-usage" &&
  test -x /usr/bin/chromium &&
  test -u /usr/lib/chromium/chrome-sandbox &&
  timeout 20 /usr/bin/chromium \
    --headless \
    $WEBUTILS_CHROME_ARGS \
    --disable-gpu \
    --remote-debugging-port=0 \
    --dump-dom "data:text/html,<html><body>ok</body></html>" >/tmp/chromium-dom.txt 2>/dev/null || status=$? &&
  case "${status:-0}" in
    0|124) ;;
    *) exit "${status}" ;;
  esac &&
  grep -q "<body>ok</body>" /tmp/chromium-dom.txt
'
echo "    bundled browser executable, sandbox payload, default flags, and headless launch OK"

echo "==> Verifying runtime entrypoint wiring"
if [[ "$("$CONTAINER_ENGINE" inspect --format '{{json .Config.Entrypoint}}' "$IMAGE_NAME")" != '["/usr/local/bin/entrypoint.sh"]' ]]; then
  echo "FAIL: expected image ENTRYPOINT to be [\"/usr/local/bin/entrypoint.sh\"]" >&2
  exit 1
fi
echo "    image ENTRYPOINT is /usr/local/bin/entrypoint.sh"

echo "==> Verifying llama-server host default"
if ! "$CONTAINER_ENGINE" inspect \
    --format '{{range .Config.Env}}{{println .}}{{end}}' "$IMAGE_NAME" | grep -qx 'LLAMA_SERVER_HOST=0.0.0.0'; then
  echo "FAIL: expected image default LLAMA_SERVER_HOST=0.0.0.0" >&2
  exit 1
fi
echo "    LLAMA_SERVER_HOST defaults to 0.0.0.0"

mkdir -p "$WORK_DIR/output"
chmod 0777 "$WORK_DIR/output"

cat > "$WORK_DIR/https-fixture.go" <<'EOF'
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"log"
	"math/big"
	"net"
	"net/http"
	"time"
)

func main() {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		log.Fatalf("generate key: %v", err)
	}
	serialLimit := new(big.Int).Lsh(big.NewInt(1), 62)
	serialNumber, err := rand.Int(rand.Reader, serialLimit)
	if err != nil {
		log.Fatalf("serial: %v", err)
	}
	template := &x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName: "fixture.example",
		},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		DNSNames:              []string{"fixture.example"},
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, privateKey.Public(), privateKey)
	if err != nil {
		log.Fatalf("create certificate: %v", err)
	}
	certificate := tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  privateKey,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Fixture OK</title><link rel="icon" href="data:,"></head><body><main>non-root chromium fixture page</main><a href="https://fixture.example:9443/next">next</a></body></html>`))
	})
	mux.HandleFunc("/next", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><title>Fixture Next</title><link rel="icon" href="data:,"></head><body>next page</body></html>`))
	})

	listener, err := net.Listen("tcp", "127.0.0.1:9443")
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("fixture listening on https://127.0.0.1:9443")
	tlsListener := tls.NewListener(listener, &tls.Config{
		Certificates: []tls.Certificate{certificate},
		MinVersion:   tls.VersionTLS12,
	})
	log.Fatal(http.Serve(tlsListener, mux))
}
EOF
GOFLAGS='' CGO_ENABLED=0 go build -o "$WORK_DIR/https-fixture" "$WORK_DIR/https-fixture.go"

cat > "$WORK_DIR/chromium-test-wrapper" <<'EOF'
#!/usr/bin/env sh
exec /usr/bin/chromium \
  --host-resolver-rules='MAP fixture.example 127.0.0.1' \
  --ignore-certificate-errors \
  "$@"
EOF

cat > "$WORK_DIR/stub-llama-server" <<'EOF'
#!/usr/bin/env perl
use strict;
use warnings;
use IO::Socket::INET;

open my $fp, '>', '/output/llama-argv.txt' or die "open /output/llama-argv.txt: $!";
print {$fp} join("\n", @ARGV), "\n";
close $fp;

my $host = '0.0.0.0';
my $port = '8080';
for (my $i = 0; $i <= $#ARGV; $i++) {
  if ($ARGV[$i] eq '--host' && defined $ARGV[$i + 1]) {
    $host = $ARGV[$i + 1];
    $i++;
    next;
  }
  if ($ARGV[$i] eq '--port' && defined $ARGV[$i + 1]) {
    $port = $ARGV[$i + 1];
    $i++;
    next;
  }
}

my $server = IO::Socket::INET->new(
  LocalAddr => $host,
  LocalPort => $port,
  Listen => 5,
  ReuseAddr => 1,
  Proto => 'tcp',
) or die "listen: $!";

while (my $client = $server->accept()) {
  my $request = <$client>;
  next unless defined $request;
  while (defined(my $line = <$client>)) {
    last if $line =~ /^\r?\n$/;
  }
  my ($path) = $request =~ m{^\S+\s+(\S+)};
  if (defined $path && ($path eq '/health' || $path eq '/v1/models')) {
    print {$client} "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{}";
  } else {
    print {$client} "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n";
  }
  close $client;
}
EOF

cat > "$WORK_DIR/stub-groovy-agent" <<'EOF'
#!/usr/bin/env bash
# Deterministic stand-in for groovy-agent that records the argv forwarded by
# docker/entrypoint.sh instead of performing real LLM inference.
printf '%s\n' "$@" > /output/forward-log.txt
exit 0
EOF

chmod +x "$WORK_DIR/chromium-test-wrapper" "$WORK_DIR/stub-llama-server" "$WORK_DIR/stub-groovy-agent"
touch "$WORK_DIR/fake-model.gguf"

echo "==> Verifying Chromium-backed local browsing under the default container user"
timeout 90 "$CONTAINER_ENGINE" run --rm \
  --add-host fixture.example:93.184.216.34 \
  -v "$WORK_DIR/chromium-test-wrapper:/tmp/chromium-test-wrapper:ro" \
  -v "$WORK_DIR/https-fixture:/tmp/https-fixture:ro" \
  --entrypoint /bin/sh \
  "$IMAGE_NAME" -ceu '
    /tmp/https-fixture >/tmp/https-fixture.log 2>&1 &
    fixture_pid=$!
    cleanup() {
      kill -TERM "$fixture_pid" 2>/dev/null || true
      wait "$fixture_pid" 2>/dev/null || true
    }
    trap cleanup EXIT INT TERM
    attempts=0
    until curl -fkSs https://127.0.0.1:9443/ >/dev/null 2>&1; do
      if ! kill -0 "$fixture_pid" 2>/dev/null; then
        cat /tmp/https-fixture.log >&2 || true
        exit 1
      fi
      attempts=$((attempts + 1))
      if [ "$attempts" -ge 20 ]; then
        echo "fixture did not become ready in time" >&2
        cat /tmp/https-fixture.log >&2 || true
        exit 1
      fi
      sleep 1
    done
    timeout 30 /tmp/chromium-test-wrapper \
      --headless \
      $WEBUTILS_CHROME_ARGS \
      --disable-gpu \
      --remote-debugging-port=0 \
      --dump-dom "https://fixture.example:9443/" >/tmp/fixture-dom.txt 2>/dev/null || status=$?
    case "${status:-0}" in
      0|124) ;;
      *) exit "${status}" ;;
    esac
    grep -q "<title>Fixture OK</title>" /tmp/fixture-dom.txt
    grep -q "non-root chromium fixture page" /tmp/fixture-dom.txt
    grep -q "https://fixture.example:9443/next" /tmp/fixture-dom.txt
  '
echo "    local HTTPS fixture browsing succeeded under uid ${EXPECTED_UID}"

run_forwarding_case() {
  local case_name="$1"
  shift
  rm -f "$WORK_DIR/output/forward-log.txt" "$WORK_DIR/output/llama-argv.txt"
  echo "==> Verifying entrypoint startup/command forwarding ($case_name)"
  "$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
  timeout 60 "$CONTAINER_ENGINE" run --rm \
    --name "$CONTAINER_NAME" \
    -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
    -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
    -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
    -v "$WORK_DIR/output:/output" \
    -e LLAMA_STARTUP_TIMEOUT=15 \
    ${EXTRA_RUN_ENV[@]+"${EXTRA_RUN_ENV[@]}"} \
    "$IMAGE_NAME" "$@"

  if [[ ! -f "$WORK_DIR/output/forward-log.txt" ]]; then
    echo "FAIL: entrypoint did not forward to groovy-agent ($case_name)" >&2
    exit 1
  fi
  echo "    forwarded argv: $(tr '\n' ' ' < "$WORK_DIR/output/forward-log.txt")"
}

EXTRA_RUN_ENV=()

# The entrypoint provides container defaults before user-supplied agent flags and
# prompt arguments. With bundled MCP enabled, the agent talks to the public
# proxy address while llama-server itself binds to the internal upstream
# address.
run_forwarding_case "agent defaults plus prompt" --workspace /output "test prompt"
default_agent_llama_url="$(awk 'seen{print; exit} $0=="--llama-url"{seen=1}' "$WORK_DIR/output/forward-log.txt")"
if [[ "$default_agent_llama_url" != "http://0.0.0.0:8080" ]]; then
  echo "FAIL: expected forwarded agent --llama-url http://0.0.0.0:8080, got '${default_agent_llama_url:-<missing>}'" >&2
  exit 1
fi
echo "    entrypoint forwards the public llama/proxy URL to groovy-agent"
default_llama_host="$(awk 'seen{print; exit} $0=="--host"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$default_llama_host" != "127.0.0.1" ]]; then
  echo "FAIL: expected entrypoint default internal llama-server host 127.0.0.1, got '${default_llama_host:-<missing>}'" >&2
  exit 1
fi
echo "    entrypoint binds bundled llama-server to the internal upstream host"
if ! grep -qx -- "--mcp-command" "$WORK_DIR/output/forward-log.txt"; then
  echo "FAIL: expected bundled MCP command flag" >&2
  exit 1
fi
if ! grep -qx -- "/usr/local/bin/coreutils-mcp" "$WORK_DIR/output/forward-log.txt"; then
  echo "FAIL: expected bundled MCP command path" >&2
  exit 1
fi
if ! tail -n1 "$WORK_DIR/output/forward-log.txt" | grep -qx "test prompt"; then
  echo "FAIL: expected prompt to be forwarded" >&2
  exit 1
fi

# llama-server itself is started as an MCP client: the bundled read-only
# coreutils server is registered with it over stdio, confined to the workspace.
if ! grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected llama-server to be started with --mcp-servers-json" >&2
  exit 1
fi
if ! grep -q '"command":"/usr/local/bin/coreutils-mcp"' "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected the coreutils MCP server in the llama-server MCP config" >&2
  exit 1
fi
if ! grep -q '"--workspace","/output"' "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected the MCP config to confine tools to the workspace" >&2
  exit 1
fi
echo "    llama-server registers the bundled coreutils MCP server over stdio"

# EXTERNAL_LLAMA_URL switches the container into one-shot agent mode against an
# external OpenAI-compatible llama endpoint: the bundled llama-server must not
# start, no local model is needed, and the agent must still get its MCP command.
EXTRA_RUN_ENV=(-e EXTERNAL_LLAMA_URL=http://llama:8080)
run_forwarding_case "external llama URL" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if ! awk 'seen{print; exit} $0=="--llama-url"{seen=1}' "$WORK_DIR/output/forward-log.txt" | grep -qx "http://llama:8080"; then
  echo "FAIL: expected EXTERNAL_LLAMA_URL to be forwarded to groovy-agent" >&2
  exit 1
fi
if [[ -f "$WORK_DIR/output/llama-argv.txt" ]]; then
  echo "FAIL: bundled llama-server must not start when EXTERNAL_LLAMA_URL is set" >&2
  exit 1
fi
echo "    EXTERNAL_LLAMA_URL skips bundled llama-server and forwards the external URL"

# The entrypoint should validate EXTERNAL_LLAMA_URL with the same accepted
# http/https scheme rule as the Go agent.
echo "==> Verifying EXTERNAL_LLAMA_URL validation"
rm -f "$WORK_DIR/output/forward-log.txt" "$WORK_DIR/output/llama-argv.txt"
set +e
external_invalid_log="$("$CONTAINER_ENGINE" run --rm \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
  -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
  -v "$WORK_DIR/output:/output" \
  -e EXTERNAL_LLAMA_URL=llama:8080 \
  "$IMAGE_NAME" --workspace /output "test prompt" 2>&1)"
external_invalid_status=$?
set -e
if (( external_invalid_status == 0 )); then
  echo "FAIL: invalid EXTERNAL_LLAMA_URL unexpectedly succeeded" >&2
  exit 1
fi
if ! grep -q "EXTERNAL_LLAMA_URL must be an http or https URL" <<<"$external_invalid_log"; then
  echo "FAIL: expected invalid EXTERNAL_LLAMA_URL diagnostic" >&2
  printf '%s\n' "$external_invalid_log" >&2
  exit 1
fi
if [[ -f "$WORK_DIR/output/forward-log.txt" || -f "$WORK_DIR/output/llama-argv.txt" ]]; then
  echo "FAIL: invalid EXTERNAL_LLAMA_URL must fail before starting groovy-agent or llama-server" >&2
  exit 1
fi
echo "    invalid EXTERNAL_LLAMA_URL is rejected before any child process starts"

run_forwarding_case "agent defaults plus prompt (proxy assertions)" --workspace /output "test prompt"

# Alongside the server-side MCP registration, llama-server is also started
# with its own Web UI MCP CORS proxy (--ui-mcp-proxy) by default, mirroring
# groovy-sky/local-ai's LLAMA_ARG_UI_MCP_PROXY=true. This lets the Web UI's
# browser JavaScript reach any *further* MCP servers a user registers from
# its Settings panel; the bundled coreutils tools above are unaffected either
# way, since llama-server talks to that one directly over stdio, never
# through a browser.
if ! grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: expected llama-server to be started with --ui-mcp-proxy" >&2
  exit 1
fi
echo "    llama-server enables the Web UI MCP CORS proxy (--ui-mcp-proxy)"

# --ui-mcp-proxy must never be added without --tools or by weakening
# confinement: it must not be present when llama.cpp's separate, unsafe
# built-in tools feature would be enabled (it is never enabled by this
# entrypoint).
if grep -qx -- "--tools" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: entrypoint must never enable llama.cpp's built-in --tools" >&2
  exit 1
fi

# --ui-mcp-proxy has its own opt-out, independent of the coreutils
# registration, so operators can keep the bundled tools but disable the
# browser-facing CORS proxy.
EXTRA_RUN_ENV=(-e LLAMA_MCP_UI_PROXY=0)
run_forwarding_case "MCP UI proxy disabled" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if ! grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_UI_PROXY=0 must not disable the coreutils MCP server" >&2
  exit 1
fi
if grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_UI_PROXY=0 must not add --ui-mcp-proxy" >&2
  exit 1
fi
echo "    LLAMA_MCP_UI_PROXY=0 keeps the coreutils MCP server without --ui-mcp-proxy"

# An explicit LLAMA_SERVER_HOST override must still be forwarded to the
# public address the one-shot agent uses.
EXTRA_RUN_ENV=(-e LLAMA_SERVER_HOST=127.0.0.1)
run_forwarding_case "llama host override" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
override_agent_llama_url="$(awk 'seen{print; exit} $0=="--llama-url"{seen=1}' "$WORK_DIR/output/forward-log.txt")"
if [[ "$override_agent_llama_url" != "http://127.0.0.1:8080" ]]; then
  echo "FAIL: expected LLAMA_SERVER_HOST override to set forwarded --llama-url http://127.0.0.1:8080, got '${override_agent_llama_url:-<missing>}'" >&2
  exit 1
fi
echo "    LLAMA_SERVER_HOST override is forwarded to the public llama/proxy URL"

# The registration is opt-out, so operators can start llama-server without any
# tool set at all.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0)
run_forwarding_case "MCP registration disabled" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if grep -qx -- "--mcp-servers-json" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_COREUTILS=0 must not register MCP servers" >&2
  exit 1
fi
if grep -qx -- "--ui-mcp-proxy" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: LLAMA_MCP_COREUTILS=0 must not add --ui-mcp-proxy either" >&2
  exit 1
fi
echo "    LLAMA_MCP_COREUTILS=0 starts llama-server without MCP servers or the UI proxy"

# With both bundled MCP servers disabled and no explicit template override, the
# entrypoint must not force a bundled tool-aware template file.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0 -e LLAMA_MCP_WEBUTILS=0)
run_forwarding_case "no MCP and no explicit template override" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if grep -qx -- "--chat-template-file" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: no-MCP default path must not force --chat-template-file" >&2
  exit 1
fi
if grep -qx -- "--chat-template" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: no-MCP default path must not force --chat-template" >&2
  exit 1
fi
echo "    no-MCP default path keeps llama-server's model/default template selection"

# With bundled MCP enabled, keep the bundled default tool-aware template file.
run_forwarding_case "MCP enabled keeps default bundled template file" --workspace /output "test prompt"
default_template_file="$(awk 'seen{print; exit} $0=="--chat-template-file"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$default_template_file" != "/opt/llama/chat-templates/tool-use-chatml.jinja" ]]; then
  echo "FAIL: expected default bundled chat template file with MCP enabled, got '${default_template_file:-<missing>}'" >&2
  exit 1
fi
echo "    MCP-enabled default path keeps the bundled tool-aware template file"

# An explicit non-empty template file must be retained even without bundled MCP.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0 -e LLAMA_MCP_WEBUTILS=0 -e LLAMA_CHAT_TEMPLATE_FILE=/opt/llama/chat-templates/tool-use-gemma.jinja)
run_forwarding_case "explicit non-empty chat template file with no MCP" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
explicit_template_file="$(awk 'seen{print; exit} $0=="--chat-template-file"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$explicit_template_file" != "/opt/llama/chat-templates/tool-use-gemma.jinja" ]]; then
  echo "FAIL: explicit LLAMA_CHAT_TEMPLATE_FILE must be retained without MCP, got '${explicit_template_file:-<missing>}'" >&2
  exit 1
fi
echo "    explicit non-empty LLAMA_CHAT_TEMPLATE_FILE is retained with no MCP"

# An explicit empty template-file override is intentional and must not be
# replaced by the bundled default.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0 -e LLAMA_MCP_WEBUTILS=0 -e LLAMA_CHAT_TEMPLATE_FILE=)
run_forwarding_case "explicit empty chat template file with no MCP" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
if grep -qx -- "--chat-template-file" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: explicit empty LLAMA_CHAT_TEMPLATE_FILE must not add --chat-template-file" >&2
  exit 1
fi
if grep -qx -- "--chat-template" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: explicit empty LLAMA_CHAT_TEMPLATE_FILE must not add --chat-template" >&2
  exit 1
fi
echo "    explicit empty LLAMA_CHAT_TEMPLATE_FILE is preserved with no MCP"

# An explicit LLAMA_CHAT_TEMPLATE must take priority over template-file
# selection, even when LLAMA_CHAT_TEMPLATE_FILE is also set.
EXTRA_RUN_ENV=(-e LLAMA_MCP_COREUTILS=0 -e LLAMA_MCP_WEBUTILS=0 -e LLAMA_CHAT_TEMPLATE=chatml -e LLAMA_CHAT_TEMPLATE_FILE=/opt/llama/chat-templates/tool-use-gemma.jinja)
run_forwarding_case "explicit chat template wins over template file with no MCP" --workspace /output "test prompt"
EXTRA_RUN_ENV=()
explicit_template="$(awk 'seen{print; exit} $0=="--chat-template"{seen=1}' "$WORK_DIR/output/llama-argv.txt")"
if [[ "$explicit_template" != "chatml" ]]; then
  echo "FAIL: explicit LLAMA_CHAT_TEMPLATE must be retained, got '${explicit_template:-<missing>}'" >&2
  exit 1
fi
if grep -qx -- "--chat-template-file" "$WORK_DIR/output/llama-argv.txt"; then
  echo "FAIL: explicit LLAMA_CHAT_TEMPLATE must win over LLAMA_CHAT_TEMPLATE_FILE" >&2
  exit 1
fi
echo "    explicit LLAMA_CHAT_TEMPLATE is retained and takes priority over template-file selection"

# Without a positional prompt there is nothing for the one-shot agent to do, so
# the entrypoint must keep llama-server running as an API server instead of
# invoking groovy-agent and failing with its missing-prompt usage error.
echo "==> Verifying no-prompt serve-only mode"
rm -f "$WORK_DIR/output/forward-log.txt"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
  -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
  -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_STARTUP_TIMEOUT=15 \
  "$IMAGE_NAME" >/dev/null

serve_deadline=$((SECONDS + 60))
until "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "serving llama-server only"; do
  if (( SECONDS >= serve_deadline )); then
    echo "FAIL: entrypoint did not announce serve-only mode" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done

if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME")" != "true" ]]; then
  echo "FAIL: container exited instead of serving llama-server" >&2
  "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
  exit 1
fi

if [[ -f "$WORK_DIR/output/forward-log.txt" ]]; then
  echo "FAIL: groovy-agent was invoked without a prompt" >&2
  exit 1
fi

if ! "$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
    curl -fsS "http://127.0.0.1:8080/v1/models" >/dev/null; then
  echo "FAIL: llama-server API not reachable in serve-only mode" >&2
  exit 1
fi
echo "    llama-server kept serving and groovy-agent was not invoked"

"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null
serve_exit="$("$CONTAINER_ENGINE" inspect -f '{{.State.ExitCode}}' "$CONTAINER_NAME")"
case "$serve_exit" in
  0|1|143) ;;
  *)
    echo "FAIL: serve-only container exited with unexpected status $serve_exit" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
    ;;
esac
echo "    serve-only container shut down cleanly on SIGTERM (exit $serve_exit)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

echo "==> Verifying EXTERNAL_LLAMA_URL without a prompt exits with a clear diagnostic"
rm -f "$WORK_DIR/output/forward-log.txt" "$WORK_DIR/output/llama-argv.txt"
set +e
external_serve_log="$("$CONTAINER_ENGINE" run --rm \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/stub-llama-server:/opt/llama/llama-server:ro" \
  -v "$WORK_DIR/stub-groovy-agent:/usr/local/bin/groovy-agent:ro" \
  -v "$WORK_DIR/output:/output" \
  -e EXTERNAL_LLAMA_URL=http://llama:8080 \
  "$IMAGE_NAME" 2>&1)"
external_serve_status=$?
set -e
if (( external_serve_status == 0 )); then
  echo "FAIL: external no-prompt mode should not claim to serve a local API" >&2
  exit 1
fi
if ! grep -q "No-prompt serve mode is unavailable in external-llama mode" <<<"$external_serve_log"; then
  echo "FAIL: expected external no-prompt diagnostic" >&2
  printf '%s\n' "$external_serve_log" >&2
  exit 1
fi
if [[ -f "$WORK_DIR/output/forward-log.txt" || -f "$WORK_DIR/output/llama-argv.txt" ]]; then
  echo "FAIL: external no-prompt mode must not start groovy-agent or llama-server" >&2
  exit 1
fi
echo "    external no-prompt mode exits clearly without starting local services"

# The bundled llama.cpp build is itself an MCP client: it spawns the servers
# listed in --mcp-servers-json over stdio and registers their tools before it
# loads the model. Running the *real* llama-server against a placeholder model
# file therefore exercises the full discovery handshake (initialize /
# notifications/initialized / tools/list) against the real coreutils-mcp
# binary, and still needs no model download, GPU, or inference: llama-server
# fails right after discovery, when it tries to load the placeholder model.
echo "==> Verifying llama-server discovers the bundled coreutils MCP tools"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
set +e
discovery_log="$(timeout 120 "$CONTAINER_ENGINE" run --rm \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/fake-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_STARTUP_TIMEOUT=15 \
  "$IMAGE_NAME" 2>&1)"
discovery_status=$?
set -e
if (( discovery_status == 124 )); then
  echo "FAIL: llama-server did not exit within the discovery timeout" >&2
  echo "$discovery_log" >&2
  exit 1
fi

if ! grep -qE "MCP warmup: 'coreutils' discovered [1-9][0-9]* tools" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not discover any coreutils MCP tools" >&2
  echo "$discovery_log" >&2
  exit 1
fi
if ! grep -qE "Added [1-9][0-9]* MCP tools" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not register the discovered MCP tools" >&2
  echo "$discovery_log" >&2
  exit 1
fi
echo "    llama-server spawned coreutils-mcp over stdio and registered its tools"

# --ui-mcp-proxy is passed alongside --mcp-servers-json by default (see
# docker/entrypoint.sh); confirm the real llama-server binary accepts it and
# reports the feature as enabled, rather than only asserting the argv wiring
# against the deterministic stub above.
if ! grep -q "MCP proxy (experimental)" <<< "$discovery_log"; then
  echo "FAIL: llama-server did not report the MCP proxy (--ui-mcp-proxy) as enabled" >&2
  echo "$discovery_log" >&2
  exit 1
fi
echo "    llama-server enabled the Web UI MCP CORS proxy (--ui-mcp-proxy)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# The model's own embedded chat template (and llama.cpp's plain "chatml"
# --chat-template fallback) do not render `tools`/`tool_calls`, so
# llama-server's jinja capability probe reports supports_tools/
# supports_tool_calls as false and the Web UI/API never receives a
# structured tool call (see the issue this addresses). docker/entrypoint.sh
# instead points --chat-template-file at a bundled tool-aware template by
# default. This is verified deterministically, without downloading or
# running inference against the real ~4GB Phi-4-mini GGUF, by loading the
# real `llama-server` binary against a committed placeholder GGUF
# (scripts/testdata/chat-template-smoke-model.gguf): a real but tiny/randomly
# initialized model whose *tokenizer and architecture* are enough for
# llama-server to start and answer GET /props, even though its untrained
# weights make it useless for actual text generation. `docker exec curl` is
# used so no port is ever published outside the container.
echo "==> Verifying the bundled chat template reports tool-call support at /props"
if ! command -v python3 >/dev/null 2>&1; then
  echo "FAIL: python3 is required on the host to parse the /props response" >&2
  exit 1
fi
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
props_startup_timeout=30
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$ROOT_DIR/scripts/testdata/chat-template-smoke-model.gguf:/models/Phi-4-mini-instruct.Q8_0.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_CTX_SIZE=4096 \
  -e LLAMA_STARTUP_TIMEOUT="$props_startup_timeout" \
  "$IMAGE_NAME" >/dev/null

# Add slack on top of LLAMA_STARTUP_TIMEOUT for container scheduling/model
# loading overhead (image pull/start latency observed in this test
# environment is well under this), so this deadline tracks the configured
# startup timeout instead of a second, independently maintained constant.
props_deadline_slack_seconds=30
props_deadline=$((SECONDS + props_startup_timeout + props_deadline_slack_seconds))
props_response=""
until [[ -n "$props_response" ]]; do
  if (( SECONDS >= props_deadline )); then
    echo "FAIL: llama-server did not become ready to serve /props in time" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: llama-server container exited before serving /props" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  props_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
    curl -fsS "http://127.0.0.1:8080/props" 2>/dev/null || true)"
  [[ -n "$props_response" ]] || sleep 1
done

if ! props_caps="$(python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    caps = doc["chat_template_caps"]
    ok = bool(caps.get("supports_tools")) and bool(caps.get("supports_tool_calls"))
except Exception as exc:
    print(f"error parsing /props: {exc}", file=sys.stderr)
    sys.exit(1)
print(json.dumps(caps))
sys.exit(0 if ok else 1)
' <<< "$props_response")"; then
  echo "FAIL: /props does not report chat_template_caps.supports_tools/supports_tool_calls: true" >&2
  echo "$props_response" >&2
  exit 1
fi
echo "    /props reports chat_template_caps: $props_caps"

# With MCP enabled by default, /tools must be active (not feature_disabled)
# and expose the bundled coreutils tool names consumed by the built-in Web UI.
tools_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
  curl -fsS "http://127.0.0.1:8080/tools" 2>/dev/null || true)"
if [[ -z "$tools_response" ]]; then
  echo "FAIL: /tools returned an empty response" >&2
  exit 1
fi
if ! tools_count="$(python3 -c '
import json, sys
doc = json.load(sys.stdin)
if isinstance(doc, dict):
    if doc.get("error", {}).get("type") == "feature_disabled":
        print("feature_disabled", file=sys.stderr)
        sys.exit(1)
    items = doc.get("data")
elif isinstance(doc, list):
    items = doc
else:
    items = None
if not isinstance(items, list):
    print("missing tools list", file=sys.stderr)
    sys.exit(1)
count = sum(1 for item in items if isinstance(item, dict) and str(item.get("tool", "")).startswith("coreutils_"))
if count <= 0:
    print("no coreutils tools discovered", file=sys.stderr)
    sys.exit(1)
print(count)
' <<< "$tools_response")"; then
  echo "FAIL: /tools is not enabled with bundled MCP tools" >&2
  echo "$tools_response" >&2
  exit 1
fi
echo "    /tools is enabled and advertises $tools_count bundled coreutils tool(s)"

# --ui-mcp-proxy makes llama-server serve a `/cors-proxy` endpoint for the Web
# UI's own MCP-over-browser feature; a disabled proxy answers 403 there (see
# LLAMA_MCP_UI_PROXY=0 in docker/entrypoint.sh), while an enabled one accepts
# the request path and fails later for unrelated reasons (e.g. a missing/
# invalid target), so any non-403 status is enough to confirm the capability
# is actually wired up in the real binary, without needing a real upstream
# MCP server to proxy to.
cors_proxy_status="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
  curl -s -o /dev/null -w '%{http_code}' "http://127.0.0.1:8080/cors-proxy" 2>/dev/null || true)"
if [[ "$cors_proxy_status" == "403" || -z "$cors_proxy_status" ]]; then
  echo "FAIL: /cors-proxy reported status '$cors_proxy_status'; expected --ui-mcp-proxy to be enabled" >&2
  exit 1
fi
echo "    /cors-proxy responds (status $cors_proxy_status), confirming --ui-mcp-proxy is enabled"
"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

echo "==> Verifying the bundled Gemma template at /props and /apply-template"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$ROOT_DIR/scripts/testdata/chat-template-smoke-model.gguf:/models/gemma-4-E2B-it-Q4_K_M.gguf:ro" \
  -v "$WORK_DIR/output:/output" \
  -e LLAMA_MODEL_FILE=gemma-4-E2B-it-Q4_K_M.gguf \
  -e LLAMA_MODEL_NAME=gemma-4-E2B-it \
  -e LLAMA_CTX_SIZE=4096 \
  -e LLAMA_STARTUP_TIMEOUT="$props_startup_timeout" \
  -e LLAMA_CHAT_TEMPLATE_FILE=/opt/llama/chat-templates/tool-use-gemma.jinja \
  "$IMAGE_NAME" >/dev/null

gemma_props_deadline=$((SECONDS + props_startup_timeout + props_deadline_slack_seconds))
gemma_props_response=""
until [[ -n "$gemma_props_response" ]]; do
  if (( SECONDS >= gemma_props_deadline )); then
    echo "FAIL: Gemma-template llama-server did not become ready to serve /props in time" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: Gemma-template llama-server container exited before serving /props" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  gemma_props_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" \
    curl -fsS "http://127.0.0.1:8080/props" 2>/dev/null || true)"
  [[ -n "$gemma_props_response" ]] || sleep 1
done

if ! gemma_props_caps="$(python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    caps = doc["chat_template_caps"]
    ok = bool(caps.get("supports_tools")) and bool(caps.get("supports_tool_calls"))
except Exception as exc:
    print(f"error parsing /props: {exc}", file=sys.stderr)
    sys.exit(1)
print(json.dumps(caps))
sys.exit(0 if ok else 1)
' <<< "$gemma_props_response")"; then
  echo "FAIL: Gemma template /props does not report chat_template_caps.supports_tools/supports_tool_calls: true" >&2
  echo "$gemma_props_response" >&2
  exit 1
fi
echo "    Gemma template /props reports chat_template_caps: $gemma_props_caps"

gemma_apply_template_request="$(cat <<'EOF'
{
  "messages": [
    {"role": "system", "content": "System says: use the provided tools when the user asks for fresh web information."},
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Search the web for the latest groovy-agent release notes."}
      ]
    },
    {
      "role": "assistant",
      "content": [
        {"type": "text", "text": "I will look that up."}
      ],
      "tool_calls": [
        {
          "id": "call_1",
          "type": "function",
          "function": {
            "name": "webutils_search_web",
            "arguments": {
              "query": "groovy-agent latest release notes"
            }
          }
        }
      ]
    },
    {
      "role": "tool",
      "tool_call_id": "call_1",
      "content": "{\"results\":[{\"title\":\"Release notes\",\"url\":\"https://example.invalid/release-notes\"}]}"
    }
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "webutils_search_web",
        "description": "Search the public web.",
        "parameters": {
          "type": "object",
          "properties": {
            "query": {"type": "string"}
          },
          "required": ["query"]
        }
      }
    },
    {
      "type": "function",
      "function": {
        "name": "webutils_browse_url",
        "description": "Browse a public HTTPS URL.",
        "parameters": {
          "type": "object",
          "properties": {
            "url": {"type": "string"}
          },
          "required": ["url"]
        }
      }
    }
  ],
  "add_generation_prompt": true
}
EOF
)"
gemma_rendered_prompt="$(
  printf '%s' "$gemma_apply_template_request" \
    | "$CONTAINER_ENGINE" exec -i "$CONTAINER_NAME" curl -fsS \
        -H 'Content-Type: application/json' \
        --data-binary @- \
        "http://127.0.0.1:8080/apply-template" \
        2>/dev/null || true
)"
if [[ -z "$gemma_rendered_prompt" ]]; then
  echo "FAIL: /apply-template returned an empty response for the Gemma template" >&2
  exit 1
fi
if ! gemma_prompt_summary="$(python3 -c '
import json, sys
import re
try:
    doc = json.load(sys.stdin)
    prompt = doc["prompt"]
except Exception as exc:
    print(f"error parsing /apply-template response: {exc}", file=sys.stderr)
    sys.exit(1)
checks = [
    ("<start_of_turn>user\nSystem says:", "missing Gemma user turn with folded system prompt"),
    ("Search the web for the latest groovy-agent release notes.", "missing user content"),
    ("webutils_search_web", "missing rendered tool schema/tool call"),
    ("webutils_browse_url", "missing rendered browse tool schema"),
    ("<tool_call>", "missing rendered tool call block"),
    ("<tool_response>", "missing rendered tool response block"),
    ("\"tool_call_id\": \"call_1\"", "missing rendered tool_call_id"),
    ("{\"results\":[", "tool response JSON payload was not preserved"),
    ("<start_of_turn>model\n", "missing Gemma model generation prompt")
]
for needle, error in checks:
    if needle not in prompt:
        print(error, file=sys.stderr)
        sys.exit(1)
for bad in ("<|im_start|>", "<|im_end|>"):
    if bad in prompt:
        print(f"unexpected ChatML marker in Gemma prompt: {bad}", file=sys.stderr)
        sys.exit(1)
match = re.search(r"I will look that up\.\n<tool_call>\n(.*?)\n</tool_call>", prompt, re.S)
if not match:
    print("missing parseable Gemma tool_call block", file=sys.stderr)
    sys.exit(1)
try:
    tool_call = json.loads(match.group(1))
except Exception as exc:
    print(f"rendered Gemma tool_call block was not valid JSON: {exc}", file=sys.stderr)
    sys.exit(1)
if tool_call.get("arguments") != {"query": "groovy-agent latest release notes"}:
    print("string tool arguments were not rendered as a raw JSON object", file=sys.stderr)
    sys.exit(1)
if prompt.index("System says:") > prompt.index("Search the web for the latest groovy-agent release notes."):
    print("system prompt was not folded ahead of the first user message", file=sys.stderr)
    sys.exit(1)
print(json.dumps({
    "prefix": prompt[:220],
    "contains_tool_call": "<tool_call>" in prompt,
    "contains_tool_response": "<tool_response>" in prompt
}))
' <<< "$gemma_rendered_prompt")"; then
  echo "FAIL: Gemma template /apply-template did not render the expected prompt shape" >&2
  echo "$gemma_rendered_prompt" >&2
  exit 1
fi
echo "    Gemma template /apply-template rendered the expected prompt shape: $gemma_prompt_summary"

gemma_no_user_request="$(cat <<'EOF'
{
  "messages": [
    {"role": "system", "content": "Synthetic first user instructions."},
    {"role": "assistant", "content": "Earlier assistant context."}
  ],
  "tools": [
    {
      "type": "function",
      "function": {
        "name": "webutils_search_web",
        "description": "Search the public web.",
        "parameters": {
          "type": "object",
          "properties": {
            "query": {"type": "string"}
          },
          "required": ["query"]
        }
      }
    }
  ],
  "add_generation_prompt": true
}
EOF
)"
gemma_no_user_rendered_prompt="$(
  printf '%s' "$gemma_no_user_request" \
    | "$CONTAINER_ENGINE" exec -i "$CONTAINER_NAME" curl -fsS \
        -H 'Content-Type: application/json' \
        --data-binary @- \
        "http://127.0.0.1:8080/apply-template" \
        2>/dev/null || true
)"
if [[ -z "$gemma_no_user_rendered_prompt" ]]; then
  echo "FAIL: /apply-template returned an empty response for the Gemma no-user edge case" >&2
  exit 1
fi
if ! python3 -c '
import json, sys
try:
    doc = json.load(sys.stdin)
    prompt = doc["prompt"]
except Exception as exc:
    print(f"error parsing no-user /apply-template response: {exc}", file=sys.stderr)
    sys.exit(1)
required = [
    "<start_of_turn>user\nSynthetic first user instructions.",
    "Earlier assistant context."
]
for needle in required:
    if needle not in prompt:
        print(f"missing expected content: {needle}", file=sys.stderr)
        sys.exit(1)
if prompt.index("<start_of_turn>user\nSynthetic first user instructions.") > prompt.index("Earlier assistant context."):
    print("synthetic first user turn was emitted after assistant history", file=sys.stderr)
    sys.exit(1)
' <<< "$gemma_no_user_rendered_prompt"; then
  echo "FAIL: Gemma template no-user edge case rendered out of order" >&2
  echo "$gemma_no_user_rendered_prompt" >&2
  exit 1
fi
echo "    Gemma template keeps the synthetic first user turn ahead of assistant-only history"
"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

# The `mcp` subcommand is a third, explicit deployment mode: it serves the
# bundled read-only coreutils MCP tool set over the MCP Streamable HTTP
# transport, independently of llama-server (which is not started at all in
# this mode). This exercises the real `coreutils-mcp` binary end to end
# (initialize, tools/list, tools/call) without any model/GPU/CPU inference,
# using `docker exec` so no port is ever published outside the container.
echo "==> Verifying remote MCP server mode (docker run ... mcp)"
rm -f "$WORK_DIR/output/forward-log.txt"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
"$CONTAINER_ENGINE" run -d \
  --name "$CONTAINER_NAME" \
  -v "$WORK_DIR/output:/output" \
  "$IMAGE_NAME" mcp >/dev/null

mcp_deadline=$((SECONDS + 30))
until "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "listening for MCP Streamable HTTP"; do
  if (( SECONDS >= mcp_deadline )); then
    echo "FAIL: coreutils-mcp did not report it was listening" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: mcp container exited before becoming ready" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done

warning_deadline=$((SECONDS + 15))
until "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" 2>&1 | grep -q "WARNING: MCP_HTTP_TOKEN is not set"; do
  if (( SECONDS >= warning_deadline )); then
    echo "FAIL: expected an unauthenticated-exposure warning without MCP_HTTP_TOKEN" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  if [[ "$("$CONTAINER_ENGINE" inspect -f '{{.State.Running}}' "$CONTAINER_NAME" 2>/dev/null)" != "true" ]]; then
    echo "FAIL: mcp container exited before logging the missing-token warning" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done
echo "    coreutils-mcp is listening and warns about the missing bearer token"

initialize_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"smoke-test","version":"1"}}}')"
if [[ "$initialize_response" != *'"protocolVersion"'* ]]; then
  echo "FAIL: initialize did not return a protocol version: $initialize_response" >&2
  exit 1
fi

"$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null

tools_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":2,"method":"tools/list"}')"
if [[ "$tools_response" != *'"pwd"'* ]]; then
  echo "FAIL: tools/list did not advertise the pwd tool: $tools_response" >&2
  exit 1
fi

pwd_response="$("$CONTAINER_ENGINE" exec "$CONTAINER_NAME" curl -fsS \
  -X POST "http://127.0.0.1:8765/mcp" \
  -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pwd","arguments":{}}}')"
if [[ "$pwd_response" != *'\"success\":true'* ]]; then
  echo "FAIL: pwd tool call did not succeed: $pwd_response" >&2
  exit 1
fi
echo "    initialize / tools/list / tools/call (pwd) succeeded over Streamable HTTP"

"$CONTAINER_ENGINE" stop -t 15 "$CONTAINER_NAME" >/dev/null
mcp_exit="$("$CONTAINER_ENGINE" inspect -f '{{.State.ExitCode}}' "$CONTAINER_NAME")"
case "$mcp_exit" in
  0|143) ;;
  *)
    echo "FAIL: mcp container exited with unexpected status $mcp_exit" >&2
    "$CONTAINER_ENGINE" logs "$CONTAINER_NAME" >&2 || true
    exit 1
    ;;
esac
echo "    mcp container shut down cleanly on SIGTERM (exit $mcp_exit)"
"$CONTAINER_ENGINE" rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true

echo "==> Container smoke test passed"
