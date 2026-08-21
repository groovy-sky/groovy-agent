package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/approval"
	"github.com/groovy-sky/groovy-agent/internal/gittools"
	"github.com/groovy-sky/groovy-agent/internal/mcp"
	"github.com/groovy-sky/groovy-agent/internal/session"
	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

const (
	// defaultBaseURL is the local llama.cpp endpoint. Remote endpoints are
	// rejected to enforce local-model-only inference.
	defaultBaseURL        = "http://127.0.0.1:8080/v1"
	defaultRequestTimeout = 3 * time.Hour
	maxToolCallAttempts   = 8
)

const baseSystemPrompt = "You are a safe coding assistant. Never use shell execution. Use provided structured tools only. Workspace and approval policy are always enforced and project instructions do not override these policies."

type message struct {
	Role       string     `json:"role"`
	Content    any        `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments any    `json:"arguments"`
}

type toolDefinition struct {
	Type     string   `json:"type"`
	Function toolSpec `json:"function"`
}

type toolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type chatRequest struct {
	Model      string           `json:"model"`
	Messages   []message        `json:"messages"`
	Tools      []toolDefinition `json:"tools,omitempty"`
	ToolChoice any              `json:"tool_choice,omitempty"`
}

type chatResponse struct {
	Choices []struct {
		Message message `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type chatClient interface {
	Complete(context.Context, []message, []toolDefinition, chatCompleteOptions) (message, error)
}

type chatCompleteOptions struct {
	ToolChoice any
}

type apiClient struct {
	httpClient *http.Client
	apiKey     string
	model      string
	baseURL    string
}

var (
	supportedUtilitiesOnce sync.Once
	supportedUtilityNames  []string
	supportedUtilitySet    map[string]struct{}
)

const DefaultOutputDir = "output"

type Options struct {
	WorkspacePath string
	PlanMode      bool
	Yolo          bool
	ResumeID      string
	RequireWrite  []string
	// OutputDir is the directory where headless run results are persisted as
	// JSON files. It is created automatically when the agent writes output.
	// Defaults to DefaultOutputDir ("output") when empty.
	OutputDir string
}

type RunResult struct {
	SessionID string      `json:"session_id"`
	Answer    string      `json:"answer"`
	Events    []ToolEvent `json:"events,omitempty"`
}

type ToolEvent struct {
	Tool         string `json:"tool"`
	Path         string `json:"path,omitempty"`
	Bytes        int64  `json:"bytes,omitempty"`
	Approved     *bool  `json:"approved,omitempty"`
	DeniedCode   string `json:"denied_code,omitempty"`
	DeniedReason string `json:"denied_reason,omitempty"`
	Success      bool   `json:"success"`
}

type toolRuntime struct {
	workspace *workspace.Workspace
	policy    approval.Policy
	prompt    func(preview string) (bool, error)
	events    *[]ToolEvent
}

// toolDispatcher routes model tool calls to an implementation.
type toolDispatcher interface {
	Execute(ctx context.Context, call toolCall) string
}

// Execute makes toolRuntime implement toolDispatcher for unit tests that
// bypass the MCP transport.
func (runtime *toolRuntime) Execute(ctx context.Context, call toolCall) string {
	return executeToolCallWithRuntime(ctx, runtime, call)
}

// mcpClient is a JSON-RPC 2.0 MCP client that communicates over a pair of
// io pipes connecting to an in-process MCP server goroutine.
type mcpClient struct {
	enc *json.Encoder
	dec *json.Decoder
	mu  sync.Mutex
	id  int
}

func newMCPClient(r io.Reader, w io.Writer) *mcpClient {
	return &mcpClient{enc: json.NewEncoder(w), dec: json.NewDecoder(r)}
}

// rpcCall sends a JSON-RPC request and returns the result.
func (c *mcpClient) rpcCall(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.id++
	type req struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	if err := c.enc.Encode(req{JSONRPC: "2.0", ID: c.id, Method: method, Params: params}); err != nil {
		return nil, fmt.Errorf("MCP send %s: %w", method, err)
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := c.dec.Decode(&resp); err != nil {
		return nil, fmt.Errorf("MCP recv %s: %w", method, err)
	}
	if resp.Error != nil {
		return nil, fmt.Errorf("MCP error %d: %s", resp.Error.Code, resp.Error.Message)
	}
	return resp.Result, nil
}

// rpcNotify sends a JSON-RPC notification (no response expected).
func (c *mcpClient) rpcNotify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	type notif struct {
		JSONRPC string `json:"jsonrpc"`
		Method  string `json:"method"`
		Params  any    `json:"params,omitempty"`
	}
	return c.enc.Encode(notif{JSONRPC: "2.0", Method: method, Params: params})
}

func (c *mcpClient) initialize() error {
	_, err := c.rpcCall("initialize", map[string]any{
		"protocolVersion": "2025-06-18",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "groovy-agent", "version": "0.1.0"},
	})
	if err != nil {
		return err
	}
	return c.rpcNotify("notifications/initialized", nil)
}

func (c *mcpClient) listTools() ([]toolDefinition, error) {
	result, err := c.rpcCall("tools/list", nil)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"inputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	tools := make([]toolDefinition, 0, len(resp.Tools))
	for _, t := range resp.Tools {
		tools = append(tools, functionTool(t.Name, t.Description, t.InputSchema))
	}
	return tools, nil
}

