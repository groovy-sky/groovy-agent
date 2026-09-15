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
		t.Fatalf("New: %v", err)
	}
	return server
}

func call(t *testing.T, server *Server, name, arguments string) map[string]any {
	t.Helper()
	params, _ := json.Marshal(mcpproto.CallToolParams{Name: name, Arguments: json.RawMessage(arguments)})
	result := server.callTool(context.Background(), params)
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	return body
}

func TestOnlySafeMCPToolsAreExposed(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	expected := []string{
		"cat", "coreutils_run", "cp", "find", "grep", "ls", "mkdir", "mv", "pwd", "rm", "rmdir", "touch", "write_file",
	}
	if names := server.ToolNames(); strings.Join(names, ",") != strings.Join(expected, ",") {
		t.Fatalf("unexpected tools: %v", names)
	}
	for _, name := range append(WriteCapableTools, "sh", "exec_shell_command") {
		if body := call(t, server, name, `{}`); body["error"] != mcpproto.ErrorUnknownTool {
			t.Fatalf("%s: %v", name, body)
		}
	}
}

func TestPwdReturnsLogicalWorkspacePath(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "project")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	body := call(t, newTestServer(t, workspace), "pwd", `{}`)
	if body["success"] != true || body["output"] != "/project" {
		t.Fatalf("unexpected pwd result: %v", body)
	}
}

func TestCoreutilsRunExecutesAndRejectsUnsafeInput(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	body := call(t, server, "coreutils_run", `{"command":"sort","stdin":"b\na\n"}`)
	if body["success"] != true || body["stdout"] != "a\nb\n" {
		t.Fatalf("unexpected sort result: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"rm"}`); body["error"] != mcpproto.ErrorPermissionDenied {
		t.Fatalf("unsafe command: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"sort","shell":"rm -rf /"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("invalid schema: %v", body)
	}
	if body := call(t, server, "coreutils_run", `{"command":"head","args":["-n","-1"]}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("invalid args: %v", body)
	}
}

func TestCatViewArgumentsAreValidated(t *testing.T) {
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("a\nb\nc\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t, workspace)
	if body := call(t, server, "cat", `{"path":"README.md","lines":2}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected lines+full rejection, got %v", body)
	}
	if body := call(t, server, "cat", `{"path":"README.md","view":"middle"}`); body["error"] != mcpproto.ErrorInvalidArguments {
		t.Fatalf("expected invalid view rejection, got %v", body)
	}
	full := call(t, server, "cat", `{"path":"README.md"}`)
	if full["success"] != true || full["output"] != "a\nb\nc\n" {
		t.Fatalf("expected successful default full view output, got %v", full)
	}
	fullMeta, ok := full["metadata"].(map[string]any)
	if !ok || fullMeta["view"] != "full" {
		t.Fatalf("unexpected full metadata: %v", full["metadata"])
	}
	head := call(t, server, "cat", `{"path":"README.md","view":"head","lines":2}`)
	if head["success"] != true || head["output"] != "a\nb\n" {
		t.Fatalf("expected successful head view output, got %v", head)
	}
	headMeta, ok := head["metadata"].(map[string]any)
	if !ok || headMeta["view"] != "head" || headMeta["lines"] != float64(2) {
		t.Fatalf("unexpected head metadata: %v", head["metadata"])
	}
	tail := call(t, server, "cat", `{"path":"README.md","view":"tail","lines":2,"max_bytes":4}`)
	if tail["success"] != true || tail["output"] != "b\nc\n" {
		t.Fatalf("expected successful tail view output, got %v", tail)
	}
	tailMeta, ok := tail["metadata"].(map[string]any)
	if !ok || tailMeta["view"] != "tail" || tailMeta["lines"] != float64(2) || tailMeta["max_bytes"] != float64(4) {
		t.Fatalf("unexpected tail metadata: %v", tail["metadata"])
	}
}

func TestServeHandlesLifecycleOverStdio(t *testing.T) {
	server := newTestServer(t, t.TempDir())
	input := strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"initialize\"}\n{\"jsonrpc\":\"2.0\",\"id\":2,\"method\":\"tools/list\"}\n")
	output := &strings.Builder{}
	if err := server.Serve(context.Background(), input, output); err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(output.String()), "\n"); len(lines) != 2 {
		t.Fatalf("unexpected output: %q", output.String())
	}
}
