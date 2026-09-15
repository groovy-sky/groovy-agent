package openaiproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultToolsCacheTTL      = 10 * time.Second
	maxChatCompletionBodySize = 1 << 20
)

// Config configures the OpenAI-compatible proxy.
type Config struct {
	UpstreamURL               string
	MCPEnabled                bool
	MaxToolCallsPerTurn       int
	ToolsCacheTTL             time.Duration
	DisableDuplicateToolCalls bool
}

// Proxy forwards OpenAI-compatible requests while auto-exposing MCP tools.
type Proxy struct {
	upstream                  *url.URL
	reverse                   *httputil.ReverseProxy
	mcpEnabled                bool
	maxToolCallsPerTurn       int
	disableDuplicateToolCalls bool
	toolsCacheTTL             time.Duration
	logger                    *log.Logger

	mu               sync.Mutex
	cachedTools      []any
	cachedToolsAt    time.Time
	cachedToolsErr   error
	cachedToolsErrAt time.Time
}

func New(config Config, logger *log.Logger) (*Proxy, error) {
	parsed, err := url.Parse(strings.TrimSpace(config.UpstreamURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid upstream URL %q", config.UpstreamURL)
	}
	if logger == nil {
		logger = log.New(io.Discard, "", 0)
	}
	ttl := config.ToolsCacheTTL
	if ttl <= 0 {
		ttl = defaultToolsCacheTTL
	}
	maxCalls := config.MaxToolCallsPerTurn
	if maxCalls <= 0 {
		maxCalls = 3
	}
	reverse := httputil.NewSingleHostReverseProxy(parsed)
	reverse.ErrorLog = logger
	return &Proxy{
		upstream:                  parsed,
		reverse:                   reverse,
		mcpEnabled:                config.MCPEnabled,
		maxToolCallsPerTurn:       maxCalls,
		disableDuplicateToolCalls: config.DisableDuplicateToolCalls,
		toolsCacheTTL:             ttl,
		logger:                    logger,
	}, nil
}

func (p *Proxy) Handler() http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodPost && request.URL.Path == "/v1/chat/completions" {
			p.handleChatCompletions(writer, request)
			return
		}
		p.reverse.ServeHTTP(writer, request)
	})
}

func (p *Proxy) handleChatCompletions(writer http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(io.LimitReader(request.Body, maxChatCompletionBodySize+1))
	if err != nil {
		http.Error(writer, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = request.Body.Close()
	if len(body) > maxChatCompletionBodySize {
		http.Error(writer, "request body too large", http.StatusRequestEntityTooLarge)
		return
	}

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		p.forwardRaw(writer, request, body)
		return
	}

	if p.mcpEnabled && !hasUsableTools(payload) {
		tools, err := p.getOpenAITools(request.Context())
		if err != nil {
			p.writeDiagnosticCompletion(writer, payload, err)
			return
		}
		payload["tools"] = tools
		if _, hasChoice := payload["tool_choice"]; !hasChoice {
			payload["tool_choice"] = "auto"
		}
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		http.Error(writer, "request JSON encoding failed", http.StatusBadRequest)
		return
	}

	response, err := p.forwardJSON(request.Context(), request, encoded)
	if err != nil {
		http.Error(writer, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		http.Error(writer, "failed to read upstream response", http.StatusBadGateway)
		return
	}

	headers := writer.Header()
	for key := range headers {
		headers.Del(key)
	}
	copyHeaders(headers, response.Header)

	if response.StatusCode == http.StatusOK {
		guarded, changed := p.applyToolCallGuards(responseBody)
		if changed {
			responseBody = guarded
		}
	}

	writer.WriteHeader(response.StatusCode)
	_, _ = writer.Write(responseBody)
}

func (p *Proxy) forwardRaw(writer http.ResponseWriter, request *http.Request, body []byte) {
	response, err := p.forwardJSON(request.Context(), request, body)
	if err != nil {
		http.Error(writer, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer response.Body.Close()
	copyHeaders(writer.Header(), response.Header)
	writer.WriteHeader(response.StatusCode)
	_, _ = io.Copy(writer, response.Body)
}

func (p *Proxy) forwardJSON(ctx context.Context, request *http.Request, body []byte) (*http.Response, error) {
	target := *p.upstream
	target.Path = request.URL.Path
	target.RawPath = request.URL.RawPath
	target.RawQuery = request.URL.RawQuery

	forward, err := http.NewRequestWithContext(ctx, request.Method, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	copyHeaders(forward.Header, request.Header)
	forward.Host = p.upstream.Host
	forward.ContentLength = int64(len(body))

	return http.DefaultClient.Do(forward)
}

func copyHeaders(dst http.Header, src http.Header) {
	for key := range src {
		dst.Del(key)
		for _, value := range src.Values(key) {
			dst.Add(key, value)
		}
	}
}

func hasUsableTools(payload map[string]any) bool {
	toolsRaw, exists := payload["tools"]
	if !exists {
		return false
	}
	tools, ok := toolsRaw.([]any)
	return ok && len(tools) > 0
}

func (p *Proxy) getOpenAITools(ctx context.Context) ([]any, error) {
	now := time.Now()

	p.mu.Lock()
	if len(p.cachedTools) > 0 && now.Sub(p.cachedToolsAt) < p.toolsCacheTTL {
		cached := append([]any(nil), p.cachedTools...)
		p.mu.Unlock()
		return cached, nil
	}
	if p.cachedToolsErr != nil && now.Sub(p.cachedToolsErrAt) < time.Second {
		err := p.cachedToolsErr
		p.mu.Unlock()
		return nil, err
	}
	p.mu.Unlock()

	tools, err := p.fetchOpenAITools(ctx)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.cachedToolsErr = err
		p.cachedToolsErrAt = now
		return nil, err
	}
	p.cachedTools = append([]any(nil), tools...)
	p.cachedToolsAt = now
	p.cachedToolsErr = nil
	return append([]any(nil), p.cachedTools...), nil
}

func (p *Proxy) fetchOpenAITools(ctx context.Context) ([]any, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, p.upstream.String()+"/tools", nil)
	if err != nil {
		return nil, err
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("fetch /tools: %w", err)
	}
	defer response.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("read /tools response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("/tools returned status %d", response.StatusCode)
	}
	return parseOpenAITools(payload)
}

func parseOpenAITools(payload []byte) ([]any, error) {
	var decoded any
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return nil, fmt.Errorf("parse /tools JSON: %w", err)
	}
	items, err := extractToolItems(decoded)
	if err != nil {
		return nil, err
	}
	result := make([]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		adapted, ok := adaptTool(tool)
		if !ok {
			continue
		}
		result = append(result, adapted)
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("/tools returned no usable function definitions")
	}
	sort.SliceStable(result, func(i, j int) bool {
		left := result[i].(map[string]any)["function"].(map[string]any)["name"].(string)
		right := result[j].(map[string]any)["function"].(map[string]any)["name"].(string)
		return left < right
	})
	return result, nil
}

func extractToolItems(decoded any) ([]any, error) {
	if list, ok := decoded.([]any); ok {
		return list, nil
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("/tools response is not a JSON object or array")
	}
	if data, ok := object["data"].([]any); ok {
		return data, nil
	}
	if tools, ok := object["tools"].([]any); ok {
		return tools, nil
	}
	return nil, fmt.Errorf("/tools response does not include a tool list")
}

func adaptTool(tool map[string]any) (map[string]any, bool) {
	if kind, ok := tool["type"].(string); ok && kind == "function" {
		if function, ok := tool["function"].(map[string]any); ok {
			name, _ := function["name"].(string)
			if strings.TrimSpace(name) == "" {
				return nil, false
			}
			if _, ok := function["parameters"].(map[string]any); !ok {
				function["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
			}
			return map[string]any{"type": "function", "function": function}, true
		}
	}
	name, _ := tool["tool"].(string)
	if strings.TrimSpace(name) == "" {
		name, _ = tool["name"].(string)
	}
	if strings.TrimSpace(name) == "" {
		return nil, false
	}
	description, _ := tool["description"].(string)
	parameters := firstSchema(tool, "parameters", "input_schema", "inputSchema", "arguments", "schema")
	if parameters == nil {
		parameters = map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": true}
	}
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        name,
			"description": description,
			"parameters":  parameters,
		},
	}, true
}

func firstSchema(tool map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if schema, ok := tool[key].(map[string]any); ok {
			return schema
		}
	}
	if function, ok := tool["function"].(map[string]any); ok {
		if schema, ok := function["parameters"].(map[string]any); ok {
			return schema
		}
	}
	return nil
}

