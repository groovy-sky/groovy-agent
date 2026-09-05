# groovy-agent

A resource-efficient Go CLI agent for a local [`llama-server`](https://github.com/ggml-org/llama.cpp)
running Qwen2.5, plus a dedicated stdio **coreutils MCP server** child process.

[`PLAN.md`](PLAN.md) is the product specification; this README describes how to
build and run what it specifies.

```text
User
  │
  ▼
cmd/agent
  ├── HTTP ─────► llama-server (Qwen2.5-1.5B-Instruct Q4_K_M, 4096 ctx)
  └── stdio ────► cmd/coreutils-mcp (read-only coreutils tools)
```

## Layout

```text
cmd/agent/            CLI entry point
cmd/coreutils-mcp/    MCP server executable (stdio)
coreutils/            bounded, pure utility helpers
internal/agent/       configuration, tool profiles, validation, agent loop
internal/llm/         llama-server chat-completions client (function tools)
internal/mcpclient/   MCP client: lifecycle, discovery, CallTool
internal/mcpserver/   coreutils MCP server: tools, workspace, limits
internal/mcpproto/    JSON-RPC 2.0 / MCP message types
internal/jsonschema/  strict subset validator for tool input schemas
```

## Build

```bash
./scripts/build.sh          # produces ./bin/agent and ./bin/coreutils-mcp
go build ./...              # or build everything without installing
```

## Run

Start `llama-server` with the PLAN.md profile:

```bash
llama-server \
  -m /models/qwen2.5-1.5b-instruct-q4_k_m.gguf \
  --host 127.0.0.1 \
  --port 8080 \
  --alias local-qwen2.5 \
  --threads 4 \
  --threads-batch 4 \
  --ctx-size 4096 \
  --batch-size 128 \
  --parallel 1 \
  --jinja
```

Then run the agent:

```bash
go run ./cmd/agent \
  --llama-url http://127.0.0.1:8080 \
  --model local-qwen2.5 \
  --mcp-command ./bin/coreutils-mcp \
  --workspace "$(pwd)" \
  "Show the workspace path and identify the likely README."
```

The agent starts `coreutils-mcp` itself; the MCP server is never launched by
hand. Diagnostics go to **stderr**; only the final answer goes to **stdout**, so
`... "prompt" 2>/dev/null` yields a clean answer.

### CLI flags

| Flag | Default | Description |
| --- | --- | --- |
| `--llama-url` | `http://127.0.0.1:8080` | Base URL of the local `llama-server`. |
| `--model` | `local-qwen2.5` | Model name/alias advertised by `llama-server`. |
| `--mcp-command` | `./bin/coreutils-mcp` | Path to the MCP server executable. |
| `--workspace` | `.` | Directory that bounds every filesystem operation. |
| *(positional)* | – | The request. Required. |

`coreutils-mcp` accepts a single flag, `--workspace`, and is started by the
agent with the validated workspace path.

## Security model

* **Read-only tool policy.** The MCP server implements only
  `base64`, `basename`, `cat`, `cut`, `date`, `dirname`, `grep`, `head`,
  `paste`, `pwd`, `sha256sum`, `sort`, `tail`, `tr`, `uniq`, `wc`.
  The write-capable tools (`cp`, `link`, `mkdir`, `rmdir`, `tee`, `touch`,
  `unlink`) are **not implemented**; they remain disabled until a separate
  approval design exists.
* **Workspace boundary.** Paths must be workspace-relative. Absolute paths,
  `..` traversal, symlinks pointing outside the workspace, directories, and
  non-regular files are rejected. The agent pre-validates paths and the MCP
  server enforces the boundary again.
* **No shell.** Tool names are never treated as commands and arguments are
  never passed through a shell; every tool is implemented in Go.
* **Untrusted model output.** Every tool call is validated: supported call
  type, present call ID, membership in the currently exposed profile, JSON
  object arguments, JSON-schema conformity, path safety, requested limits that
  never exceed configured limits, and the total call budget.
* **Bounded execution.** Result 16 KiB, file read 12 KiB, grep matches 20, line
  length 2 KiB, tool duration 10 s, model output 256 tokens, model request
  180 s, MCP startup 10 s, 3 model rounds, 5 total tool calls.
* **Safe errors.** Failures are reported with a compact category —
  `unknown_tool`, `invalid_arguments`, `workspace_violation`,
  `permission_denied`, `timeout`, `result_too_large`, `tool_error` — without
  host details.
* **Lifecycle.** Signals, timeouts, errors, and success all cancel in-flight
  work, close the MCP session, and terminate the child process.

## Tool profiles

At most six tools are exposed per request, chosen deterministically from the
request text (never by an extra model call):

| Profile | Tools |
| --- | --- |
| `date` | `date` |
| `file_search` | `pwd`, `grep`, `head`, `wc` |
| `file_inspection` | `pwd`, `cat`, `head`, `tail`, `wc`, `sha256sum` |
| `path_processing` | `pwd`, `basename`, `dirname` |
| `text_processing` | `sort`, `uniq`, `wc`, `cut`, `tr`, `base64` |
| `fallback` | `pwd`, `cat`, `grep`, `head`, `tail`, `wc` |

## Acceptance examples

```bash
agent ... "Use the date tool and report the exact current time."
agent ... "Show the current workspace path and identify its README."
agent ... "Find occurrences of \"TODO\" in README.md."
agent ... "Read the beginning of README.md and summarize it."
agent ... "Count the unique sorted lines in the supplied text."
agent ... "Read ../../etc/passwd."        # rejected: workspace_violation
agent ... "Delete README.md."             # no write tool is exposed
```

If the model keeps calling tools without answering, the agent stops with:

```text
Agent stopped: maximum model rounds reached.
```

## Tests

```bash
gofmt -l .
go vet ./...
go test ./...
```

The suite covers workspace traversal and symlink escape, the disabled write
tools, bounded reads/grep/results, profile selection, model tool-call
validation, MCP discovery constraints (pagination, duplicates, malformed
schemas, capability negotiation), and process lifecycle/cleanup, including an
end-to-end run against a stub `llama-server` with a real `coreutils-mcp` child
process.

## Container image

The image bundles `llama-server`, both executables, and (optionally) the GGUF
model:

```bash
DOCKER_BUILDKIT=1 docker build --target runtime --build-arg DOWNLOAD_MODEL=0 -t groovy-agent:runtime-check .
./scripts/package-image.sh          # build and export output/groovy-agent.tar
./scripts/download-model.sh         # fetch the GGUF into artifacts/models
```

At run time the entrypoint starts `llama-server`, waits until it is healthy,
and runs the agent once with the container command as the prompt:

```bash
docker run --rm \
  -v /models:/models:ro \
  -v "$(pwd)":/workspace:ro \
  groovy-agent:Qwen2_5 \
  "Show the workspace path and identify the likely README."
```
