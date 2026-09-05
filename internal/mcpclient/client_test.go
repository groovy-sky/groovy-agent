package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// scriptedServer answers requests using the supplied handler.
func scriptedServer(t *testing.T, handler func(mcpproto.Message) *mcpproto.Message) *Client {
	t.Helper()
	clientReader, serverWriter := io.Pipe()
	serverReader, clientWriter := io.Pipe()

	go func() {
		defer serverWriter.Close()
		scanner := bufio.NewScanner(serverReader)
		for scanner.Scan() {
			request := mcpproto.Message{}
			if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
				return
			}
			response := handler(request)
			if response == nil {
				continue
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				return
			}
			if _, err := serverWriter.Write(append(encoded, '\n')); err != nil {
				return
			}
		}
	}()

	client := New(clientReader, clientWriter, nil)
	t.Cleanup(client.Close)
	return client
}

func result(t *testing.T, id json.RawMessage, value any) *mcpproto.Message {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("encode result: %v", err)
	}
	return &mcpproto.Message{JSONRPC: "2.0", ID: id, Result: encoded}
}

func TestInitializeRequiresToolCapability(t *testing.T) {
	client := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		if request.Method != "initialize" {
			return nil
		}
		return result(t, request.ID, mcpproto.InitializeResult{ProtocolVersion: mcpproto.Version})
	})
	if _, err := client.Initialize(context.Background()); err == nil {
		t.Fatal("expected initialization to fail without tool capability")
	}
}

func TestInitializeSucceeds(t *testing.T) {
	client := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		if request.Method != "initialize" {
			return nil
		}
		return result(t, request.ID, mcpproto.InitializeResult{
			ProtocolVersion: mcpproto.Version,
			Capabilities:    mcpproto.ServerCapabilities{Tools: &mcpproto.ToolsCapability{}},
			ServerInfo:      mcpproto.Implementation{Name: "coreutils-mcp", Version: "1.0.0"},
		})
	})
	info, err := client.Initialize(context.Background())
	if err != nil {
		t.Fatalf("Initialize failed: %v", err)
	}
	if info.ServerInfo.Name != "coreutils-mcp" {
		t.Fatalf("unexpected server info %+v", info.ServerInfo)
	}
}

func TestListToolsFollowsPagination(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	client := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		params := mcpproto.ListToolsParams{}
		_ = json.Unmarshal(request.Params, &params)
		if params.Cursor == "" {
			return result(t, request.ID, mcpproto.ListToolsResult{
				Tools:      []mcpproto.Tool{{Name: "pwd", InputSchema: schema}},
				NextCursor: "page-2",
			})
		}
		return result(t, request.ID, mcpproto.ListToolsResult{
			Tools: []mcpproto.Tool{{Name: "date", InputSchema: schema}},
		})
	})
	tools, err := client.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools failed: %v", err)
	}
	if len(tools) != 2 || tools[0].Name != "pwd" || tools[1].Name != "date" {
		t.Fatalf("unexpected tools %+v", tools)
	}
}

func TestListToolsRejectsDuplicateAndMalformedDefinitions(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	duplicates := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		return result(t, request.ID, mcpproto.ListToolsResult{Tools: []mcpproto.Tool{
			{Name: "pwd", InputSchema: schema},
			{Name: "pwd", InputSchema: schema},
		}})
	})
	if _, err := duplicates.ListTools(context.Background()); err == nil {
		t.Fatal("expected duplicate tool definitions to be rejected")
	}

	malformed := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		return result(t, request.ID, mcpproto.ListToolsResult{Tools: []mcpproto.Tool{
			{Name: "pwd", InputSchema: json.RawMessage(`"not-an-object"`)},
		}})
	})
	if _, err := malformed.ListTools(context.Background()); err == nil {
		t.Fatal("expected a malformed schema to be rejected")
	}

	unnamed := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		return result(t, request.ID, mcpproto.ListToolsResult{Tools: []mcpproto.Tool{{InputSchema: schema}}})
	})
	if _, err := unnamed.ListTools(context.Background()); err == nil {
		t.Fatal("expected an unnamed tool to be rejected")
	}
}

func TestCallToolReturnsServerResult(t *testing.T) {
	client := scriptedServer(t, func(request mcpproto.Message) *mcpproto.Message {
		return result(t, request.ID, mcpproto.CallToolResult{
			Content: []mcpproto.Content{{Type: "text", Text: `{"success":true}`}},
		})
	})
	response, err := client.CallTool(context.Background(), "pwd", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("CallTool failed: %v", err)
	}
	if response.Text() != `{"success":true}` {
		t.Fatalf("unexpected result %q", response.Text())
	}
}

func TestCallRespectsContextCancellation(t *testing.T) {
	client := scriptedServer(t, func(mcpproto.Message) *mcpproto.Message { return nil })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := client.CallTool(ctx, "pwd", json.RawMessage(`{}`)); err == nil {
		t.Fatal("expected the call to be cancelled")
	}
}

func TestCloseTerminatesChildProcess(t *testing.T) {
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep is unavailable")
	}
	client, err := StartProcess(context.Background(), "sleep", []string{"120"}, t.TempDir(), io.Discard)
	if err != nil {
		t.Fatalf("StartProcess failed: %v", err)
	}
	done := make(chan struct{})
	go func() {
		client.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not terminate the child process")
	}
}
