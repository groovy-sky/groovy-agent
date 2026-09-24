package webutils

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

func TestNewServerFromEnvDisabledByDefault(t *testing.T) {
	t.Setenv(chromeControlURLEnvVar, "")
	server, err := NewServerFromEnv(DefaultLimits(), &fakeBrowser{}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatalf("NewServerFromEnv returned error: %v", err)
	}
	if got := server.ToolNames(); len(got) != 2 {
		t.Fatalf("expected default 2 tools, got %d (%v)", len(got), got)
	}
}

func TestNewServerFromEnvInvalidURL(t *testing.T) {
	t.Setenv(chromeControlURLEnvVar, "://bad")
	_, err := NewServerFromEnv(DefaultLimits(), &fakeBrowser{}, log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("expected invalid WEBUTILS_CHROME_CONTROL_URL to fail")
	}
}

func TestChromeControlResolvePathAndNoVNCURL(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:7777/prefix/")
	client := &chromeControlClient{baseURL: base, noVNCURL: "http://127.0.0.1:6080"}
	if got := client.resolvePath("/v1/sessions"); got != "http://127.0.0.1:7777/prefix/v1/sessions" {
		t.Fatalf("unexpected resolved path: %s", got)
	}
	if got := client.noVNCURLForHuman(); got != "http://127.0.0.1:6080/vnc.html" {
		t.Fatalf("unexpected noVNC URL: %s", got)
	}
}

func TestValidateSessionToken(t *testing.T) {
	valid := "tok_abcDEF123456"
	if err := validateSessionToken(valid); err != nil {
		t.Fatalf("expected valid token, got %v", err)
	}
	for _, invalid := range []string{"", "short", "bad/slash", "has space"} {
		if err := validateSessionToken(invalid); err == nil {
			t.Fatalf("expected token %q to be invalid", invalid)
		}
	}
}

func TestBrowserSessionSchemasClosed(t *testing.T) {
	server := newServer(DefaultLimits(), &fakeBrowser{}, &chromeControlClient{}, log.New(io.Discard, "", 0))
	tools := server.listTools().Tools
	found := map[string]json.RawMessage{}
	for _, tool := range tools {
		found[tool.Name] = tool.InputSchema
	}
	if _, ok := found[toolNameBrowserSessionCreate]; !ok {
		t.Fatalf("expected %s in tool list", toolNameBrowserSessionCreate)
	}
	schema := map[string]any{}
	if err := json.Unmarshal(found[toolNameBrowserSessionContinue], &schema); err != nil {
		t.Fatalf("unmarshal continue schema: %v", err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("expected closed continue schema, got %+v", schema)
	}
	required, ok := schema["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "token" {
		t.Fatalf("expected continue token requirement, got %+v", schema["required"])
	}
}

func TestBrowserSessionWorkflowWithHTTPEndpoints(t *testing.T) {
	var mu sync.Mutex
	statusByToken := map[string]map[string]any{}

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions":
			mu.Lock()
			statusByToken["tok_abc12345"] = map[string]any{"status": "waiting"}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"token":"tok_abc12345","status":"waiting"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/sessions/tok_abc12345":
			mu.Lock()
			status := statusByToken["tok_abc12345"]
			mu.Unlock()
			if status == nil {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"session not found"}`))
				return
			}
			if status["status"] == "completed" {
				_, _ = w.Write([]byte(`{"status":"completed","result":{"title":"Example","content":"hello","visible_text":"hello","links":[{"text":"A","url":"https://example.com"}],"screenshot":{"mime_type":"image/png"}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"waiting"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/sessions/tok_abc12345/continue":
			mu.Lock()
			statusByToken["tok_abc12345"] = map[string]any{"status": "completed"}
			mu.Unlock()
			_, _ = w.Write([]byte(`{"ok":true}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/sessions/tok_abc12345":
			mu.Lock()
			delete(statusByToken, "tok_abc12345")
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer backend.Close()

	baseURL, _ := url.Parse(backend.URL + "/api")
	server := newServer(DefaultLimits(), &fakeBrowser{}, &chromeControlClient{
		baseURL:  baseURL,
		noVNCURL: "http://127.0.0.1:6080",
		client:   backend.Client(),
		resolver: staticResolver{records: map[string][]netip.Addr{}},
	}, log.New(io.Discard, "", 0))

	create := server.callTool(context.Background(), json.RawMessage(`{"name":"browser_session_create","arguments":{"capture_screenshot":true}}`))
	if create.IsError {
		t.Fatalf("create failed: %+v", create)
	}
	if !strings.Contains(create.Text(), `"novnc_url":"http://127.0.0.1:6080/vnc.html"`) {
		t.Fatalf("expected noVNC URL in create result, got %s", create.Text())
	}
	if len(server.sessionTokens) != 1 {
		t.Fatalf("expected one tracked token after create, got %d", len(server.sessionTokens))
	}

	continueResult := server.callTool(context.Background(), json.RawMessage(`{"name":"browser_session_continue","arguments":{"token":"tok_abc12345","wait_for_completion":true,"max_wait_seconds":5}}`))
	if continueResult.IsError {
		t.Fatalf("continue failed: %+v", continueResult)
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(continueResult.Text()), &body); err != nil {
		t.Fatalf("decode continue result: %v", err)
	}
	session, ok := body["session"].(map[string]any)
	if !ok || session["status"] != "completed" {
		t.Fatalf("expected completed session, got %+v", body)
	}
	if len(server.sessionTokens) != 0 {
		t.Fatalf("expected token bookkeeping cleanup after completion, got %d", len(server.sessionTokens))
	}
}

func TestBrowserSessionErrorTranslation(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"forbidden"}`))
	}))
	defer backend.Close()

	baseURL, _ := url.Parse(backend.URL)
	server := newServer(DefaultLimits(), &fakeBrowser{}, &chromeControlClient{
		baseURL:  baseURL,
		client:   backend.Client(),
		resolver: defaultResolver(),
	}, log.New(io.Discard, "", 0))

	result := server.callTool(context.Background(), json.RawMessage(`{"name":"browser_session_status","arguments":{"token":"tok_abc12345"}}`))
	if !result.IsError {
		t.Fatalf("expected status call to fail")
	}
	body := map[string]any{}
	if err := json.Unmarshal([]byte(result.Text()), &body); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if body["error"] != mcpproto.ErrorPermissionDenied {
		t.Fatalf("expected permission_denied, got %+v", body)
	}
}