func (p *Proxy) writeDiagnosticCompletion(writer http.ResponseWriter, requestPayload map[string]any, err error) {
	message := "Tool bridge temporarily unavailable: MCP tools are enabled, but automatic tool exposure for this request failed. " +
		"Please retry shortly or call GET /tools to inspect currently available tools. Diagnostic: " + err.Error()
	p.logger.Printf("tool bridge error: %v", err)
	model, _ := requestPayload["model"].(string)
	response := map[string]any{
		"id":      "chatcmpl-tool-bridge-error",
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []map[string]any{{
			"index": 0,
			"message": map[string]any{
				"role":    "assistant",
				"content": message,
			},
			"finish_reason": "stop",
		}},
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(writer).Encode(response)
}

func (p *Proxy) applyToolCallGuards(responseBody []byte) ([]byte, bool) {
	var payload map[string]any
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return responseBody, false
	}
	choices, ok := payload["choices"].([]any)
	if !ok || len(choices) == 0 {
		return responseBody, false
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return responseBody, false
	}
	message, ok := choice["message"].(map[string]any)
	if !ok {
		return responseBody, false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return responseBody, false
	}

	seen := map[string]struct{}{}
	filtered := make([]any, 0, len(calls))
	guardReason := ""
	for _, raw := range calls {
		call, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if len(filtered) >= p.maxToolCallsPerTurn {
			guardReason = fmt.Sprintf("partial completion: limited to %d tool calls per assistant turn", p.maxToolCallsPerTurn)
			break
		}
		key := toolCallKey(call)
		if p.disableDuplicateToolCalls {
			if _, exists := seen[key]; exists {
				guardReason = "partial completion: duplicate tool call detected in the same assistant turn"
				break
			}
			seen[key] = struct{}{}
		}
		filtered = append(filtered, call)
	}
	if guardReason == "" {
		return responseBody, false
	}
	if len(filtered) == 0 {
		delete(message, "tool_calls")
		message["content"] = guardReason + ". Retry with a narrower request."
		choice["finish_reason"] = "stop"
	} else {
		message["tool_calls"] = filtered
		content, _ := message["content"].(string)
		if strings.TrimSpace(content) == "" {
			message["content"] = guardReason + ". Executing a bounded subset of tool calls."
		} else {
			message["content"] = strings.TrimSpace(content) + "\n\n" + guardReason + "."
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return responseBody, false
	}
	return encoded, true
}

func toolCallKey(call map[string]any) string {
	function, _ := call["function"].(map[string]any)
	name, _ := function["name"].(string)
	arguments, _ := function["arguments"].(string)
	return name + "\n" + strings.TrimSpace(arguments)
}
