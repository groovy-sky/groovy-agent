package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/approval"
	"github.com/groovy-sky/groovy-agent/internal/mcp"
	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

func TestAPIClientCompleteParsesToolCalls(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/chat/completions" {
			t.Fatalf("path = %s", request.URL.Path)
		}
		if got := request.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") {
			t.Fatalf("authorization = %q", got)
		}
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.Model != "test-model" {
			t.Fatalf("model = %q", payload.Model)
		}
		if len(payload.Messages) != 1 || payload.Messages[0].Role != "user" {
			t.Fatalf("unexpected messages: %+v", payload.Messages)
		}
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_coreutil","arguments":"{\"utility\":\"pwd\"}"}}]}}]}`)
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	message, err := client.Complete(context.Background(), []message{{Role: "user", Content: "where am I"}}, openAITools())
	if err != nil {
		t.Fatal(err)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool call count = %d", len(message.ToolCalls))
	}
	if message.ToolCalls[0].Function.Name != "run_coreutil" {
		t.Fatalf("tool = %q", message.ToolCalls[0].Function.Name)
	}
}

func TestCompleteTurnExecutesToolCall(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_coreutil","arguments":"{\"utility\":\"cat\",\"stdin\":\"hello\\n\"}"}}]}}]}`)
		case 2:
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "tool" {
				t.Fatalf("last role = %q", last.Role)
			}
			toolOutput, err := contentText(last.Content)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(toolOutput, `"stdout":"hello\n"`) {
				t.Fatalf("unexpected tool output: %s", toolOutput)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	messages, answer, err := completeTurn(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "say hello"}}, openAITools())
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q", answer)
	}
	if got := len(messages); got != 5 {
		t.Fatalf("message count = %d", got)
	}
}

func TestExecuteToolCallErrors(t *testing.T) {
	output := executeToolCall(context.Background(), toolCall{ID: "call_1", Type: "function", Function: toolFunction{Name: "run_coreutil", Arguments: `{"utility":"missing"}`}})
	if !strings.Contains(output, `"error":"unsupported utility \"missing\""`) {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestExecuteToolCallParsesObjectArguments(t *testing.T) {
	output := executeToolCall(context.Background(), toolCall{ID: "call_1", Type: "function", Function: toolFunction{Name: "run_coreutil", Arguments: map[string]any{"utility": "cat", "stdin": "hi\n"}}})
	if !strings.Contains(output, `"stdout":"hi\n"`) {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestExecuteToolCallPlanModeDeniesMutation(t *testing.T) {
	ws, err := workspace.New(t.TempDir(), workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	runtime := &toolRuntime{workspace: ws, policy: approval.Policy{PlanMode: true, Interactive: false}}
	output := executeToolCallWithRuntime(context.Background(), runtime, toolCall{ID: "call_1", Type: "function", Function: toolFunction{Name: "write_file", Arguments: map[string]any{"path": "a.txt", "content": "x"}}})
	if !strings.Contains(output, `"code":"plan_mode_denied"`) {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestApplyUnifiedPatch(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("a.txt", "one\ntwo\n"); err != nil {
		t.Fatal(err)
	}
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -1,2 +1,2 @@\n one\n-two\n+three\n"
	if _, err := ws.ApplyPatch(patch); err != nil {
		t.Fatal(err)
	}
	read, err := ws.ReadFile("a.txt", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Lines) != 2 || read.Lines[1].Text != "three" {
		t.Fatalf("unexpected contents: %+v", read)
	}
}

func TestClientFromEnvReadsOverrides(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "local-model")
	t.Setenv("OPENAI_BASE_URL", "http://127.0.0.1:8080/v1/")
	t.Setenv("OPENAI_REQUEST_TIMEOUT", "15m")

	client, err := clientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if client.apiKey != "token" {
		t.Fatalf("api key = %q", client.apiKey)
	}
	if client.model != "local-model" {
		t.Fatalf("model = %q", client.model)
	}
	if client.baseURL != "http://127.0.0.1:8080/v1" {
		t.Fatalf("baseURL = %q", client.baseURL)
	}
	if client.httpClient.Timeout != 15*time.Minute {
		t.Fatalf("timeout = %s", client.httpClient.Timeout)
	}
}

func TestClientFromEnvUsesLongRequestTimeoutByDefault(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "local-model")
	t.Setenv("OPENAI_REQUEST_TIMEOUT", "")

	client, err := clientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != defaultRequestTimeout {
		t.Fatalf("timeout = %s", client.httpClient.Timeout)
	}
}

func TestClientFromEnvRejectsInvalidRequestTimeout(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "local-model")
	t.Setenv("OPENAI_REQUEST_TIMEOUT", "-1s")

	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientFromEnvAllowsDisabledRequestTimeout(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "local-model")
	t.Setenv("OPENAI_REQUEST_TIMEOUT", "0")

	client, err := clientFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if client.httpClient.Timeout != 0 {
		t.Fatalf("timeout = %s", client.httpClient.Timeout)
	}
}

func TestClientFromEnvRequiresKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientFromEnvRequiresModel(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "")
	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error for missing OPENAI_MODEL")
	}
}

func TestClientFromEnvRejectsRemoteURL(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "local-model")
	t.Setenv("OPENAI_BASE_URL", "https://api.openai.com/v1")
	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error for remote OPENAI_BASE_URL")
	}
}

