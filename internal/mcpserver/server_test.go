package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func newTestServer(t *testing.T, workspace string) *Server {
	t.Helper()
	server, err := New(workspace, DefaultLimits(), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	return server
}

func call(t *testing.T, server *Server, name string, arguments map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(mcpproto.CallToolParams{Name: name, Arguments: mustJSON(t, arguments)})
	if err != nil {
		t.Fatalf("encode params: %v", err)
	}
	result := server.callTool(context.Background(), encoded)
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("tool result is not JSON: %v", err)
	}
	return body
}

func mustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return encoded
}

func expectError(t *testing.T, body map[string]any, category string) {
	t.Helper()
	if body["success"] != false {
		t.Fatalf("expected failure, got %v", body)
	}
	if body["error"] != category {
		t.Fatalf("expected error %q, got %v", category, body["error"])
	}
	if message, _ := body["message"].(string); strings.Contains(message, string(os.PathSeparator)+"tmp") {
		t.Fatalf("error message leaked a host path: %q", message)
	}
}

func TestWriteCapableToolsAreNotExposed(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	exposed := map[string]bool{}
	for _, name := range server.ToolNames() {
		exposed[name] = true
	}
	for _, name := range WriteCapableTools {
		if exposed[name] {
			t.Fatalf("write-capable tool %q must not be exposed", name)
		}
	}
	for _, name := range []string{"base64", "basename", "cat", "cut", "date", "dirname", "grep", "head", "paste", "pwd", "sha256sum", "sort", "tail", "tr", "uniq", "wc"} {
		if !exposed[name] {
			t.Fatalf("read-only tool %q must be exposed", name)
		}
	}
	body := call(t, server, "unlink", map[string]any{"path": "a.txt"})
	expectError(t, body, mcpproto.ErrorUnknownTool)
}

func TestPathTraversalIsRejected(t *testing.T) {
	workspace := t.TempDir()
	server := newTestServer(t, workspace)

	for _, path := range []string{"../../etc/passwd", "..", "/etc/passwd", "sub/../../outside.txt"} {
		body := call(t, server, "cat", map[string]any{"path": path})
		expectError(t, body, mcpproto.ErrorWorkspaceViolation)
	}
}

func TestSymlinkEscapeIsRejected(t *testing.T) {
	workspace := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(workspace, "escape.txt")); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	server := newTestServer(t, workspace)
	body := call(t, server, "cat", map[string]any{"path": "escape.txt"})
	expectError(t, body, mcpproto.ErrorWorkspaceViolation)
}

