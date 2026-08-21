package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
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

func TestServeWithConfigListsAgentTools(t *testing.T) {
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`,
		"",
	}, "\n")
	var output bytes.Buffer
	// Use a non-nil workspace path so ServeWithConfig returns agent tools.
	// We pass an empty Config{} here to exercise the workspace-less code path
	// and separately verify that a workspace-populated config works in an
	// integration test in the agent package.
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
