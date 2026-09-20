package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCompleteSendsBoundedRequestAndParsesToolCalls(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %q", request.URL.Path)
		}
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Errorf("request is not JSON: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"pwd","arguments":"{}"}}]}}]}`)
	}))
	defer server.Close()

	client := New(server.URL, "local-phi-4-mini-instruct")
	tools := []Tool{{Type: "function", Function: Function{Name: "pwd", Parameters: map[string]any{"type": "object"}}}}
	message, err := client.Complete(context.Background(), []Message{{Role: "user", Content: "hi"}}, tools)
	if err != nil {
		t.Fatalf("Complete failed: %v", err)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Function.Name != "pwd" {
		t.Fatalf("unexpected message %+v", message)
	}
	if received["max_tokens"].(float64) != MaxOutputTokens {
		t.Fatalf("expected max_tokens=%d, got %v", MaxOutputTokens, received["max_tokens"])
	}
	if received["model"] != "local-phi-4-mini-instruct" || received["tool_choice"] != "auto" {
		t.Fatalf("unexpected request %+v", received)
	}
}

func TestCompleteSerializesPriorToolCallArgumentsAsStructuredJSON(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("request is not JSON: %v", err)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`)
	}))
	defer server.Close()

	client := New(server.URL, "local-phi-4-mini-instruct")
	messages := []Message{
		{Role: "user", Content: "hi"},
		{
			Role: "assistant",
			ToolCalls: []ToolCall{{
				ID:   "call_1",
				Type: "function",
				Function: FunctionCall{
					Name:      "webutils_search_web",
					Arguments: `{"query":"groovy-agent latest release notes"}`,
				},
			}},
		},
	}
	if _, err := client.Complete(context.Background(), messages, nil); err != nil {
		t.Fatalf("Complete failed: %v", err)
	}

	wireMessages := received["messages"].([]any)
	assistantMessage := wireMessages[1].(map[string]any)
	wireToolCalls := assistantMessage["tool_calls"].([]any)
	wireFunction := wireToolCalls[0].(map[string]any)["function"].(map[string]any)
	arguments, ok := wireFunction["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("expected structured JSON arguments, got %#v", wireFunction["arguments"])
	}
	if arguments["query"] != "groovy-agent latest release notes" {
		t.Fatalf("unexpected structured arguments %#v", arguments)
	}
}

func TestFunctionCallMarshalJSONKeepsPlainStringsQuoted(t *testing.T) {
	encoded, err := json.Marshal(FunctionCall{
		Name:      "tool",
		Arguments: "plain string argument",
	})
	if err != nil {
		t.Fatalf("Marshal failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("encoded output is not valid JSON: %v", err)
	}
	if decoded["arguments"] != "plain string argument" {
		t.Fatalf("expected plain string fallback, got %#v", decoded["arguments"])
	}
}

func TestCompleteRejectsErrorResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	if _, err := New(server.URL, "m").Complete(context.Background(), nil, nil); err == nil {
		t.Fatal("expected a failing status to be reported")
	}
}

func TestPingDetectsUnreachableServer(t *testing.T) {
	if err := New("http://127.0.0.1:1", "m").Ping(context.Background()); err == nil {
		t.Fatal("expected an unreachable server to be reported")
	}

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := New(server.URL, "m").Ping(context.Background()); err != nil {
		t.Fatalf("expected a healthy server: %v", err)
	}
}
