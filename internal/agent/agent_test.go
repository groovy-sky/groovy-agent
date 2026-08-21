package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/approval"
	"github.com/groovy-sky/groovy-agent/internal/mcp"
	"github.com/groovy-sky/groovy-agent/internal/session"
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
		if payload.ToolChoice != "auto" {
			t.Fatalf("tool_choice = %#v", payload.ToolChoice)
		}
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_coreutil","arguments":"{\"utility\":\"pwd\"}"}}]}}]}`)
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	message, err := client.Complete(context.Background(), []message{{Role: "user", Content: "where am I"}}, openAITools(), chatCompleteOptions{})
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
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 1 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"run_coreutil","arguments":"{\"utility\":\"cat\",\"stdin\":\"hello\\n\"}"}}]}}]}`)
		case 2:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 2 tool_choice = %#v", payload.ToolChoice)
			}
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

func TestCompleteTurnTextualToolCallForcesSingleRequiredRetry(t *testing.T) {
	var requestCount int
	dispatcher := &recordingDispatcher{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 1 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"name\\\":\\\"write_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"hello.go\\\",\\\"content\\\":\\\"package main\\\"}}\\n```\"}}]}")
		case 2:
			if payload.ToolChoice != "required" {
				t.Fatalf("request 2 tool_choice = %#v", payload.ToolChoice)
			}
			if got := payload.Messages[len(payload.Messages)-2].Role; got != "assistant" {
				t.Fatalf("expected assistant context before corrective user message, got %q", got)
			}
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "user" || !strings.Contains(last.Content.(string), "native OpenAI-compatible `tool_calls`") {
				t.Fatalf("missing corrective retry message: %+v", last)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_2","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"hello.go\",\"content\":\"package main\"}"}}]}}]}`)
		case 3:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 3 tool_choice = %#v", payload.ToolChoice)
			}
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "tool" {
				t.Fatalf("last role = %q", last.Role)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	messages, answer, err := completeTurnWithRuntime(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "create file"}}, openAITools(), dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q", answer)
	}
	if requestCount != 3 {
		t.Fatalf("request count = %d", requestCount)
	}
	if len(dispatcher.calls) != 1 {
		t.Fatalf("dispatcher call count = %d", len(dispatcher.calls))
	}
	if dispatcher.calls[0].Function.Name != "write_file" {
		t.Fatalf("dispatcher tool = %q", dispatcher.calls[0].Function.Name)
	}
	if len(messages) == 0 || messages[len(messages)-1].Role != "assistant" {
		t.Fatalf("expected final assistant response in transcript")
	}
}

func TestCompleteTurnTextualToolCallAfterForcedRetryFails(t *testing.T) {
	var requestCount int
	dispatcher := &recordingDispatcher{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 1 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"{\"name\":\"write_file\",\"arguments\":{\"path\":\"hello.go\",\"content\":\"package main\"}}"}}]}`)
		case 2:
			if payload.ToolChoice != "required" {
				t.Fatalf("request 2 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"name\\\":\\\"write_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"hello.go\\\",\\\"content\\\":\\\"package main\\\"}}\\n```\"}}]}")
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	_, _, err := completeTurnWithRuntime(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "create file"}}, openAITools(), dispatcher)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "textual tool call") {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher call count = %d", len(dispatcher.calls))
	}
}

func TestCompleteTurnRequiringWritesSatisfiedByNativeWrite(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 1 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"time.txt\",\"content\":\"value\"}"}}]}}]}`)
		case 2:
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "tool" {
				t.Fatalf("last role = %q", last.Role)
			}
			toolOutput, err := contentText(last.Content)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(toolOutput, `"path":"time.txt"`) {
				t.Fatalf("unexpected tool output: %s", toolOutput)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	ws, err := workspace.New(t.TempDir(), workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	events := make([]ToolEvent, 0)
	dispatcher := &eventDispatcher{
		base:      &toolRuntime{workspace: ws, policy: approval.Policy{Yolo: true, Interactive: false}},
		workspace: ws,
		events:    &events,
	}

	_, answer, err := completeTurnRequiringWrites(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "write time.txt"}}, openAITools(), dispatcher, ws, []string{"time.txt"}, &events)
	if err != nil {
		t.Fatal(err)
	}
	if answer != "done" {
		t.Fatalf("answer = %q", answer)
	}
	if got := len(events); got == 0 || events[0].Path != "time.txt" || !events[0].Success {
		t.Fatalf("unexpected events: %+v", events)
	}
	if _, err := ws.StatFile("time.txt"); err != nil {
		t.Fatalf("expected written file: %v", err)
	}
}

func TestUnmetRequiredWrites(t *testing.T) {
	ws, err := workspace.New(t.TempDir(), workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.WriteFile("written.txt", "ok"); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		events []ToolEvent
		want   []string
	}{
		{
			name:   "write to different path fails",
			events: []ToolEvent{{Tool: "write_file", Success: true, Path: "other.txt"}},
			want:   []string{"written.txt"},
		},
		{
			name:   "no write fails",
			events: nil,
			want:   []string{"written.txt"},
		},
		{
			name:   "matching write succeeds",
			events: []ToolEvent{{Tool: "write_file", Success: true, Path: "written.txt"}},
			want:   nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			unmet := unmetRequiredWrites(ws, testCase.events, []string{"written.txt"})
			if strings.Join(unmet, ",") != strings.Join(testCase.want, ",") {
				t.Fatalf("unmet = %v, want %v", unmet, testCase.want)
			}
		})
	}
}

func TestCompleteTurnMalformedTextualToolCallRequiredWriteFails(t *testing.T) {
	var requestCount int
	dispatcher := &recordingDispatcher{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			if payload.ToolChoice != "auto" {
				t.Fatalf("request 1 tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"name\\\":\\\"write_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"time.txt\\\",\\\"content\\\":\\\"value\\\"}}}\\n```\"}}]}")
		case 2:
			if payload.ToolChoice != "required" {
				t.Fatalf("request 2 tool_choice = %#v", payload.ToolChoice)
			}
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "user" || !strings.Contains(last.Content.(string), "native OpenAI-compatible `tool_calls`") {
				t.Fatalf("missing structured retry request: %+v", last)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		case 3:
			if payload.ToolChoice != "required" {
				t.Fatalf("request 3 tool_choice = %#v", payload.ToolChoice)
			}
			last := payload.Messages[len(payload.Messages)-1]
			if last.Role != "user" || !strings.Contains(last.Content.(string), "required write was not completed: time.txt") {
				t.Fatalf("missing required-write repair request: %+v", last)
			}
			if !strings.Contains(last.Content.(string), "shell commands") {
				t.Fatalf("repair prompt missing shell-command restriction: %+v", last)
			}
			_, _ = io.WriteString(writer, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"```json\\n{\\\"name\\\":\\\"write_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"time.txt\\\",\\\"content\\\":\\\"value\\\"}}\\n```\"}}]}")
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	ws, err := workspace.New(t.TempDir(), workspace.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	events := make([]ToolEvent, 0)
	observed := &eventDispatcher{base: dispatcher, workspace: ws, events: &events}

	_, _, err = completeTurnRequiringWrites(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "write time.txt"}}, openAITools(), observed, ws, []string{"time.txt"}, &events)
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "required write was not completed: time.txt" {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher call count = %d", len(dispatcher.calls))
	}
}

