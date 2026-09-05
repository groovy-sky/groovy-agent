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
