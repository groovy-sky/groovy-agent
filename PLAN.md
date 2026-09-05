# PLAN.md — Local Qwen2.5 MCP Agent in Go

## Goal

Build a resource-efficient Go CLI agent that:

1. Connects to a local `llama-server`.
2. Starts the repository's `coreutils` MCP server over stdio.
3. Discovers and filters its tools.
4. Sends selected tool definitions to Qwen2.5.
5. Executes validated tool calls through MCP.
6. Returns bounded tool results to the model.
7. Prints the final answer.

Target environment:

```text
CPU: 4 cores
RAM: 8 GB
GPU: not required
Concurrent requests: 1
MCP servers: 1
```

---

## Architecture

```text
User
  │
  ▼
Go CLI agent
  ├── HTTP ─────► llama-server
  │               Qwen2.5 Instruct GGUF
  │
  └── stdio ────► coreutils MCP server
```

The Go agent owns:

- conversation state;
- MCP process lifecycle;
- tool discovery and filtering;
- schema adaptation;
- tool-call validation;
- bounded tool execution;
- context and iteration limits;
- final output.

---

## Model Configuration

Use this default profile:

```text
Model:        Qwen2.5-1.5B-Instruct
Format:       GGUF
Quantization: Q4_K_M
Context:      4096 tokens
Concurrency:  1
```

Start `llama-server` with:

```bash
llama-server \
  -m /models/qwen2.5-1.5b-instruct-q4_k_m.gguf \
  --host 127.0.0.1 \
  --port 8080 \
  --threads 4 \
  --threads-batch 4 \
  --ctx-size 4096 \
  --batch-size 128 \
  --parallel 1 \
  --jinja
```

If the machine swaps or becomes unresponsive:

1. reduce context to 3072;
2. reduce context to 2048;
3. reduce batch size to 64;
4. reduce the number of exposed tools.

Qwen2.5-3B-Instruct Q4_K_M may be evaluated later, but it is not the default
for an 8 GB machine.

---

## Coreutils MCP Server

The MCP server exposes these tools:

```text
base64
basename
cat
cp
cut
date
dirname
grep
head
link
mkdir
paste
pwd
rmdir
sha256sum
sort
tail
tee
touch
tr
uniq
unlink
wc
```

Build coreutils as a dedicated Go MCP executable and launch it as a child
process.

Suggested repository layout:

```text
groovy-agent/
├── cmd/
│   ├── agent/
│   └── coreutils-mcp/
├── coreutils/
├── internal/
│   ├── agent/
│   ├── llm/
│   └── mcpclient/
├── go.mod
└── PLAN.md
```

The coreutils MCP server must:

- use stdin and stdout only for MCP messages;
- write diagnostics to stderr;
- validate all arguments;
- enforce workspace boundaries;
- enforce execution and output limits;
- terminate when its parent connection closes.

---

## Tool Policy

### Default read-only tools

The MVP may expose these tools:

```text
base64
basename
cat
cut
date
dirname
grep
head
paste
pwd
sha256sum
sort
tail
tr
uniq
wc
```

These tools are still subject to input, workspace, runtime, and output limits.

### Write-capable tools

Disable these tools by default:

```text
cp
link
mkdir
rmdir
tee
touch
unlink
```

They may be added later behind an explicit operator option and an approval
workflow.

Tool descriptions alone are not a security boundary. The MCP implementation
must enforce the policy.

---

## Tool Profiles

Do not send all tools to the 1.5B model on every request. Select a small,
deterministic profile based on the user request.

### File inspection

```text
pwd
cat
head
tail
wc
sha256sum
```

### File search

```text
pwd
grep
head
wc
```

### Path processing

```text
pwd
basename
dirname
```

### Text processing

```text
base64
cut
paste
sort
tr
uniq
wc
```

### Date

```text
date
```

Expose no more than six tools per model request.

When no profile matches confidently, expose:

```text
pwd
cat
grep
head
tail
wc
```

Profile selection must be deterministic. Do not make another LLM request merely
to select tools.

---

## Workspace Security

All filesystem operations must be restricted to an operator-configured
workspace.

Coreutils must:

- accept workspace-relative paths;
- canonicalize paths before use;
- reject path traversal;
- prevent symbolic-link escape;
- reject unauthorized absolute paths;
- limit recursive operations;
- reject special device files where applicable;
- avoid following unexpected links;
- return safe errors without host details.

The Go agent must pass the workspace configuration when starting coreutils, but
coreutils remains responsible for enforcing the boundary.

---

## Tool Limits

Recommended defaults:

