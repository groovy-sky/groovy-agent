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
	if !strings.Contains(output.String(), `"name":"cat"`) {
		t.Fatalf("cat tool not listed: %s", output.String())
	}
}
