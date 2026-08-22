package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/approval"
	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

func TestServeInitializeAndCall(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cat","arguments":{"stdin":"hello MCP\n"}}}`,
		"",
	}, "\n")
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d responses: %s", len(lines), output.String())
	}
	var initialize struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &initialize); err != nil {
		t.Fatal(err)
	}
	if initialize.Result.ProtocolVersion != protocolVersion {
		t.Fatalf("protocol version = %q", initialize.Result.ProtocolVersion)
	}
	var call struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &call); err != nil {
		t.Fatal(err)
	}
	if len(call.Result.Content) != 1 || call.Result.Content[0].Text != "hello MCP\n" {
		t.Fatalf("unexpected call result: %s", lines[1])
	}
}

func TestServeListsTools(t *testing.T) {
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(
		"{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"tools/list\"}\n",
	), &output); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cat", "cp", "date", "grep", "pwd"} {
		if !strings.Contains(output.String(), `"name":"`+name+`"`) {
			t.Fatalf("%s tool not listed: %s", name, output.String())
		}
	}
}

func TestServeCallsGrep(t *testing.T) {
	var output bytes.Buffer
	if err := Serve(context.Background(), strings.NewReader(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"grep","arguments":{"args":["-n","match"],"stdin":"skip\nmatch\n"}}}`+"\n",
	), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "2:match\\n") {
		t.Fatalf("unexpected grep result: %s", output.String())
	}
}

func TestServeExternalModeListsCoreutils(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"",
	}, "\n")
	var output bytes.Buffer
	// External MCP mode (no workspace Config) uses Serve, which only exposes coreutils.
	if err := Serve(context.Background(), strings.NewReader(input), &output); err != nil {
		t.Fatal(err)
	}
	// External MCP mode (no workspace) still lists coreutils.
	if !strings.Contains(output.String(), `"name":"cat"`) {
		t.Fatalf("expected cat tool: %s", output.String())
	}
	// Agent-mode tools must NOT appear in external MCP mode.
	if strings.Contains(output.String(), `"name":"list_files"`) {
		t.Fatalf("list_files should not appear in external MCP mode: %s", output.String())
	}
}

func TestServeWithConfigListsAgentTools(t *testing.T) {
	tmp := t.TempDir()
	ws, err := workspace.New(tmp, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := approval.Policy{Yolo: true}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		"",
	}, "\n")
	var output bytes.Buffer
	cfg := Config{Workspace: ws, Policy: &policy}
	if err := ServeWithConfig(context.Background(), strings.NewReader(input), &output, cfg); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"list_files", "read_file", "search_files", "write_file", "apply_patch", "mkdir", "git_status", "git_diff", "run_coreutil", "exec_command"} {
		if !strings.Contains(output.String(), `"name":"`+name+`"`) {
			t.Fatalf("expected tool %s in ServeWithConfig output: %s", name, output.String())
		}
	}
	// Individual coreutils should NOT appear; they're routed via run_coreutil.
	if strings.Contains(output.String(), `"name":"cat"`) {
		t.Fatalf("cat should not appear as its own tool in agent MCP mode: %s", output.String())
	}
}

func TestServeWithConfigExecCommand(t *testing.T) {
	tmp := t.TempDir()
	ws, err := workspace.New(tmp, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	policy := approval.Policy{Yolo: true}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"exec_command","arguments":{"executable":"go","args":["env","GOVERSION"],"timeout_seconds":10}}}`,
		"",
	}, "\n")
	var output bytes.Buffer
	cfg := Config{Workspace: ws, Policy: &policy}
	if err := ServeWithConfig(context.Background(), strings.NewReader(input), &output, cfg); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("unexpected response count: %s", output.String())
	}
	var call struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &call); err != nil {
		t.Fatal(err)
	}
	if len(call.Result.Content) != 1 {
		t.Fatalf("unexpected call content: %s", lines[1])
	}
	if !strings.Contains(call.Result.Content[0].Text, `"success":true`) {
		t.Fatalf("expected success result: %s", call.Result.Content[0].Text)
	}
	if !strings.Contains(call.Result.Content[0].Text, "go1.") {
		t.Fatalf("expected go version output: %s", call.Result.Content[0].Text)
	}
}
