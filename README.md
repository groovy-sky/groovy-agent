# go-core-mcp

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
go build -o go-core-mcp .
```

The implementation has no third-party runtime or build dependencies.

## MCP configuration

Build the binary and add it to an MCP client's configuration:

```json
{
  "mcpServers": {
    "coreutils": {
      "command": "/absolute/path/to/go-core-mcp"
    }
  }
}
```

The default mode is an MCP server using newline-delimited JSON-RPC over
standard input and output.

## Command-line use

The same binary can run a utility directly:

```sh
go run . sha256sum README.md
printf 'one\ntwo\n' | go run . wc
printf 'b\na\n' | go run . sort
```

This is a focused, portable implementation rather than a claim of complete
GNU coreutils compatibility. Unsupported options return an error instead of
being silently ignored.

`grep` supports regular expressions by default, or literal searches with `-F`,
along with `-n` (line numbers), `-v` (inverted matches), and `-E`. Each input
is limited to 16 MiB. `cp` copies regular files and will not replace an
existing destination unless `-f` is supplied. `date` supports `-u` and a
`+FORMAT` using `%Y`, `%m`, `%d`, `%H`, `%M`, `%S`, `%z`, `%Z`, `%F`, `%T`,
and `%%`.