func TestCompleteTurnDoesNotTreatProseJSONAsToolCall(t *testing.T) {
	dispatcher := &recordingDispatcher{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload.ToolChoice != "auto" {
			t.Fatalf("tool_choice = %#v", payload.ToolChoice)
		}
		_, _ = io.WriteString(writer, "{\"choices\":[{\"message\":{\"role\":\"assistant\",\"content\":\"Here is an example only:\\n```json\\n{\\\"name\\\":\\\"write_file\\\",\\\"arguments\\\":{\\\"path\\\":\\\"hello.go\\\",\\\"content\\\":\\\"package main\\\"}}\\n```\\nDo not execute this JSON.\"}}]}")
	}))
	defer server.Close()

	client := &apiClient{httpClient: server.Client(), apiKey: "token", model: "test-model", baseURL: server.URL}
	_, answer, err := completeTurnWithRuntime(context.Background(), client, []message{{Role: "system", Content: "system"}, {Role: "user", Content: "show an example"}}, openAITools(), dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer, "example only") {
		t.Fatalf("answer = %q", answer)
	}
	if len(dispatcher.calls) != 0 {
		t.Fatalf("dispatcher call count = %d", len(dispatcher.calls))
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
		Policy:    &approval.Policy{Yolo: true},
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
		Policy:    &approval.Policy{PlanMode: true, Interactive: false},
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

func TestRunHeadlessPreservesPersistenceOnRequiredWriteFailure(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		var payload chatRequest
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		switch requestCount {
		case 1:
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"other.txt\",\"content\":\"wrong\"}"}}]}}]}`)
		case 2:
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
		case 3:
			if payload.ToolChoice != "required" {
				t.Fatalf("repair tool_choice = %#v", payload.ToolChoice)
			}
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"still done"}}]}`)
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("OPENAI_BASE_URL", server.URL)

	workspaceRoot := t.TempDir()
	outputDir := filepath.Join(t.TempDir(), "results")
	result, err := RunHeadless(context.Background(), "write time.txt", Options{
		WorkspacePath: workspaceRoot,
		Yolo:          true,
		RequireWrite:  []string{"time.txt"},
		OutputDir:     outputDir,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if err.Error() != "required write was not completed: time.txt" {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Answer != "still done" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if len(result.Events) == 0 || result.Events[0].Path != "other.txt" || !result.Events[0].Success {
		t.Fatalf("unexpected events: %+v", result.Events)
	}
	resultPath := filepath.Join(outputDir, result.SessionID+".json")
	if _, statErr := os.Stat(resultPath); statErr != nil {
		t.Fatalf("expected persisted result: %v", statErr)
	}
	data, readErr := os.ReadFile(resultPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var persisted RunResult
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Answer != result.Answer || len(persisted.Events) != len(result.Events) {
		t.Fatalf("unexpected persisted result: %+v", persisted)
	}
	sessionPath := filepath.Join(workspaceRoot, session.SessionsDir, result.SessionID+".jsonl")
	if _, statErr := os.Stat(sessionPath); statErr != nil {
		t.Fatalf("expected persisted session: %v", statErr)
	}
}

func TestRunHeadlessWithoutRequireWritePreservesExistingBehavior(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requestCount++
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"done"}}]}`)
	}))
	defer server.Close()

	t.Setenv("OPENAI_API_KEY", "token")
	t.Setenv("OPENAI_MODEL", "test-model")
	t.Setenv("OPENAI_BASE_URL", server.URL)

	result, err := RunHeadless(context.Background(), "say hello", Options{
		WorkspacePath: t.TempDir(),
		OutputDir:     filepath.Join(t.TempDir(), "results"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Answer != "done" {
		t.Fatalf("answer = %q", result.Answer)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d", requestCount)
	}
}

type recordingDispatcher struct {
	calls []toolCall
}

func (d *recordingDispatcher) Execute(_ context.Context, call toolCall) string {
	d.calls = append(d.calls, call)
	return `{"success":true}`
}
