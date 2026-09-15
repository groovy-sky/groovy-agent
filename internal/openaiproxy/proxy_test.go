package openaiproxy

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestProxyInjectsToolsForChatCompletionsWhenMissing(t *testing.T) {
	received := map[string]any{}
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/tools":
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"data":[{"tool":"webutils_search_web","description":"Search web","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}}]}`)
		case "/v1/chat/completions":
			if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer upstream.Close()

	proxy, err := New(Config{UpstreamURL: upstream.URL, MCPEnabled: true, MaxToolCallsPerTurn: 3, DisableDuplicateToolCalls: true}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	body := bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"search"}]}`)
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", response.StatusCode)
	}

	if received["tool_choice"] != "auto" {
		t.Fatalf("expected tool_choice auto, got %v", received["tool_choice"])
	}
	tools, ok := received["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("expected one injected tool, got %#v", received["tools"])
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "webutils_search_web" {
		t.Fatalf("unexpected injected tool name: %v", function["name"])
	}
}

func TestProxyReturnsDiagnosticWhenToolBridgeFails(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/tools" {
			http.Error(writer, "boom", http.StatusInternalServerError)
			return
		}
		http.NotFound(writer, request)
	}))
	defer upstream.Close()

	proxy, err := New(Config{UpstreamURL: upstream.URL, MCPEnabled: true}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	body := bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"browse"}]}`)
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()

	payload, _ := io.ReadAll(response.Body)
	if !bytes.Contains(payload, []byte("Tool bridge temporarily unavailable")) {
		t.Fatalf("expected diagnostic message, got %s", string(payload))
	}
	if !bytes.Contains(payload, []byte("/tools returned status 500")) {
		t.Fatalf("expected status detail, got %s", string(payload))
	}
}

func TestProxyAppliesPerTurnToolCallGuards(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/chat/completions" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"1","type":"function","function":{"name":"webutils_search_web","arguments":"{\"q\":\"a\"}"}},{"id":"2","type":"function","function":{"name":"webutils_search_web","arguments":"{\"q\":\"a\"}"}},{"id":"3","type":"function","function":{"name":"webutils_browse_url","arguments":"{\"url\":\"https://example.com\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if request.URL.Path == "/tools" {
			writer.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(writer, `{"data":[]}`)
			return
		}
		http.NotFound(writer, request)
	}))
	defer upstream.Close()

	proxy, err := New(Config{UpstreamURL: upstream.URL, MCPEnabled: false, MaxToolCallsPerTurn: 3, DisableDuplicateToolCalls: true}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("New failed: %v", err)
	}
	server := httptest.NewServer(proxy.Handler())
	defer server.Close()

	body := bytes.NewBufferString(`{"model":"m","messages":[{"role":"user","content":"browse"}],"tools":[{"type":"function","function":{"name":"webutils_search_web","parameters":{"type":"object"}}}]}`)
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer response.Body.Close()

	decoded := map[string]any{}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	choices := decoded["choices"].([]any)
	message := choices[0].(map[string]any)["message"].(map[string]any)
	calls := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("expected duplicate call guard to keep one call, got %d", len(calls))
	}
	content, _ := message["content"].(string)
	if content == "" || !bytes.Contains([]byte(content), []byte("partial completion")) {
		t.Fatalf("expected guard content note, got %q", content)
	}
}
