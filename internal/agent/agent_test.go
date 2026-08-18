package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

	client := &apiClient{
		httpClient: server.Client(),
		apiKey:     "token",
		model:      "test-model",
		baseURL:    server.URL,
	}
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

	client := &apiClient{
		httpClient: server.Client(),
		apiKey:     "token",
		model:      "test-model",
		baseURL:    server.URL,
	}
	messages, answer, err := completeTurn(
		context.Background(),
		client,
		[]message{
			{Role: "system", Content: "system"},
			{Role: "user", Content: "say hello"},
		},
		openAITools(),
	)
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
	_, err := executeToolCall(context.Background(), toolCall{
		ID:   "call_1",
		Type: "function",
		Function: toolFunction{
			Name:      "run_coreutil",
			Arguments: `{"utility":"missing"}`,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "unsupported utility") {
		t.Fatalf("unexpected error: %v", err)
	}
}