func (c *mcpClient) callTool(_ context.Context, name string, args any) (string, error) {
	// Normalise args to json.RawMessage.
	var argsRaw json.RawMessage
	switch v := args.(type) {
	case string:
		if v == "" {
			argsRaw = json.RawMessage("{}")
		} else {
			argsRaw = json.RawMessage(v)
		}
	case nil:
		argsRaw = json.RawMessage("{}")
	default:
		data, err := json.Marshal(v)
		if err != nil {
			return "", err
		}
		argsRaw = data
	}
	result, err := c.rpcCall("tools/call", map[string]any{
		"name":      name,
		"arguments": argsRaw,
	})
	if err != nil {
		return "", err
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(result, &resp); err != nil {
		return "", fmt.Errorf("parse tools/call: %w", err)
	}
	var parts []string
	for _, c := range resp.Content {
		if c.Type == "text" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, ""), nil
}

// mcpDispatcher routes model tool calls through the in-process MCP client.
type mcpDispatcher struct {
	client *mcpClient
}

func (d *mcpDispatcher) Execute(ctx context.Context, call toolCall) string {
	output, err := d.client.callTool(ctx, call.Function.Name, call.Function.Arguments)
	if err != nil {
		result := toolResult{Success: false, Error: fmt.Sprintf("MCP tool call failed: %v", err)}
		return marshalToolResult(result)
	}
	return output
}

type eventDispatcher struct {
	base      toolDispatcher
	workspace *workspace.Workspace
	events    *[]ToolEvent
}

func (dispatcher *eventDispatcher) Execute(ctx context.Context, call toolCall) string {
	output := dispatcher.base.Execute(ctx, call)
	recordEvent(dispatcher.events, buildToolEvent(dispatcher.workspace, call, output))
	return output
}

// startInProcessMCP starts an MCP server goroutine connected via in-process
// pipes. It returns the client (already initialized) and a stop function.
// The server shuts down when stop is called or ctx is cancelled.
func startInProcessMCP(ctx context.Context, cfg mcp.Config) (*mcpClient, func(), error) {
	serverInR, serverInW := io.Pipe()
	serverOutR, serverOutW := io.Pipe()

	srvCtx, cancel := context.WithCancel(ctx)
	go func() {
		defer serverOutW.Close()
		_ = mcp.ServeWithConfig(srvCtx, serverInR, serverOutW, cfg)
	}()

	client := newMCPClient(serverOutR, serverInW)
	stop := func() {
		cancel()
		_ = serverInW.Close()
	}
	if err := client.initialize(); err != nil {
		stop()
		return nil, nil, fmt.Errorf("MCP initialize: %w", err)
	}
	return client, stop, nil
}

type interactiveState struct {
	sessionID string
	createdAt time.Time
	messages  []message
}

// Run starts interactive terminal agent mode.
func Run(ctx context.Context, input io.Reader, output, errOutput io.Writer, options Options) error {
	chatClient, err := clientFromEnv()
	if err != nil {
		return err
	}
	workspaceRoot := options.WorkspacePath
	if strings.TrimSpace(workspaceRoot) == "" {
		workspaceRoot, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	ws, err := workspace.New(workspaceRoot, workspace.DefaultLimits())
	if err != nil {
		return err
	}
	requiredWrites, err := normalizeRequiredWrites(ws, options.RequireWrite)
	if err != nil {
		return err
	}
	store := session.NewStore(ws.Root)
	instructionText, instructionSources, err := store.LoadProjectInstructions()
	if err != nil {
		return err
	}
	baseMessages := []message{{Role: "system", Content: buildSystemPrompt(instructionText, instructionSources)}}

	state := interactiveState{sessionID: session.NewSessionID(time.Now()), createdAt: time.Now(), messages: baseMessages}
	if strings.TrimSpace(options.ResumeID) != "" {
		loaded, loadedAt, loadErr := loadSessionMessages(store, options.ResumeID)
		if loadErr != nil {
			return loadErr
		}
		state.sessionID = options.ResumeID
		state.createdAt = loadedAt
		state.messages = loaded
	}

	lineReader := newLineReader(input)
	policy := approval.Policy{PlanMode: options.PlanMode, Yolo: options.Yolo, Interactive: true}

	promptFn := func(preview string) (bool, error) {
		if strings.TrimSpace(preview) != "" {
			fmt.Fprintln(output, preview)
		}
		answer, promptErr := lineReader.ReadLine("Approve mutation? [y/N]: ", output)
		if promptErr != nil {
			return false, promptErr
		}
		answer = strings.ToLower(strings.TrimSpace(answer))
		return answer == "y" || answer == "yes", nil
	}

	// Start the in-process MCP server once before the loop.
	// Config.Policy is a pointer so plan-mode changes made via /plan take effect
	// immediately without restarting the server.
	mcpCfg := mcp.Config{
		Workspace: ws,
		Policy:    &policy,
		Prompt:    promptFn,
	}
	mcpCli, stopMCP, mcpErr := startInProcessMCP(ctx, mcpCfg)
	if mcpErr != nil {
		return fmt.Errorf("MCP start error: %w", mcpErr)
	}
	defer stopMCP()
	tools, mcpErr := mcpCli.listTools()
	if mcpErr != nil {
		return fmt.Errorf("MCP list tools error: %w", mcpErr)
	}
	dispatcher := &mcpDispatcher{client: mcpCli}

	fmt.Fprintln(output, "groovy-agent agent mode. Type 'exit' to quit. Use /help for commands.")
	for {
		line, readErr := lineReader.ReadLine("", output)
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if strings.HasPrefix(line, "/") {
			handled, cmdErr := handleSlashCommand(line, &state, baseMessages, &policy, ws, store, output)
			if cmdErr != nil {
				fmt.Fprintf(errOutput, "command error: %v\n", cmdErr)
			}
			if handled {
				continue
			}
		}

		turn := append(append([]message{}, state.messages...), message{Role: "user", Content: line})
		events := make([]ToolEvent, 0)
		recordingDispatcher := &eventDispatcher{base: dispatcher, workspace: ws, events: &events}
		updated, assistantAnswer, turnErr := completeTurnRequiringWrites(ctx, chatClient, turn, tools, recordingDispatcher, ws, requiredWrites, &events)
		if turnErr != nil {
			fmt.Fprintf(errOutput, "agent error: %v\n", turnErr)
			continue
		}
		state.messages = updated
		_ = storeSnapshot(store, state.sessionID, state.createdAt, state.messages)
		fmt.Fprintln(output, assistantAnswer)
	}
}

func RunHeadless(ctx context.Context, prompt string, options Options) (RunResult, error) {
	if strings.TrimSpace(prompt) == "" {
		return RunResult{}, errors.New("prompt is required")
	}
	chatClient, err := clientFromEnv()
	if err != nil {
		return RunResult{}, err
	}
	ws, err := workspace.New(options.WorkspacePath, workspace.DefaultLimits())
	if err != nil {
		return RunResult{}, err
	}
	requiredWrites, err := normalizeRequiredWrites(ws, options.RequireWrite)
	if err != nil {
		return RunResult{}, err
	}
	store := session.NewStore(ws.Root)
	instructionText, instructionSources, err := store.LoadProjectInstructions()
	if err != nil {
		return RunResult{}, err
	}
	messages := []message{{Role: "system", Content: buildSystemPrompt(instructionText, instructionSources)}}
	sessionID := session.NewSessionID(time.Now())
	createdAt := time.Now()
	if strings.TrimSpace(options.ResumeID) != "" {
		loaded, loadedAt, loadErr := loadSessionMessages(store, options.ResumeID)
		if loadErr != nil {
			return RunResult{}, loadErr
		}
		sessionID = options.ResumeID
		createdAt = loadedAt
		messages = loaded
	}
	turn := append(messages, message{Role: "user", Content: prompt})
	events := make([]ToolEvent, 0)
	policy := approval.Policy{PlanMode: options.PlanMode, Yolo: options.Yolo, Interactive: false}
	mcpCfg := mcp.Config{
		Workspace: ws,
		Policy:    &policy,
	}
	mcpCli, stopMCP, mcpErr := startInProcessMCP(ctx, mcpCfg)
	if mcpErr != nil {
		return RunResult{SessionID: sessionID, Events: events}, mcpErr
	}
	defer stopMCP()
	tools, mcpErr := mcpCli.listTools()
	if mcpErr != nil {
		return RunResult{SessionID: sessionID, Events: events}, mcpErr
	}
	dispatcher := &eventDispatcher{base: &mcpDispatcher{client: mcpCli}, workspace: ws, events: &events}
	updated, answer, err := completeTurnRequiringWrites(ctx, chatClient, turn, tools, dispatcher, ws, requiredWrites, &events)
	if err != nil {
		result := RunResult{SessionID: sessionID, Answer: answer, Events: events}
		if len(updated) > 0 {
			_ = storeSnapshot(store, sessionID, createdAt, updated)
			_ = persistResult(options.OutputDir, sessionID, result)
		}
		return result, err
	}
	for _, event := range events {
		if event.DeniedCode == "approval_required_non_interactive" || event.DeniedCode == "plan_mode_denied" || event.DeniedCode == "approval_denied" {
			_ = storeSnapshot(store, sessionID, createdAt, updated)
			deniedResult := RunResult{SessionID: sessionID, Answer: answer, Events: events}
			_ = persistResult(options.OutputDir, sessionID, deniedResult)
			return deniedResult, errors.New(event.DeniedCode)
		}
	}
	_ = storeSnapshot(store, sessionID, createdAt, updated)
	result := RunResult{SessionID: sessionID, Answer: answer, Events: events}
	_ = persistResult(options.OutputDir, sessionID, result)
	return result, nil
}

// persistResult writes a JSON file for result into outputDir, creating the
// directory automatically. Errors are non-fatal so that a missing or
// unwritable output directory never prevents the agent from returning its
// answer.
func persistResult(outputDir, sessionID string, result RunResult) error {
	if outputDir == "" {
		outputDir = DefaultOutputDir
	}
	if err := os.MkdirAll(outputDir, 0o700); err != nil {
		return fmt.Errorf("create output directory: %w", err)
	}
	path := filepath.Join(outputDir, sessionID+".json")
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write result file: %w", err)
	}
	return nil
}

func handleSlashCommand(command string, state *interactiveState, baseMessages []message, policy *approval.Policy, ws *workspace.Workspace, store *session.Store, output io.Writer) (bool, error) {
	fields := strings.Fields(command)
	switch fields[0] {
	case "/help":
		fmt.Fprintln(output, "/help /status /diff /plan /clear /session /resume <id>")
		return true, nil
	case "/status":
		fmt.Fprintf(output, "workspace=%s plan=%v yolo=%v session=%s\n", ws.Root, policy.PlanMode, policy.Yolo, state.sessionID)
		return true, nil
	case "/diff":
		diff, err := gittools.Diff(ws.Root, ws.Limits.MaxOutputBytes)
		if err != nil {
			return true, err
		}
		if strings.TrimSpace(diff) == "" {
			diff = "(no diff)"
		}
		fmt.Fprintln(output, diff)
		return true, nil
	case "/plan":
		policy.PlanMode = !policy.PlanMode
		fmt.Fprintf(output, "plan mode now %v\n", policy.PlanMode)
		return true, nil
	case "/clear":
		state.messages = append([]message{}, baseMessages...)
		_ = storeSnapshot(store, state.sessionID, state.createdAt, state.messages)
		fmt.Fprintln(output, "conversation cleared")
		return true, nil
	case "/session":
		fmt.Fprintf(output, "session=%s\n", state.sessionID)
		return true, nil
	case "/resume":
		if len(fields) < 2 {
			return true, errors.New("usage: /resume <session-id>")
		}
		loaded, loadedAt, err := loadSessionMessages(store, fields[1])
		if err != nil {
			return true, err
		}
		state.sessionID = fields[1]
		state.createdAt = loadedAt
		state.messages = loaded
		fmt.Fprintf(output, "resumed %s\n", fields[1])
		return true, nil
	default:
		return false, nil
	}
}

type lineReader struct {
	reader *bufio.Reader
	mu     sync.Mutex
}

func newLineReader(input io.Reader) *lineReader {
	return &lineReader{reader: bufio.NewReader(input)}
}

func (reader *lineReader) ReadLine(prompt string, output io.Writer) (string, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	if prompt == "" {
		prompt = "> "
	}
	if _, err := io.WriteString(output, prompt); err != nil {
		return "", err
	}
	line, err := reader.reader.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line != "" {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func clientFromEnv() (*apiClient, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required")
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		return nil, errors.New("OPENAI_MODEL is required: set it to the local model alias configured in llama-server")
	}
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if !isLocalURL(baseURL) {
		return nil, fmt.Errorf("OPENAI_BASE_URL %q is not a local endpoint: only loopback addresses (127.0.0.1, localhost, ::1) are allowed", baseURL)
	}
	requestTimeout := defaultRequestTimeout
	if value := strings.TrimSpace(os.Getenv("OPENAI_REQUEST_TIMEOUT")); value != "" {
		parsedTimeout, err := time.ParseDuration(value)
		if err != nil || parsedTimeout < 0 {
			return nil, fmt.Errorf("invalid OPENAI_REQUEST_TIMEOUT %q: expected a non-negative duration", value)
		}
		requestTimeout = parsedTimeout
	}
	return &apiClient{
		httpClient: &http.Client{Timeout: requestTimeout},
		apiKey:     apiKey,
		model:      model,
		baseURL:    strings.TrimRight(baseURL, "/"),
	}, nil
}

// isLocalURL returns true if rawURL's host is a loopback address.
// It accepts 127.0.0.1, localhost, ::1, and any port on those hosts.
// httptest.NewServer also binds to 127.0.0.1, so test URLs pass as well.
func isLocalURL(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := u.Hostname()
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}

func (client *apiClient) Complete(ctx context.Context, messages []message, tools []toolDefinition, options chatCompleteOptions) (message, error) {
	toolChoice := options.ToolChoice
	if len(tools) > 0 && toolChoice == nil {
		toolChoice = "auto"
	}
	requestBody, err := json.Marshal(chatRequest{Model: client.model, Messages: messages, Tools: tools, ToolChoice: toolChoice})
	if err != nil {
		return message{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, client.baseURL+"/chat/completions", bytes.NewReader(requestBody))
	if err != nil {
		return message{}, err
	}
	request.Header.Set("Authorization", "Bearer "+client.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := client.httpClient.Do(request)
	if err != nil {
		return message{}, err
	}
	defer response.Body.Close()
	responseBody, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		return message{}, fmt.Errorf("read response: %w", readErr)
	}
	if response.StatusCode >= http.StatusBadRequest {
		var payload chatResponse
		_ = json.Unmarshal(responseBody, &payload)
		if payload.Error != nil && payload.Error.Message != "" {
			return message{}, fmt.Errorf("api error: status %d: %s", response.StatusCode, payload.Error.Message)
		}
		text := strings.TrimSpace(string(responseBody))
		if text == "" {
			return message{}, fmt.Errorf("api error: status %d", response.StatusCode)
		}
		return message{}, fmt.Errorf("api error: status %d: %s", response.StatusCode, text)
	}
	var payload chatResponse
	if err := json.Unmarshal(responseBody, &payload); err != nil {
		return message{}, fmt.Errorf("decode response: %w", err)
	}
	if len(payload.Choices) == 0 {
		return message{}, errors.New("api error: no choices returned")
	}
	return payload.Choices[0].Message, nil
}

func completeTurn(ctx context.Context, client chatClient, messages []message, tools []toolDefinition) ([]message, string, error) {
	ws, err := workspace.New("", workspace.DefaultLimits())
	if err != nil {
		return nil, "", err
	}
	runtime := &toolRuntime{workspace: ws, policy: approval.Policy{Yolo: true, Interactive: false}}
	return completeTurnWithRuntime(ctx, client, messages, tools, runtime)
}

func completeTurnWithRuntime(ctx context.Context, client chatClient, messages []message, tools []toolDefinition, dispatcher toolDispatcher) ([]message, string, error) {
	return completeTurnWithRuntimeOptions(ctx, client, messages, tools, dispatcher, false)
}

func completeTurnWithRuntimeOptions(ctx context.Context, client chatClient, messages []message, tools []toolDefinition, dispatcher toolDispatcher, forceNativeToolCall bool) ([]message, string, error) {
	for i := 0; i < maxToolCallAttempts; i++ {
		options := chatCompleteOptions{}
		if len(tools) > 0 {
			options.ToolChoice = "auto"
			if forceNativeToolCall {
				options.ToolChoice = "required"
			}
		}
		assistantMessage, err := client.Complete(ctx, messages, tools, options)
		if err != nil {
			return nil, "", err
		}
		if len(assistantMessage.ToolCalls) == 0 {
			text, err := contentText(assistantMessage.Content)
			if err != nil {
				return nil, "", err
			}
			if strings.TrimSpace(text) == "" {
				return nil, "", errors.New("assistant returned an empty response")
			}
			if looksLikeTextualToolCall(text, tools) {
				if forceNativeToolCall {
					return nil, "", fmt.Errorf("assistant returned a textual tool call after forced structured retry; expected native tool_calls (preview: %q)", previewText(text, 200))
				}
				forceNativeToolCall = true
				messages = append(messages,
					message{Role: "assistant", Content: text},
					message{
						Role:    "user",
						Content: "Your previous response printed a simulated tool call as text. Do not print JSON or Markdown tool calls. Retry and return a native OpenAI-compatible `tool_calls` response using one of the provided tools.",
					},
				)
				continue
			}
			messages = append(messages, message{Role: "assistant", Content: text})
			return messages, text, nil
		}
		forceNativeToolCall = false
		messages = append(messages, message{Role: "assistant", Content: assistantMessage.Content, ToolCalls: assistantMessage.ToolCalls})
		for _, call := range assistantMessage.ToolCalls {
			if call.ID == "" {
				return nil, "", errors.New("malformed tool call: missing id")
			}
			toolOutput := dispatcher.Execute(ctx, call)
			messages = append(messages, message{
				Role:       "tool",
				ToolCallID: call.ID,
				Name:       call.Function.Name,
				Content:    toolOutput,
			})
		}
	}
	return nil, "", fmt.Errorf("tool loop exceeded %d iterations", maxToolCallAttempts)
}

func completeTurnRequiringWrites(ctx context.Context, client chatClient, messages []message, tools []toolDefinition, dispatcher toolDispatcher, ws *workspace.Workspace, requiredWrites []string, events *[]ToolEvent) ([]message, string, error) {
	updated, answer, err := completeTurnWithRuntime(ctx, client, messages, tools, dispatcher)
	if err != nil || len(requiredWrites) == 0 {
		return updated, answer, err
	}
	unmet := unmetRequiredWrites(ws, eventSlice(events), requiredWrites)
	if len(unmet) == 0 {
		return updated, answer, nil
	}
	repairMessages := append(append([]message{}, updated...), message{
		Role:    "user",
		Content: buildRequiredWriteRepairPrompt(unmet),
	})
	repaired, repairedAnswer, repairErr := completeTurnWithRuntimeOptions(ctx, client, repairMessages, tools, dispatcher, true)
	if repairErr == nil {
		updated = repaired
		answer = repairedAnswer
	} else {
		updated = repairMessages
	}
	unmet = unmetRequiredWrites(ws, eventSlice(events), requiredWrites)
	if len(unmet) == 0 {
		return updated, answer, nil
	}
	return updated, answer, requiredWriteError(unmet[0])
}

func contentText(content any) (string, error) {
	switch value := content.(type) {
	case string:
		return value, nil
	case []any:
		var builder strings.Builder
		for _, item := range value {
			switch part := item.(type) {
			case string:
				builder.WriteString(part)
			case map[string]any:
				if text, ok := part["text"].(string); ok {
					builder.WriteString(text)
				}
			}
		}
		return builder.String(), nil
	case map[string]any:
		if text, ok := value["text"].(string); ok {
			return text, nil
		}
		return "", errors.New("assistant content object did not contain a text field")
	case nil:
		return "", nil
	default:
		return "", fmt.Errorf("unexpected assistant content type %T", content)
	}
}

func looksLikeTextualToolCall(text string, tools []toolDefinition) bool {
	if len(tools) == 0 {
		return false
	}
	candidate, ok := extractStandaloneJSONObject(text)
	if !ok {
		return looksLikeMalformedTextualToolCall(text, tools)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(candidate), &payload); err != nil {
		return false
	}
	name, ok := payload["name"].(string)
	if !ok || strings.TrimSpace(name) == "" {
		return false
	}
	toolNames := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		toolNames[tool.Function.Name] = struct{}{}
	}
	if _, ok := toolNames[name]; !ok {
		return false
	}
	if _, ok := payload["arguments"]; !ok {
		return false
	}
	for key := range payload {
		switch key {
		case "name", "arguments", "id", "type":
		default:
			return false
		}
	}
	if kind, ok := payload["type"]; ok {
		typeString, ok := kind.(string)
		if !ok || typeString != "function" {
			return false
		}
	}
	switch value := payload["arguments"].(type) {
	case map[string]any:
		return true
	case string:
		var decoded map[string]any
		return json.Unmarshal([]byte(value), &decoded) == nil
	default:
		return false
	}
}

func looksLikeMalformedTextualToolCall(text string, tools []toolDefinition) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "```") {
		return false
	}
	newline := strings.IndexByte(trimmed, '\n')
	if newline == -1 {
		return false
	}
	language := strings.TrimSpace(trimmed[3:newline])
	if language != "" && !strings.EqualFold(language, "json") {
		return false
	}
	fenceEnd := strings.LastIndex(trimmed, "```")
	if fenceEnd <= newline || strings.TrimSpace(trimmed[fenceEnd+3:]) != "" {
		return false
	}
	candidate := trimmed[newline+1 : fenceEnd]
	for _, tool := range tools {
		if strings.Contains(candidate, `"name"`) &&
			strings.Contains(candidate, tool.Function.Name) &&
			strings.Contains(candidate, `"arguments"`) {
			return true
		}
	}
	return false
}