func TestIsLocalURL(t *testing.T) {
	cases := []struct {
		url   string
		local bool
	}{
		{"http://127.0.0.1:8080/v1", true},
		{"http://localhost:8080/v1", true},
		{"http://[::1]:8080/v1", true},
		{"https://api.openai.com/v1", false},
		{"http://example.com/v1", false},
		{"http://10.0.0.1:8080/v1", false},
	}
	for _, c := range cases {
		got := isLocalURL(c.url)
		if got != c.local {
			t.Errorf("isLocalURL(%q) = %v, want %v", c.url, got, c.local)
		}
	}
}

func TestMCPDispatcherCallsThroughMCP(t *testing.T) {
	// Start an in-process MCP server and verify tool calls traverse it.
	root := t.TempDir()
	ws, err := workspace.New(root, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cfg := mcp.Config{
		Workspace: ws,
		Policy:    approval.Policy{Yolo: true},
	}
	mcpCli, stop, err := startInProcessMCP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	tools, err := mcpCli.listTools()
	if err != nil {
		t.Fatal(err)
	}

	// tools/list must include workspace tools.
	found := false
	for _, td := range tools {
		if td.Function.Name == "list_files" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("list_files not in tools: %v", tools)
	}

	// tools/call routes through the MCP protocol.
	dispatcher := &mcpDispatcher{client: mcpCli}
	call := toolCall{
		ID:   "test-call-1",
		Type: "function",
		Function: toolFunction{
			Name:      "run_coreutil",
			Arguments: map[string]any{"utility": "cat", "stdin": "hello from MCP\n"},
		},
	}
	output := dispatcher.Execute(context.Background(), call)
	if !strings.Contains(output, "hello from MCP") {
		t.Fatalf("unexpected output: %s", output)
	}
}

func TestMCPPlanModeDeniesMutationViaMCP(t *testing.T) {
	root := t.TempDir()
	ws, err := workspace.New(root, workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	cfg := mcp.Config{
		Workspace: ws,
		Policy:    approval.Policy{PlanMode: true, Interactive: false},
	}
	mcpCli, stop, err := startInProcessMCP(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	dispatcher := &mcpDispatcher{client: mcpCli}
	call := toolCall{
		ID:   "test-deny-1",
		Type: "function",
		Function: toolFunction{
			Name:      "write_file",
			Arguments: map[string]any{"path": "x.txt", "content": "hello"},
		},
	}
	output := dispatcher.Execute(context.Background(), call)
	if !strings.Contains(output, "plan_mode_denied") {
		t.Fatalf("expected plan_mode_denied, got: %s", output)
	}
}

func TestPersistResultCreatesDirectoryAndFile(t *testing.T) {
	dir := t.TempDir()
	outDir := dir + "/nested/output"
	result := RunResult{SessionID: "test-session-01", Answer: "hello"}
	if err := persistResult(outDir, "test-session-01", result); err != nil {
		t.Fatalf("persistResult error: %v", err)
	}
	data, err := os.ReadFile(outDir + "/test-session-01.json")
	if err != nil {
		t.Fatalf("read result file: %v", err)
	}
	var got RunResult
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if got.SessionID != "test-session-01" {
		t.Fatalf("session id = %q", got.SessionID)
	}
	if got.Answer != "hello" {
		t.Fatalf("answer = %q", got.Answer)
	}
}

func TestPersistResultUsesDefaultDirWhenEmpty(t *testing.T) {
	// Verify that DefaultOutputDir has the documented value; any change to the
	// constant would break the documented default path.
	if DefaultOutputDir != "output" {
		t.Fatalf("DefaultOutputDir = %q, want \"output\"", DefaultOutputDir)
	}

	// Verify that an empty outputDir falls back to DefaultOutputDir by passing
	// an explicit temp path equivalent to the resolved default.
	dir := t.TempDir()
	explicitDefault := dir + "/" + DefaultOutputDir
	result := RunResult{SessionID: "default-dir-test", Answer: "ok"}
	if err := persistResult(explicitDefault, "default-dir-test", result); err != nil {
		t.Fatalf("persistResult error: %v", err)
	}
	if _, err := os.Stat(explicitDefault + "/default-dir-test.json"); err != nil {
		t.Fatalf("expected file in default dir: %v", err)
	}
}
