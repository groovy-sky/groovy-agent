# groovy-agent

A modern, dependency-free Go implementation of common coreutils, exposed as
tools over the [Model Context Protocol (MCP)](https://modelcontextprotocol.io/).
The project takes inspiration from
[`guonaihong/coreutils`](https://github.com/guonaihong/coreutils), while using
the current Go standard library and a stream-oriented API.

## Utilities

`base64`, `basename`, `cat`, `cp`, `cut`, `date`, `dirname`, `grep`, `head`,
`link`, `mkdir`, `paste`, `pwd`, `rmdir`, `sha256sum`, `sort`, `tail`, `tee`,
`touch`, `tr`, `uniq`, `unlink`, and `wc`.

Each MCP tool accepts optional `args` (an array of command arguments) and
`stdin` (a string). File operands are resolved relative to the server process.
Tool output is limited to 4 MiB and in-memory input used by utilities such as
`wc`, `tac`, and `tail` is bounded.

## Build and test

Go 1.24 or newer is required.

```sh
go test ./...
go vet ./...
go build -o groovy-agent .
```

The implementation has no third-party runtime or build dependencies.

## Dockerized local agent stack (groovy-agent + llama.cpp)

This repository includes a self-contained Docker runtime that starts:

1. `llama.cpp` `llama-server` (OpenAI-compatible API on `:8080`)
2. `groovy-agent agent`, configured to call that local API by default

Inside the container:

- `OPENAI_BASE_URL` defaults to `http://127.0.0.1:8080/v1`
- `OPENAI_MODEL` defaults to `Qwen2.5-Coder-7B-Instruct-Q4_K_M`
- `OPENAI_API_KEY` defaults to `local-llama` (override if needed)
- `OPENAI_REQUEST_TIMEOUT` defaults to `3h`

The agent exposes a safe coding toolset (workspace-confined file read/write,
bounded search/listing, fixed `git status`/`git diff` helpers, `run_coreutil`,
`exec_command`, and a dedicated `run_tests` validation tool). `exec_command`
runs an explicit executable + argument list inside the workspace (no shell
interpolation). `run_tests` runs the repository's standard `go test ./...`
command from the workspace root with a bounded timeout and a minimal child
environment (it does not inherit the agent's full environment, so local
model/API credentials are not exposed); it is intended for validating changes
after edits and never requires mutation approval.

### Requirements

- Docker with BuildKit support (`docker build --secret ...`)
- Disk:
  - image build/runtime dependencies: several GB
  - model file: ~4-5 GB (`Q4_K_M` GGUF)
- RAM: CPU inference is practical with ~16 GB+, but more is better

CPU-only inference can be slow. Tune with:

- `LLAMA_THREADS` (default auto-detected)
- `LLAMA_CTX_SIZE` (default `8192`)
- `LLAMA_N_GPU_LAYERS` (default `0`, raise for GPU offload builds/runtimes)
- `LLAMA_EXTRA_ARGS` (space-separated extra flags; avoid values containing spaces, and **never** source this from untrusted input)

### Model provisioning (no GGUF committed to git)

Download the exact model locally (supports optional `HF_TOKEN`):

```sh
./scripts/download-model.sh
# optional:
# HF_TOKEN=... ./scripts/download-model.sh
```

This stores:

- `artifacts/models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf`

`*.gguf` is ignored by git.

### Build and package image artifact

Create and export image tarball to `output/groovy-agent.tar`:

```sh
./scripts/package-image.sh
```

Optional build-time model download (for environments where you want the model
in-image and BuildKit secret auth):

```sh
HF_TOKEN=... DOWNLOAD_MODEL_AT_BUILD=1 ./scripts/package-image.sh
```

### Run the published image (Qwen2.5 model included)

The `ghcr.io/groovy-sky/groovy-agent:qwen2_5` image bundles the Qwen2.5 model,
so no host model mount or separate download step is required:

```sh
docker run --rm -it \
  -p 8080:8080 \
  ghcr.io/groovy-sky/groovy-agent:qwen2_5
```

The entrypoint starts `llama-server` and then launches `groovy-agent agent`.
The llama.cpp API is available on `http://localhost:8080` while the container
is running. The `--jinja` flag is passed to llama-server to enable Jinja-based
chat templates, which improve tool prompting for Qwen2.5 models. Every user
turn first goes through the moderator/planner (see
[Moderator/planner architecture](#moderatorplanner-architecture) below),
which requires the model to return one structured JSON plan before any tool
executes; the underlying dispatch still accepts either native OpenAI
`tool_calls` or a single standalone JSON tool-call envelope from the model, so
local llama.cpp runs do not depend on `message.tool_calls` support alone.

You can tune inference with the same environment variables documented below
(`LLAMA_THREADS`, `LLAMA_CTX_SIZE`, `LLAMA_N_GPU_LAYERS`, etc.).

**Interactive mode with `/output` as workspace:**

```sh
docker run --rm -it \
  -v "$(pwd)/output:/output" \
  -p 8080:8080 \
  ghcr.io/groovy-sky/groovy-agent:qwen2_5 \
  agent --workspace /output --require-write time.txt
```

**Headless `run` mode:**

```sh
docker run --rm \
  -v "$(pwd)/output:/output" \
  -p 8080:8080 \
  ghcr.io/groovy-sky/groovy-agent:qwen2_5 \
  run -p "store the current date in time.txt" --workspace /output --yolo --require-write time.txt
```

The entrypoint checks whether the first argument is `agent` or `run` and
forwards it directly; any other argument (or no argument) defaults to `agent`
mode.

### Run with host-mounted model (recommended)

```sh
docker run --rm -it \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -e LLAMA_MODEL_PATH=/models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  -p 8080:8080 \
  groovy-agent:local
```

The entrypoint starts `llama-server`, waits for readiness, then launches
`groovy-agent agent`.

### Output persistence (headless mode)

When running in headless (`run`) mode, each completed run writes a JSON result
file to the output directory. Inside the container the default is `/output`
(controlled by `AGENT_OUTPUT_DIR`). Mount a host directory there to receive the
files automatically:

```sh
docker run --rm \
  -v "$(pwd)/artifacts/models:/models:ro" \
  -v "$(pwd)/output:/output" \
  -e LLAMA_MODEL_PATH=/models/Qwen2.5-Coder-7B-Instruct-Q4_K_M.gguf \
  -p 8080:8080 \
  groovy-agent:local \
  run -p "store the current date in time.txt" --workspace /output --yolo --require-write time.txt
```

**`/output` has two roles depending on mode:**

- As **result persistence directory**: result JSON files (session ID, answer,
  events) are written here after each headless run.
- As **workspace**: if you pass `--workspace /output`, the agent also reads and
  writes files there. Use this when you want the agent to operate on files
  placed in the mounted output directory.

```sh
# Interactive mode where /output is also the workspace
docker run --rm -it \
  -v "$(pwd)/output:/output" \
  ghcr.io/groovy-sky/groovy-agent:qwen2_5 \
  agent --workspace /output --yolo --require-write time.txt
```

Once in agent mode, try prompts like:
- `list the files in the workspace at depth 1`
- `read README.md`
- `search for the word "agent" in .go files`

Each run produces `<output-dir>/<session-id>.json` containing the session ID,
final answer, and tool events. The directory is created automatically when the
agent starts, so no manual setup is required.

To change the output directory without remounting, set `AGENT_OUTPUT_DIR` or
pass `--output-dir` on the command line:

```sh
go run . run -p "hello" --output-dir /tmp/results
```

### Runtime environment variables

Agent/OpenAI-compatible settings (all optional in this image):

- `OPENAI_BASE_URL`
- `OPENAI_MODEL`
- `OPENAI_API_KEY`
- `OPENAI_REQUEST_TIMEOUT` (Go duration, default `3h`; `0` disables the timeout)
- `AGENT_OUTPUT_DIR` (default `/output` in the container, `output` on bare metal)

llama-server/container settings:

- `LLAMA_SERVER_HOST` (default `127.0.0.1`)
- `LLAMA_SERVER_PORT` (default `8080`)
- `LLAMA_MODEL_PATH` or `LLAMA_MODEL_FILE`
- `LLAMA_MODEL_NAME` (model alias passed to `llama-server --alias`)
- `LLAMA_CTX_SIZE`
- `LLAMA_THREADS`
- `LLAMA_N_GPU_LAYERS`
- `LLAMA_EXTRA_ARGS`
- `LLAMA_STARTUP_TIMEOUT`

## MCP configuration

Build the binary and add it to an MCP client's configuration:

```json
{
  "mcpServers": {
    "coreutils": {
      "command": "/absolute/path/to/groovy-agent"
    }
  }
}
```

The default mode is an MCP server using newline-delimited JSON-RPC over
standard input and output.

## Moderator/planner architecture

Both `agent` (interactive) and `run` (headless) route every user turn through
an internal moderator/planner (`internal/moderator`) before any tool executes.
This replaces a single free-running loop, where the model both chose tools and
narrated results, with a deterministic pipeline:

```text
user turn
   │
   ▼
moderator prompt + conversation → local model → exactly one JSON Plan
   │
   ▼
Go validates the Plan (internal/moderator.Validate)
   │  - decision must be answer | use_tools | clarify | reject
   │  - use_tools requires a known tool name and a JSON object for
   │    every tool_calls[i].arguments
   │  - require_writes paths are normalized to workspace-relative paths
   ▼
accepted use_tools plan → dispatched through the existing in-process MCP
server (same channel as native tool_calls / the standalone JSON envelope) →
workspace confinement and approval/Yolo/plan-mode policy still apply
   │
   ▼
verified execution summary built only from actual ToolEvents
(internal/moderator.BuildReport / VerifiedReport.Render)
```

The model returns one JSON object shaped like:

```json
{
  "decision": "use_tools",
  "reason": "short rationale, never a claim that something already happened",
  "tool_calls": [
    {"name": "write_file", "arguments": {"path": "time.txt", "content": "2026-09-04\n"}}
  ],
  "require_writes": ["time.txt"]
}
```

- `decision` selects how the turn is handled: `answer` (no tools needed, a
  normal conversational reply follows), `use_tools` (execute `tool_calls` in
  order), `clarify` (ask the user a question, taken verbatim from `reason`),
  or `reject` (refuse, with `reason` explaining why).
- `tool_calls` are only executed when `decision` is `use_tools`; each entry's
  `arguments` must be a JSON object and its `name` must be a currently listed
  tool (from `tools/list`), or the plan is rejected before anything runs.
- `require_writes` lists workspace-relative files the request is expected to
  create or modify. These are merged with any CLI `--require-write` paths and
  verified identically: the turn is only considered complete if `write_file`
  successfully targeted each exact normalized path and the file exists
  afterward. This makes the postcondition automatic for planner-declared
  outputs, not just paths supplied via `--require-write` — closing the gap
  where a model could run `date` but never call `write_file` and still be
  reported as having created a file.

If the model returns anything other than exactly one valid JSON object (free
prose, multiple values, an unknown tool name, non-object arguments, an unknown
`decision`, etc.), the moderator asks once more with corrective feedback
describing the rejection. If the second attempt is still invalid, the turn
fails with an explicit planning error rather than silently falling back to
unvalidated free-form execution.

**Verification, not narration.** For `use_tools` plans, the final response is
not generated by asking the model to summarize what it did. It is rendered
directly from a `VerifiedReport` built from the actual `ToolEvent`s recorded
during execution: files changed (only paths with a successful `write_file` /
`apply_patch` / `mkdir`), commands run, and `run_tests` pass/fail status, plus
any required writes that remain unmet. The model's own `reason` may be
prefixed as a short rationale, but it is never the source of truth for what
was changed, run, or validated.

**Bounded repair, not open-ended looping.** If required writes are unmet after
the first plan executes, the moderator asks for exactly one follow-up plan
scoped to the missing paths and executes it the same validated way. If that
still does not satisfy every required write, the turn fails with
`required write was not completed: PATH` — the same error `--require-write`
has always produced, now also reachable purely from a planner-declared
`require_writes` entry.

**Limitations.** The moderator asks for one upfront ordered list of tool
calls per turn (plus at most one bounded repair attempt for unmet required
writes); it does not run an open-ended, multi-round tool loop where later
tool calls are chosen after observing earlier tool output within the same
turn. Tasks that genuinely need to react to an intermediate result (for
example, reading a file to decide how to patch it) may need to be split
across separate user turns. Small local models can still produce an invalid
plan on both attempts, in which case the turn fails explicitly instead of
guessing; this is intentional, since the goal is a deterministic Go-owned
decision rather than trusting model prose.

## Agent mode (interactive REPL)

```sh
go run . agent [--workspace PATH] [--plan] [--yolo] [--resume SESSION_ID] [--require-write PATH ...]
```

Environment variables:

- `OPENAI_API_KEY` (required; set to `local-llama` inside the container)
- `OPENAI_MODEL` (required; set to the llama-server model alias in the container)
- `OPENAI_BASE_URL` (default: `http://127.0.0.1:8080/v1`; **only loopback addresses are accepted**)
- `OPENAI_REQUEST_TIMEOUT` (optional Go duration, default: `3h`; `0` disables)

**Inference is local-only.** The agent enforces that `OPENAI_BASE_URL` resolves
to a loopback address (`127.0.0.1`, `localhost`, or `::1`). Remote endpoints
such as `https://api.openai.com/v1` are rejected at startup with a clear error.
This ensures that the binary always talks to the bundled local llama.cpp server
rather than any external API.

**Tool dispatch via MCP.** All model-requested tool calls are routed through an
in-process MCP server using serialized JSON-RPC (`initialize`,
`notifications/initialized`, `tools/list`, `tools/call`). The tool schemas
passed to the model are derived from `tools/list`, and results are fed back as
MCP content objects. This means the agent can be observed and extended using
standard MCP tooling.

**Inference protocol vs MCP dispatch.** `llama-server` is only the
OpenAI-compatible inference endpoint. It does not speak MCP. MCP remains the
local dispatch layer inside `groovy-agent`: the agent derives tool schemas from
`tools/list`, sends those schemas to `/v1/chat/completions`, and then dispatches
validated tool requests through the in-process MCP server.

**Selected tool-call mode.** Every user turn first goes through the
moderator/planner described above, which requires the model to return exactly
one JSON Plan rather than free-form tool_calls. Once a `use_tools` plan is
accepted, its `tool_calls` are converted into the same `toolCall` shape used
elsewhere in the codebase and dispatched through the identical in-process MCP
channel — the underlying dispatch primitives still accept both:

- native OpenAI `message.tool_calls`, and
- one standalone JSON object in assistant text with exactly `name`,
  `arguments`, optional `id`, and optional `type: "function"`.

The textual envelope may be bare JSON or fenced as `json`, but it must be the
entire assistant response. Its `name` must match a listed tool, and its
`arguments` must decode to a JSON object that the target tool accepts. Prose,
shell commands, and malformed JSON are not executed at any layer.

`--require-write PATH` adds a concrete postcondition for each ordinary user turn,
merged with any workspace-relative paths the accepted plan itself declared in
`require_writes`. The path must be workspace-relative, and the turn succeeds
only if `write_file` successfully targets that exact normalized path and the
file exists as a regular file in the workspace afterward. If the requirement is
unmet, the agent requests at most one bounded repair plan scoped to the missing
paths; shell commands are still not executed.

Interactive slash commands:

- `/help`
- `/status`
- `/diff`
- `/plan` (toggle)
- `/clear`
- `/session`
- `/resume <id>`

Mutation tools (`write_file`, `apply_patch`, `mkdir`) require approval by
default. Use `--yolo` to auto-approve. Use `--plan` to deny mutations while
returning structured planning feedback to the model. `--require-write` verifies
that a specific file write happened; it does not bypass approval, so interactive
approval or `--yolo` is still required for the write itself. `exec_command` is
also subject to this approval policy, but `run_tests` is a dedicated read-only
validation tool and always runs without approval, in any mode (including plan
mode and without `--yolo`).

Sessions are persisted as JSONL snapshots under:

- `.groovy-agent/sessions/<session-id>.jsonl`

Project instructions are loaded at startup (if present), in this order:

1. `GROOVY.md`
2. `AGENTS.md`
3. `.groovy-agent/instructions.md`

Loaded instructions are context only; they do not bypass workspace or approval
policy.

## Headless mode

```sh
go run . run -p "summarize current diff" [--workspace PATH] [--output text|json] [--plan] [--yolo] [--resume SESSION_ID] [--require-write PATH ...] [--max-tool-rounds N]
```

In non-interactive mode without `--yolo`, mutating tools are denied instead of
prompting.

Each turn is handled by the moderator/planner described above: the model must
return exactly one JSON Plan, Go validates and executes any accepted
`tool_calls` through the same MCP dispatcher used elsewhere, and the reported
answer for tool-using plans is a verified execution summary derived from
actual tool results. `--max-tool-rounds` gates whether a bounded repair plan
may be requested when required writes are unmet (a value of `1` disables the
repair attempt); it no longer controls an open-ended per-turn tool loop, since
the moderator asks for one ordered plan of tool calls per turn instead.

In headless mode, each `--require-write PATH` flag adds a verified postcondition
for the run, and is merged with any workspace-relative paths the accepted plan
itself listed in `require_writes`. A run exits with failure unless `write_file`
successfully writes that exact normalized workspace-relative path during the
run and the file exists afterward. When a required write is missed, the agent
performs at most one bounded repair plan before returning
`required write was not completed: PATH`.

**Example: the originally reported failure now fails loudly instead of lying.**
A prompt like "obtain the current date and store it in time.txt" can still
lead a small local model to plan only a `date` command without a matching
`write_file` call. Previously, only an explicit `--require-write time.txt`
caught this. Now, if the model's plan itself declares
`"require_writes": ["time.txt"]` (which the moderator system prompt asks it
to do whenever a request expects file output), the same verified check runs
automatically — the run fails with `required write was not completed:
time.txt` instead of reporting success. Passing `--require-write time.txt`
explicitly continues to work exactly as before and composes with any
planner-declared paths.

## Command-line use

The same binary can run a utility directly:

```sh
go run . sha256sum README.md
printf 'one\ntwo\n' | go run . wc
printf 'b\na\n' | go run . sort
```

Behavior summary:

- `groovy-agent` (no args) or `groovy-agent mcp`: MCP server mode
- `groovy-agent agent [flags]`: interactive AI agent mode
- `groovy-agent run -p \"...\" [flags]`: headless agent mode
- `groovy-agent exec [--workspace PATH] [--workdir DIR] [--timeout 30s] [--env KEY=VALUE ...] <executable> [args...]`: run one command in a workspace-confined directory and print JSON results
- `groovy-agent <utility> ...`: direct coreutils command mode

## Safe coding boundaries and non-goals

- No shell string interpolation or unrestricted network-fetch tools.
- Command execution uses explicit executable + argument arrays and workspace-confined working directories.
- File tools are confined to a canonical workspace root.
- Path traversal (`..`), absolute paths outside workspace, and symlink escapes
  are rejected.
- `apply_patch` intentionally supports a robust subset of unified diff:
  text-only updates to existing regular files; rename/copy/binary/new/delete
  patch metadata is rejected in favor of explicit `write_file`/`mkdir`.

This is a focused, portable implementation rather than a claim of complete
GNU coreutils compatibility. Unsupported options return an error instead of
being silently ignored.

`grep` supports regular expressions by default, or literal searches with `-F`,
along with `-n` (line numbers), `-v` (inverted matches), and `-E`. Each input
is limited to 16 MiB. `cp` copies regular files and will not replace an
existing destination unless `-f` is supplied. `date` supports `-u` and a
`+FORMAT` using `%Y`, `%m`, `%d`, `%H`, `%M`, `%S`, `%z`, `%Z`, `%F`, `%T`,
and `%%`.