func extractStandaloneJSONObject(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", false
	}
	if strings.HasPrefix(trimmed, "```") {
		fenceEnd := strings.LastIndex(trimmed, "```")
		if fenceEnd <= 2 {
			return "", false
		}
		rest := strings.TrimSpace(trimmed[fenceEnd+3:])
		if rest != "" {
			return "", false
		}
		newline := strings.IndexByte(trimmed, '\n')
		if newline == -1 {
			return "", false
		}
		language := strings.TrimSpace(trimmed[3:newline])
		if language != "" && !strings.EqualFold(language, "json") {
			return "", false
		}
		trimmed = strings.TrimSpace(trimmed[newline+1 : fenceEnd])
	}
	if !strings.HasPrefix(trimmed, "{") || !strings.HasSuffix(trimmed, "}") {
		return "", false
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(trimmed))
	if err := decoder.Decode(&payload); err != nil {
		return "", false
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", false
	}
	return trimmed, true
}

func previewText(text string, limit int) string {
	normalized := strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if limit <= 0 || len(normalized) <= limit {
		return normalized
	}
	return normalized[:limit] + "..."
}

type toolResult struct {
	Success  bool   `json:"success"`
	Error    string `json:"error,omitempty"`
	Code     string `json:"code,omitempty"`
	Approved *bool  `json:"approved,omitempty"`
	Data     any    `json:"data,omitempty"`
}

