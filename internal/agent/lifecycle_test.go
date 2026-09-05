package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// buildCoreutilsMCP builds the MCP server executable used by the end-to-end
// lifecycle test.
func buildCoreutilsMCP(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("the go toolchain is unavailable")
	}
	binary := filepath.Join(t.TempDir(), "coreutils-mcp")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.Command("go", "build", "-o", binary, "github.com/groovy-sky/groovy-agent/cmd/coreutils-mcp")
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build coreutils-mcp: %v", err)
	}
	return binary
}

// fakeLlamaServer answers /health and returns the queued chat completions.
func fakeLlamaServer(t *testing.T, replies []map[string]any) *httptest.Server {
	t.Helper()
	index := 0
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health" {
			writer.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(writer, `{"status":"ok"}`)
			return
		}
		if request.URL.Path != "/v1/chat/completions" {
			writer.WriteHeader(http.StatusNotFound)
			return
		}
		body := map[string]any{}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("model request is not JSON: %v", err)
		}
		if maxTokens, _ := body["max_tokens"].(float64); maxTokens != 256 {
			t.Errorf("expected a 256-token output limit, got %v", body["max_tokens"])
		}
		message := map[string]any{"role": "assistant", "content": "done"}
		if index < len(replies) {
			message = replies[index]
		}
		index++
		writer.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []map[string]any{{"message": message, "finish_reason": "stop"}},
		})
	}))
}

func TestRunEndToEndWithChildProcess(t *testing.T) {
	binary := buildCoreutilsMCP(t)
	workspace := t.TempDir()
	if err := os.WriteFile(filepath.Join(workspace, "README.md"), []byte("# Title\nbody\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	server := fakeLlamaServer(t, []map[string]any{
		{
			"role": "assistant",
			"tool_calls": []map[string]any{{
				"id":       "call-1",
				"type":     "function",
				"function": map[string]any{"name": "head", "arguments": `{"path":"README.md","lines":1}`},
			}},
		},
		{"role": "assistant", "content": "The workspace README starts with \"# Title\"."},
	})
	defer server.Close()

	stdout := &strings.Builder{}
	stderr := &strings.Builder{}
	config := Config{
		LlamaURL:   server.URL,
		Model:      "local-qwen2.5",
		MCPCommand: binary,
		Workspace:  workspace,
		Prompt:     "Show the workspace path and identify the likely README.",
	}
	if err := Run(context.Background(), config, stdout, stderr); err != nil {
		t.Fatalf("Run failed: %v (stderr: %s)", err, stderr.String())
	}
	if !strings.Contains(stdout.String(), "# Title") {
		t.Fatalf("unexpected final answer %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "agent:") {
		t.Fatalf("diagnostics leaked to stdout: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "profile=") {
		t.Fatalf("diagnostics are missing from stderr: %q", stderr.String())
	}
}

func TestRunFailsWhenLlamaServerIsUnreachable(t *testing.T) {
	config := Config{
		LlamaURL:   "http://127.0.0.1:1",
		Model:      "local-qwen2.5",
		MCPCommand: buildCoreutilsMCP(t),
		Workspace:  t.TempDir(),
		Prompt:     "hello",
	}
	err := Run(context.Background(), config, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "llama-server is not reachable") {
		t.Fatalf("expected an unreachable llama-server error, got %v", err)
	}
}

func TestRunCancellationLeavesNoChildProcess(t *testing.T) {
	binary := buildCoreutilsMCP(t)
	workspace := t.TempDir()

	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/health" {
			writer.WriteHeader(http.StatusOK)
			return
		}
		<-release
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	config := Config{
		LlamaURL:   server.URL,
		Model:      "local-qwen2.5",
		MCPCommand: binary,
		Workspace:  workspace,
		Prompt:     "Show the current workspace path.",
	}
	done := make(chan error, 1)
	go func() { done <- Run(ctx, config, io.Discard, io.Discard) }()

	// Cancel while the model request is in flight.
	cancel()
	if err := <-done; err == nil {
		t.Fatal("expected cancellation to abort the run")
	}
	if pgrepChildren(t, binary) {
		t.Fatal("a coreutils-mcp child process survived cancellation")
	}
}

// pgrepChildren reports whether a process with the given name is still running.
func pgrepChildren(t *testing.T, name string) bool {
	t.Helper()
	if _, err := exec.LookPath("pgrep"); err != nil {
		return false
	}
	output, err := exec.Command("pgrep", "-f", name).Output()
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(output)) != ""
}