```text
Maximum exposed tools:       6
Maximum model rounds:        3
Maximum total tool calls:    5
Maximum result size:         16 KiB
Maximum file-read size:      12 KiB
Maximum grep matches:        20
Maximum line length:         2 KiB
Maximum tool duration:       10 seconds
Maximum model output:        256 tokens
Model request timeout:       180 seconds
MCP startup timeout:         10 seconds
```

Tool-specific requirements:

- `cat`, `head`, and `tail` must not return unlimited content.
- `grep` must limit matches, scanned data, and execution time.
- `paste`, `sort`, `tr`, `uniq`, and `base64` must limit input and output.
- `sha256sum` must reject files above a configured size if hashing them would
  exceed the execution budget.
- `pwd` should return the logical workspace path rather than exposing unrelated
  host paths.
- `date` should return a concise, unambiguous timestamp.
- truncated results must explicitly state that truncation occurred.

---

## Tool Discovery

At startup:

1. Connect to the coreutils MCP server.
2. Complete MCP lifecycle negotiation.
3. Verify that tool capability is advertised.
4. Discover every tool, including paginated results.
5. Compare discovered names with the known coreutils policy.
6. Reject duplicate or malformed tool definitions.
7. Expose only tools allowed by the selected profile.

Unexpected tools must be logged and denied.

Missing tools should produce a warning unless they are required for the current
profile.

---

## Schema Adaptation

The coreutils MCP server remains the source of truth for:

- tool names;
- descriptions;
- input schemas;
- output schemas.

Adapt each selected MCP tool to the function-tool envelope expected by
`llama-server`:

```text
MCP name        → function name
MCP description → function description
MCP inputSchema → function parameters
```

Preserve:

- required properties;
- parameter types;
- enum restrictions;
- path descriptions;
- minimum and maximum values;
- additional-property restrictions.

Keep descriptions short because tool schemas consume model context.

Tool execution remains an MCP `CallTool` request. Only the model-facing schema
envelope is adapted.

---

## System Prompt

Use a short system prompt:

```text
You are a local assistant with access to coreutils tools.

Use tools when workspace data or an exact calculation is required.
Never invent tool results.
Call only listed tools.
Use JSON arguments matching the tool schema.
Use workspace-relative paths.
After receiving results, answer concisely.
Do not repeat large tool output unless requested.
```

Do not duplicate complete tool documentation in the system prompt.

---

## Agent Loop

For each model round:

1. Select the smallest relevant tool profile.
2. Send the conversation and selected tools to `llama-server`.
3. Validate the assistant response.
4. Append the complete assistant message to history.
5. If there are no tool calls, print the final text and exit.
6. Validate every requested tool call.
7. Execute valid calls sequentially through MCP.
8. Normalize and limit each result.
9. Append one tool-result message per tool-call ID.
10. Continue until the model returns text or reaches a limit.

The assistant message containing tool calls must appear before the corresponding
tool-result messages.

If the limit is reached, stop with:

```text
Agent stopped: maximum model rounds reached.
```

---

## Tool-Call Validation

Treat model output as untrusted.

Before execution, verify:

- the tool-call type is supported;
- a tool-call ID is present;
- the tool name is in the currently exposed profile;
- arguments contain a valid JSON object;
- arguments satisfy the MCP input schema;
- path arguments remain inside the workspace;
- requested limits do not exceed configured limits;
- the total tool-call limit has not been reached.

Never:

- interpret a tool name as a process command;
- pass arguments through a shell;
- allow the model to change the MCP command;
- execute a discovered but non-exposed tool;
- allow the model to increase safety limits.

Execute multiple tool calls sequentially.

---

## Tool Results

Prefer concise structured results.

A successful result should clearly contain:

- success status;
- tool output;
- truncation status;
- relevant metadata such as matched lines or byte count.

A failure should use a compact error type:

```text
unknown_tool
invalid_arguments
workspace_violation
permission_denied
timeout
result_too_large
tool_error
```

Tool-level failures should normally be returned to the model so it can correct
its request or explain the problem.

Stop the entire agent if the MCP transport or session becomes unusable.

---

## Context Management

The 4096-token context includes:

- system prompt;
- tool schemas;
- user input;
- assistant messages;
- tool calls;
- tool results;
- final response.

Before each request:

1. retain the system message and current user request;
2. retain unresolved tool calls and their results;
3. remove redundant earlier assistant text;
4. discard obsolete tool results;
5. truncate oversized results;
6. reserve at least 256 tokens for output;
7. reject the request if it still cannot fit safely.

Do not use another model call to summarize history in the MVP.

---

## Process Lifecycle

Startup order:

1. Parse configuration.
2. Validate the workspace.
3. Verify that `llama-server` is reachable.
4. Start the coreutils MCP process.
5. Establish the MCP session.
6. Discover and validate tools.
7. Start the agent loop.

On success, error, timeout, or Ctrl+C:

1. cancel active model and tool requests;
2. close the MCP session;
3. terminate the coreutils process;
4. ensure no child process remains.

---

## Logging

Write diagnostics to stderr and the final answer to stdout.

Log:

- selected model profile;
- MCP startup and connection;
- discovered and exposed tool names;
- model round number;
- requested tool name;
- bounded arguments;
- tool duration;
- error category;
- result truncation.

Do not log:

- unrestricted file contents;
- secrets;
- environment variables;
- unbounded arguments or results;
- raw binary data.

---

## Implementation Steps

### 1. Model baseline

Verify that Qwen2.5-1.5B-Instruct Q4_K_M:

- loads without sustained swapping;
- produces normal chat responses;
- produces valid tool calls;
- respects a 256-token output limit.

### 2. Coreutils MCP server

Verify:

- stdio initialization;
- tool discovery;
- valid schemas;
- stderr-only logging;
- workspace enforcement;
- output limits;
- cancellation and shutdown.

### 3. One-tool integration

Start with `date` because it requires no filesystem access.

Verify:

```text
model → date
agent → MCP
MCP → result
agent → model
model → final answer
```

### 4. Read-only filesystem tools

Add:

```text
pwd
cat
head
tail
wc
grep
```

Verify workspace boundaries and result limits.

### 5. Text-processing tools

Add:

```text
base64
basename
cut
dirname
paste
sha256sum
sort
tr
uniq
```

Verify that schemas and output remain compact.

### 6. Bounded agent loop

Add:

- three model rounds;
- five total tool calls;
- sequential multiple calls;
- context pruning;
- structured tool errors;
- signal handling.

### 7. Write tools

Keep these disabled until a separate approval design exists:

```text
cp
link
mkdir
rmdir
tee
touch
unlink
```

---

## Acceptance Tests

### Date

```text
Use the date tool and report the exact current time.
```

Expected:

- one tool call;
- valid final response;
- no filesystem access.

### Workspace inspection

```text
Show the current workspace path and identify its README.
```

Expected:

- `pwd` followed by a bounded inspection tool;
- no invented files;
- no outside path access.

### Search

```text
Find occurrences of "TODO" in README.md.
```

Expected:

- bounded `grep` call;
- concise matches;
- explicit truncation if necessary.

### File summary

```text
Read the beginning of README.md and summarize it.
```

Expected:

- `head` preferred over an unlimited `cat`;
- bounded content;
- concise final response.

### Text pipeline

```text
Count the unique sorted lines in the supplied text.
```

Expected:

- appropriate text-processing tools;
- no more than five total calls;
- bounded intermediate results.

### Path traversal

```text
Read ../../etc/passwd.
```

Expected:

- request is rejected;
- no outside file is accessed;
- the model receives a workspace violation.

### Disabled write tool

```text
Delete README.md.
```

Expected:

- `unlink` is not exposed;
- no mutation occurs;
- the model explains that the operation is unavailable.

### Cancellation

Interrupt the process during a model or tool request.

Expected:

- active work is canceled;
- MCP process exits;
- no orphan remains.

---

## Done Criteria

The MVP is complete when:

```bash
go run ./cmd/agent \
  --llama-url http://127.0.0.1:8080 \
  --model local-qwen2.5 \
  --mcp-command ./bin/coreutils-mcp \
  --workspace "$(pwd)" \
  "Show the workspace path and identify the likely README."
```

---

## Implementation Notes

These notes record intentional decisions made while implementing this plan; the
requirements above are unchanged.

- The text-processing profile lists seven tools while the per-request limit is
  six. The implementation exposes `sort`, `uniq`, `wc`, `cut`, `tr`, and
  `base64`; `paste` is still implemented and remains reachable through other
  requests, but it is not part of the six exposed text-processing tools.
- The repository layout adds `internal/mcpserver` (coreutils MCP server),
  `internal/mcpproto` (JSON-RPC/MCP message types), and `internal/jsonschema`
  (strict schema-subset validator) alongside `internal/agent`, `internal/llm`,
  and `internal/mcpclient`.
- MCP is implemented directly as newline-delimited JSON-RPC 2.0 over stdio
  rather than through a third-party dependency, keeping the module free of
  external requirements.
- `pwd` returns `/<workspace directory name>` as the logical workspace path and
  `date` returns an RFC 3339 timestamp.
- Write-capable tools are not implemented at all; the server never registers
  them, so the policy cannot be bypassed by configuration.
