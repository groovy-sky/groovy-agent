package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/approval"
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
	if _, err := applyUnifiedPatch(ws, patch); err != nil {
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
	t.Setenv("OPENAI_REQUEST_TIMEOUT", "-1s")

	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error")
	}
}

func TestClientFromEnvRequiresKey(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "")
	if _, err := clientFromEnv(); err == nil {
		t.Fatal("expected error")
	}
}