func TestSpecialFilesAreRejected(t *testing.T) {
	workspace := t.TempDir()
	if err := os.Mkdir(filepath.Join(workspace, "dir"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	server := newTestServer(t, workspace)
	body := call(t, server, "cat", map[string]any{"path": "dir"})
	expectError(t, body, mcpproto.ErrorInvalidArguments)
}

func TestFileReadIsBounded(t *testing.T) {
	workspace := t.TempDir()
	large := strings.Repeat("line of text\n", 4000)
	if err := os.WriteFile(filepath.Join(workspace, "big.txt"), []byte(large), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	server := newTestServer(t, workspace)

	body := call(t, server, "cat", map[string]any{"path": "big.txt"})
	if body["truncated"] != true {
		t.Fatalf("expected truncation, got %v", body)
	}
	output, _ := body["output"].(string)
	if len(output) > DefaultLimits().MaxFileReadBytes {
		t.Fatalf("output exceeded the file read limit: %d bytes", len(output))
	}

	body = call(t, server, "head", map[string]any{"path": "big.txt", "lines": 3})
	output, _ = body["output"].(string)
	if strings.Count(output, "\n") != 3 {
		t.Fatalf("head returned %d lines", strings.Count(output, "\n"))
	}
}

func TestGrepIsBounded(t *testing.T) {
	workspace := t.TempDir()
	content := strings.Repeat("TODO item\n", 50)
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte(content), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	server := newTestServer(t, workspace)

	body := call(t, server, "grep", map[string]any{"path": "README.md", "pattern": "TODO"})
	if body["truncated"] != true {
		t.Fatalf("expected truncation flag, got %v", body)
	}
	metadata, _ := body["metadata"].(map[string]any)
	if matches, _ := metadata["matches"].(float64); matches != float64(DefaultLimits().MaxGrepMatches) {
		t.Fatalf("expected %d matches, got %v", DefaultLimits().MaxGrepMatches, metadata["matches"])
	}
}

func TestArgumentsAreValidatedAgainstSchema(t *testing.T) {
	server := newTestServer(t, t.TempDir())

	expectError(t, call(t, server, "head", map[string]any{"path": "a.txt", "lines": 5000}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, "cat", map[string]any{}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, "cat", map[string]any{"path": "a.txt", "shell": "rm -rf /"}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, "wc", map[string]any{}), mcpproto.ErrorInvalidArguments)
	expectError(t, call(t, server, "grep", map[string]any{"path": "a.txt", "pattern": ""}), mcpproto.ErrorInvalidArguments)
}

func TestPwdReturnsLogicalWorkspacePath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	server := newTestServer(t, workspace)
	body := call(t, server, "pwd", map[string]any{})
	if body["output"] != "/project" {
		t.Fatalf("expected logical path, got %v", body["output"])
	}
	if strings.Contains(body["output"].(string), os.TempDir()) {
		t.Fatalf("pwd leaked a host path: %v", body["output"])
	}
}

func TestTextToolsAreBounded(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	oversized := strings.Repeat("a", DefaultLimits().MaxFileReadBytes+1)
	expectError(t, call(t, server, "sort", map[string]any{"text": oversized}), mcpproto.ErrorInvalidArguments)

	body := call(t, server, "sort", map[string]any{"text": "b\na\nb\n", "unique": true})
	if body["output"] != "a\nb\n" {
		t.Fatalf("unexpected sort output %v", body["output"])
	}
}

func TestServeHandlesLifecycleOverStdio(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	input := strings.NewReader(strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`not json`,
		`{"jsonrpc":"2.0","id":3,"method":"nope"}`,
		"",
	}, "\n"))
	output := &strings.Builder{}
	if err := server.Serve(context.Background(), input, output); err != nil {
		t.Fatalf("Serve failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("expected 4 responses, got %d: %s", len(lines), output.String())
	}

	initialize := mcpproto.Message{}
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil {
		t.Fatalf("decode initialize response: %v", err)
	}
	result := mcpproto.InitializeResult{}
	if err := json.Unmarshal(initialize.Result, &result); err != nil {
		t.Fatalf("decode initialize result: %v", err)
	}
	if result.Capabilities.Tools == nil {
		t.Fatal("server must advertise tool capability")
	}

	listed := mcpproto.Message{}
	if err := json.Unmarshal([]byte(lines[1]), &listed); err != nil {
		t.Fatalf("decode tools/list response: %v", err)
	}
	tools := mcpproto.ListToolsResult{}
	if err := json.Unmarshal(listed.Result, &tools); err != nil {
		t.Fatalf("decode tools: %v", err)
	}
	if len(tools.Tools) != len(server.ToolNames()) {
		t.Fatalf("expected %d tools, got %d", len(server.ToolNames()), len(tools.Tools))
	}
	for _, tool := range tools.Tools {
		schema := map[string]any{}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("tool %q has an invalid schema", tool.Name)
		}
		if schema["type"] != "object" || schema["additionalProperties"] != false {
			t.Fatalf("tool %q schema must be a closed object", tool.Name)
		}
	}
}

func TestNewRejectsMissingWorkspace(t *testing.T) {
	if _, err := New(filepath.Join(t.TempDir(), "missing"), DefaultLimits(), nil); err == nil {
		t.Fatal("expected a missing workspace to be rejected")
	}
}