type runCoreutilInput struct {
	Utility string   `json:"utility"`
	Args    []string `json:"args"`
	Stdin   string   `json:"stdin"`
}

type listFilesInput struct {
	Path    string   `json:"path"`
	Depth   int      `json:"depth"`
	Include []string `json:"include"`
	Exclude []string `json:"exclude"`
}

type readFileInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

type searchFilesInput struct {
	Query      string `json:"query"`
	Path       string `json:"path"`
	MaxResults int    `json:"max_results"`
}

type writeFileInput struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type applyPatchInput struct {
	Patch string `json:"patch"`
}

type mkdirInput struct {
	Path string `json:"path"`
}

func executeToolCall(ctx context.Context, call toolCall) string {
	ws, err := workspace.New("", workspace.DefaultLimits())
	if err != nil {
		return marshalToolResult(toolResult{Success: false, Error: err.Error()})
	}
	runtime := &toolRuntime{workspace: ws, policy: approval.Policy{Yolo: true, Interactive: false}}
	return executeToolCallWithRuntime(ctx, runtime, call)
}

func executeToolCallWithRuntime(ctx context.Context, runtime *toolRuntime, call toolCall) string {
	result := toolResult{Success: false}
	switch call.Function.Name {
	case "run_coreutil":
		input, err := decodeArguments[runCoreutilInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		if input.Utility == "" {
			result.Error = "malformed tool arguments: utility is required"
			return marshalToolResult(result)
		}
		if !supportsUtility(input.Utility) {
			result.Error = fmt.Sprintf("unsupported utility %q", input.Utility)
			return marshalToolResult(result)
		}
		var stdout, stderr bytes.Buffer
		err = coreutils.Run(ctx, input.Utility, input.Args, bytes.NewBufferString(input.Stdin), &stdout, &stderr)
		if err != nil {
			result.Error = err.Error()
			result.Data = map[string]any{"utility": input.Utility, "stdout": stdout.String(), "stderr": stderr.String()}
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = map[string]any{"utility": input.Utility, "stdout": stdout.String(), "stderr": stderr.String()}
		return marshalToolResult(result)
	case "list_files":
		input, err := decodeArguments[listFilesInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		list, err := runtime.workspace.ListFiles(workspace.ListOptions{Path: input.Path, Depth: input.Depth, Include: input.Include, Exclude: input.Exclude})
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = list
		return marshalToolResult(result)
	case "read_file":
		input, err := decodeArguments[readFileInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		if strings.TrimSpace(input.Path) == "" {
			result.Error = "path is required"
			return marshalToolResult(result)
		}
		read, err := runtime.workspace.ReadFile(input.Path, input.StartLine, input.EndLine)
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = read
		return marshalToolResult(result)
	case "search_files":
		input, err := decodeArguments[searchFilesInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		search, err := runtime.workspace.SearchFiles(workspace.SearchOptions{Query: input.Query, Path: input.Path, MaxResults: input.MaxResults})
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = search
		return marshalToolResult(result)
	case "git_status":
		status, err := gittools.Status(runtime.workspace.Root, runtime.workspace.Limits.MaxOutputBytes)
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = map[string]string{"text": status}
		return marshalToolResult(result)
	case "git_diff":
		diff, err := gittools.Diff(runtime.workspace.Root, runtime.workspace.Limits.MaxOutputBytes)
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = map[string]string{"text": diff}
		return marshalToolResult(result)
	case "write_file":
		input, err := decodeArguments[writeFileInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		allowed, denied := evaluateMutation(runtime, "write_file", previewWrite(input.Path, input.Content))
		if !allowed {
			return marshalToolResult(denied)
		}
		result.Approved = denied.Approved
		if err := runtime.workspace.WriteFile(input.Path, input.Content); err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		normalizedPath, err := runtime.workspace.NormalizeRelativePath(input.Path)
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = map[string]any{"path": normalizedPath, "bytes": len(input.Content)}
		return marshalToolResult(result)
	case "apply_patch":
		input, err := decodeArguments[applyPatchInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		if strings.TrimSpace(input.Patch) == "" {
			result.Error = "patch is required"
			return marshalToolResult(result)
		}
		allowed, denied := evaluateMutation(runtime, "apply_patch", previewPatch(input.Patch, runtime.workspace.Limits.MaxOutputBytes))
		if !allowed {
			return marshalToolResult(denied)
		}
		result.Approved = denied.Approved
		applyResult, err := runtime.workspace.ApplyPatch(input.Patch)
		if err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = applyResult
		return marshalToolResult(result)
	case "mkdir":
		input, err := decodeArguments[mkdirInput](call.Function.Arguments)
		if err != nil {
			result.Error = fmt.Sprintf("malformed tool arguments: %v", err)
			return marshalToolResult(result)
		}
		allowed, denied := evaluateMutation(runtime, "mkdir", "mkdir "+input.Path)
		if !allowed {
			return marshalToolResult(denied)
		}
		result.Approved = denied.Approved
		if err := runtime.workspace.Mkdir(input.Path); err != nil {
			result.Error = err.Error()
			return marshalToolResult(result)
		}
		result.Success = true
		result.Data = map[string]string{"path": input.Path}
		return marshalToolResult(result)
	default:
		result.Error = fmt.Sprintf("unsupported tool %q", call.Function.Name)
		return marshalToolResult(result)
	}
}

func evaluateMutation(runtime *toolRuntime, toolName, preview string) (bool, toolResult) {
	decision := runtime.policy.EvaluateMutation(toolName)
	if decision.Allowed {
		recordEvent(runtime.events, ToolEvent{Tool: toolName, Success: true})
		return true, toolResult{}
	}
	if decision.NeedsApproval {
		if runtime.prompt == nil {
			result := toolResult{Success: false, Error: "approval prompt is unavailable", Code: "approval_prompt_unavailable"}
			recordEvent(runtime.events, ToolEvent{Tool: toolName, Success: false, DeniedCode: result.Code, DeniedReason: result.Error})
			return false, result
		}
		approved, err := runtime.prompt(preview)
		if err != nil {
			result := toolResult{Success: false, Error: fmt.Sprintf("approval failed: %v", err), Code: "approval_prompt_error"}
			recordEvent(runtime.events, ToolEvent{Tool: toolName, Success: false, DeniedCode: result.Code, DeniedReason: result.Error})
			return false, result
		}
		recordEvent(runtime.events, ToolEvent{Tool: toolName, Success: approved, Approved: boolPtr(approved)})
		if !approved {
			return false, toolResult{Success: false, Error: "mutation denied by user", Code: "approval_denied", Approved: boolPtr(false)}
		}
		return true, toolResult{Approved: boolPtr(true)}
	}
	result := toolResult{Success: false, Error: decision.DeniedReason, Code: decision.StructuredCode}
	recordEvent(runtime.events, ToolEvent{Tool: toolName, Success: false, DeniedCode: decision.StructuredCode, DeniedReason: decision.DeniedReason})
	return false, result
}

func recordEvent(events *[]ToolEvent, event ToolEvent) {
	if events == nil {
		return
	}
	if event.Tool == "" {
		return
	}
	*events = append(*events, event)
}

func boolPtr(value bool) *bool {
	return &value
}

func eventSlice(events *[]ToolEvent) []ToolEvent {
	if events == nil {
		return nil
	}
	return *events
}

func buildToolEvent(ws *workspace.Workspace, call toolCall, output string) ToolEvent {
	event := ToolEvent{Tool: call.Function.Name}
	var result toolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		event.Success = false
		event.DeniedReason = output
		return event
	}
	event.Success = result.Success
	event.DeniedCode = result.Code
	event.DeniedReason = result.Error
	event.Approved = result.Approved
	if !result.Success || call.Function.Name != "write_file" || ws == nil {
		return event
	}
	input, err := decodeArguments[writeFileInput](call.Function.Arguments)
	if err != nil {
		return event
	}
	normalized, err := ws.NormalizeRelativePath(input.Path)
	if err != nil {
		return event
	}
	event.Path = normalized
	event.Bytes = int64(len(input.Content))
	return event
}

func normalizeRequiredWrites(ws *workspace.Workspace, paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	normalized := make([]string, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		value, err := ws.NormalizeRelativePath(path)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func unmetRequiredWrites(ws *workspace.Workspace, events []ToolEvent, requiredWrites []string) []string {
	if len(requiredWrites) == 0 {
		return nil
	}
	written := make(map[string]struct{}, len(requiredWrites))
	for _, event := range events {
		if event.Tool != "write_file" || !event.Success || event.Path == "" {
			continue
		}
		if _, err := ws.StatFile(event.Path); err == nil {
			written[event.Path] = struct{}{}
		}
	}
	unmet := make([]string, 0)
	for _, path := range requiredWrites {
		if _, ok := written[path]; !ok {
			unmet = append(unmet, path)
		}
	}
	return unmet
}

func buildRequiredWriteRepairPrompt(paths []string) string {
	return fmt.Sprintf("The required write was not completed: %s. Return a native OpenAI-compatible `tool_calls` response that invokes `write_file` for exactly the required workspace-relative path or paths. Do not return textual JSON, fenced code blocks, prose tool descriptions, shell commands, or shell substitutions. After the required write succeeds, you may return a brief final answer.", strings.Join(paths, ", "))
}

func requiredWriteError(path string) error {
	return fmt.Errorf("required write was not completed: %s", path)
}

func previewWrite(path, content string) string {
	const maxPreview = 512
	short := content
	if len(short) > maxPreview {
		short = short[:maxPreview] + "\n... (truncated)"
	}
	return fmt.Sprintf("write_file preview for %s:\n--- %s\n+++ %s\n+%s", path, path, path, strings.ReplaceAll(short, "\n", "\n+"))
}

func previewPatch(patch string, max int) string {
	if max <= 0 {
		max = 1024
	}
	if len(patch) > max {
		return patch[:max] + "\n... (truncated)"
	}
	return patch
}

func decodeArguments[T any](arguments any) (T, error) {
	var zero T
	var payload []byte
	switch value := arguments.(type) {
	case string:
		payload = []byte(value)
	case map[string]any:
		data, err := json.Marshal(value)
		if err != nil {
			return zero, err
		}
		payload = data
	case nil:
		payload = []byte("{}")
	default:
		return zero, fmt.Errorf("unexpected arguments type %T", arguments)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var input T
	if err := decoder.Decode(&input); err != nil {
		return zero, err
	}
	if decoder.More() {
		return zero, errors.New("unexpected trailing JSON content")
	}
	return input, nil
}

func marshalToolResult(result toolResult) string {
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return `{"success":false,"error":"failed to marshal tool result"}`
	}
	return string(data)
}

func supportsUtility(name string) bool {
	ensureUtilityMetadata()
	_, ok := supportedUtilitySet[name]
	return ok
}

func openAITools() []toolDefinition {
	utilityNames := utilityNames()
	return []toolDefinition{
		functionTool("run_coreutil", "Run one available core utility with optional args and stdin.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"utility"},
			"properties": map[string]any{
				"utility": map[string]any{"type": "string", "enum": append([]string{}, utilityNames...)},
				"args":    map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
				"stdin":   map[string]any{"type": "string"},
			},
		}),
		functionTool("list_files", "List files from the workspace deterministically.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"depth":   map[string]any{"type": "integer", "minimum": 1},
				"include": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"exclude": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
		}),
		functionTool("read_file", "Read a text file from the workspace with line metadata.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"path"},
			"properties": map[string]any{
				"path":       map[string]any{"type": "string"},
				"start_line": map[string]any{"type": "integer", "minimum": 1},
				"end_line":   map[string]any{"type": "integer", "minimum": 1},
			},
		}),
		functionTool("search_files", "Search text files in the workspace using literal matching.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"query"},
			"properties": map[string]any{
				"query":       map[string]any{"type": "string"},
				"path":        map[string]any{"type": "string"},
				"max_results": map[string]any{"type": "integer", "minimum": 1},
			},
		}),
		functionTool("git_status", "Return git status --short for the workspace.", map[string]any{"type": "object", "additionalProperties": false}),
		functionTool("git_diff", "Return current git diff for the workspace.", map[string]any{"type": "object", "additionalProperties": false}),
		functionTool("write_file", "Atomically write a file in the workspace.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"path", "content"},
			"properties": map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
		}),
		functionTool("apply_patch", "Apply a bounded subset of unified diffs to regular files inside the workspace.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"patch"},
			"properties": map[string]any{
				"patch": map[string]any{"type": "string"},
			},
		}),
		functionTool("mkdir", "Create workspace-confined directories.", map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"path"},
			"properties": map[string]any{
				"path": map[string]any{"type": "string"},
			},
		}),
	}
}

