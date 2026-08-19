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

The agent still exposes only the existing `run_coreutil` tool that dispatches to
the internal `coreutils.Run(...)` implementation. It does **not** expose shell
execution or arbitrary command execution.

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

### Runtime environment variables

Agent/OpenAI-compatible settings (all optional in this image):

- `OPENAI_BASE_URL`
- `OPENAI_MODEL`
- `OPENAI_API_KEY`

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

## Agent mode (interactive REPL)

The same binary also supports a minimal terminal agent:

```sh
go run . agent
```

Environment variables:

- `OPENAI_API_KEY` (required unless provided by container entrypoint defaults)
- `OPENAI_MODEL` (optional, default: `gpt-4o-mini`)
- `OPENAI_BASE_URL` (optional, default: `https://api.openai.com/v1`)

Example:

```sh
export OPENAI_API_KEY=...
go run . agent
# groovy-agent agent mode. Type 'exit' to quit.
> count lines in README.md
README.md has 100 lines.
> show the first 2 lines
# groovy-agent
```

The agent uses an OpenAI-compatible Chat Completions endpoint and exposes
existing coreutils through one function tool (`run_coreutil`). Tool execution is
conservative by design: utilities run in the current working directory with the
same behavior as direct CLI/MCP mode. There is no shell execution, arbitrary
command execution, memory store, scheduler, subagents, or external MCP client
mode in this MVP.

## Command-line use

The same binary can run a utility directly:

```sh
go run . sha256sum README.md
printf 'one\ntwo\n' | go run . wc
printf 'b\na\n' | go run . sort
```

Behavior summary:

- `groovy-agent` (no args) or `groovy-agent mcp`: MCP server mode
- `groovy-agent agent`: interactive AI agent mode
- `groovy-agent <utility> ...`: direct coreutils command mode

This is a focused, portable implementation rather than a claim of complete
GNU coreutils compatibility. Unsupported options return an error instead of
being silently ignored.

`grep` supports regular expressions by default, or literal searches with `-F`,
along with `-n` (line numbers), `-v` (inverted matches), and `-E`. Each input
is limited to 16 MiB. `cp` copies regular files and will not replace an
existing destination unless `-f` is supplied. `date` supports `-u` and a
`+FORMAT` using `%Y`, `%m`, `%d`, `%H`, `%M`, `%S`, `%z`, `%Z`, `%F`, `%T`,
and `%%`.