func functionTool(name, description string, parameters map[string]any) toolDefinition {
	return toolDefinition{Type: "function", Function: toolSpec{Name: name, Description: description, Parameters: parameters}}
}

func ensureUtilityMetadata() {
	supportedUtilitiesOnce.Do(func() {
		commands := coreutils.Commands()
		supportedUtilityNames = make([]string, 0, len(commands))
		supportedUtilitySet = make(map[string]struct{}, len(commands))
		for _, command := range commands {
			supportedUtilityNames = append(supportedUtilityNames, command.Name)
			supportedUtilitySet[command.Name] = struct{}{}
		}
	})
}

func utilityNames() []string {
	ensureUtilityMetadata()
	return append([]string{}, supportedUtilityNames...)
}

func buildSystemPrompt(instructions string, sources []string) string {
	prompt := baseSystemPrompt
	if strings.TrimSpace(instructions) != "" {
		prompt += "\n\nProject instructions loaded from: " + strings.Join(sources, ", ") + "\n" + instructions
	}
	return prompt
}

func loadSessionMessages(store *session.Store, sessionID string) ([]message, time.Time, error) {
	snapshot, err := store.LoadLatestSnapshot(sessionID)
	if err != nil {
		return nil, time.Time{}, err
	}
	messages := make([]message, 0, len(snapshot.Messages))
	for _, stored := range snapshot.Messages {
		var content any
		if len(stored.Content) > 0 {
			if err := json.Unmarshal(stored.Content, &content); err != nil {
				return nil, time.Time{}, fmt.Errorf("decode session message content: %w", err)
			}
		}
		var toolCalls []toolCall
		if len(stored.ToolCalls) > 0 {
			if err := json.Unmarshal(stored.ToolCalls, &toolCalls); err != nil {
				return nil, time.Time{}, fmt.Errorf("decode session tool calls: %w", err)
			}
		}
		messages = append(messages, message{Role: stored.Role, Content: content, ToolCalls: toolCalls, ToolCallID: stored.ToolCallID, Name: stored.Name})
	}
	return messages, snapshot.CreatedAt, nil
}

func storeSnapshot(store *session.Store, sessionID string, createdAt time.Time, messages []message) error {
	stored := make([]session.Message, 0, len(messages))
	for _, messageEntry := range messages {
		content, err := json.Marshal(messageEntry.Content)
		if err != nil {
			return err
		}
		toolCalls, err := json.Marshal(messageEntry.ToolCalls)
		if err != nil {
			return err
		}
		stored = append(stored, session.Message{
			Role:       messageEntry.Role,
			Content:    content,
			ToolCalls:  toolCalls,
			ToolCallID: messageEntry.ToolCallID,
			Name:       messageEntry.Name,
		})
	}
	return store.SaveSnapshot(sessionID, stored, createdAt, time.Now())
}
