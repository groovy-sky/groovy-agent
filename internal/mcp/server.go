// Package mcp implements the stdio transport for the Model Context Protocol.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/approval"
	"github.com/groovy-sky/groovy-agent/internal/gittools"
	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

const (
	protocolVersion = "2025-06-18"
	maxMessageSize  = 16 << 20
	maxOutputSize   = 4 << 20
)

// Config holds optional workspace context used by the agent-mode MCP server.
// When Workspace is nil, only individual coreutils are exposed (external MCP
// server mode). When Workspace is set, the full agent toolset is exposed.
type Config struct {
	Workspace *workspace.Workspace
	// Policy is a pointer so that the caller can change plan/yolo mode between
	// tool calls without restarting the MCP server.
	Policy *approval.Policy
	// Prompt is called with a human-readable preview before mutating files.
	// May be nil (mutations that need approval are then denied).
	Prompt func(preview string) (bool, error)
	// OnEvent is called after each mutation tool is evaluated.
	OnEvent func(toolName string, success bool, approved *bool, code, reason string)
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type toolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Serve processes newline-delimited JSON-RPC requests exposing individual
// coreutil commands (external MCP server mode).
func Serve(ctx context.Context, input io.Reader, output io.Writer) error {
	return ServeWithConfig(ctx, input, output, Config{})
}

// ServeWithConfig processes newline-delimited JSON-RPC requests. When cfg
// includes a Workspace, the full agent toolset (workspace + git + coreutils)
// is exposed; otherwise only individual coreutil commands are exposed.
func ServeWithConfig(ctx context.Context, input io.Reader, output io.Writer, cfg Config) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64*1024), maxMessageSize)
	encoder := json.NewEncoder(output)
	for scanner.Scan() {
		if len(bytes.TrimSpace(scanner.Bytes())) == 0 {
			continue
		}
		var req request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			if err := encoder.Encode(response{
				JSONRPC: "2.0",
				ID:      json.RawMessage("null"),
				Error:   &rpcError{Code: -32700, Message: "parse error"},
			}); err != nil {
				return err
			}
			continue
		}
		result, rpcErr := handle(ctx, req, cfg)
		if len(req.ID) == 0 {
			continue
		}
		if err := encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handle(ctx context.Context, req request, cfg Config) (any, *rpcError) {
	if req.JSONRPC != "2.0" || req.Method == "" {
		return nil, &rpcError{Code: -32600, Message: "invalid request"}
	}
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "groovy-agent", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "tools/list":
		return listTools(cfg), nil
	case "tools/call":
		return callTool(ctx, req.Params, cfg)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func listTools(cfg Config) any {
	if cfg.Workspace != nil {
		return agentTools()
	}
	return coreutilTools()
}

// coreutilTools returns individual coreutil commands for external MCP clients.
func coreutilTools() map[string]any {
	commands := coreutils.Commands()
	tools := make([]toolDef, 0, len(commands))
	for _, command := range commands {
		tools = append(tools, toolDef{
			Name:        command.Name,
			Description: command.Description,
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"args": map[string]any{
						"type":        "array",
						"description": "Command-line operands and options",
						"items":       map[string]string{"type": "string"},
					},
					"stdin": map[string]string{
						"type":        "string",
						"description": "Text supplied to standard input",
					},
				},
			},
		})
	}
	return map[string]any{"tools": tools}
}

// utilityEnumNames returns the names of all registered coreutils for the
// run_coreutil tool schema.
func utilityEnumNames() []string {
	commands := coreutils.Commands()
	names := make([]string, 0, len(commands))
	for _, c := range commands {
		names = append(names, c.Name)
	}
	return names
}

// agentTools returns the full coding-agent toolset exposed via the in-process
// MCP server.
func agentTools() map[string]any {
	utilNames := utilityEnumNames()
	tools := []toolDef{
		{
			Name:        "run_coreutil",
			Description: "Run one available core utility with optional args and stdin.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"utility"},
				"properties": map[string]any{
					"utility": map[string]any{"type": "string", "enum": append([]string{}, utilNames...)},
					"args":    map[string]any{"type": "array", "items": map[string]string{"type": "string"}},
					"stdin":   map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "list_files",
			Description: "List files from the workspace deterministically.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"depth":   map[string]any{"type": "integer", "minimum": 1},
					"include": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					"exclude": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				},
			},
		},
		{
			Name:        "read_file",
			Description: "Read a text file from the workspace with line metadata.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"path"},
				"properties": map[string]any{
					"path":       map[string]any{"type": "string"},
					"start_line": map[string]any{"type": "integer", "minimum": 1},
					"end_line":   map[string]any{"type": "integer", "minimum": 1},
				},
			},
		},
		{
			Name:        "search_files",
			Description: "Search text files in the workspace using literal matching.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"query"},
				"properties": map[string]any{
					"query":       map[string]any{"type": "string"},
					"path":        map[string]any{"type": "string"},
					"max_results": map[string]any{"type": "integer", "minimum": 1},
				},
			},
		},
		{
			Name:        "git_status",
			Description: "Return git status --short for the workspace.",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		},
		{
			Name:        "git_diff",
			Description: "Return current git diff for the workspace.",
			InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		},
		{
			Name:        "write_file",
			Description: "Atomically write a file in the workspace.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"path", "content"},
				"properties": map[string]any{
					"path":    map[string]any{"type": "string"},
					"content": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "apply_patch",
			Description: "Apply a bounded subset of unified diffs to regular files inside the workspace.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"patch"},
				"properties": map[string]any{
					"patch": map[string]any{"type": "string"},
				},
			},
		},
		{
			Name:        "mkdir",
			Description: "Create workspace-confined directories.",
			InputSchema: map[string]any{
				"type":                 "object",
				"additionalProperties": false,
				"required":             []string{"path"},
				"properties": map[string]any{
					"path": map[string]any{"type": "string"},
				},
			},
		},
	}
	return map[string]any{"tools": tools}
}

// mcpContent wraps text into MCP content format.
func mcpContent(text string) map[string]any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
}

func mcpError(text string) map[string]any {
	return map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
		"isError": true,
	}
}

// toolResult mirrors the agent's JSON response format so the model receives
// consistent output regardless of whether dispatch is direct or via MCP.
type toolResult struct {
	Success bool   `json:"success"`
	Error   string `json:"error,omitempty"`
	Code    string `json:"code,omitempty"`
	Data    any    `json:"data,omitempty"`
}

func marshalResult(r toolResult) string {
	data, err := json.Marshal(r)
	if err != nil {
		return `{"success":false,"error":"failed to marshal tool result"}`
	}
	return string(data)
}

// callTool routes a tools/call request to the appropriate handler.
func callTool(ctx context.Context, raw json.RawMessage, cfg Config) (any, *rpcError) {
	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || params.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid tool parameters"}
	}

	if cfg.Workspace != nil {
		// Agent-mode: route workspace + agent tools
		return callAgentTool(ctx, params.Name, params.Arguments, cfg)
	}

	// External-mode: route individual coreutils
	return callCoreutilDirect(ctx, params.Name, params.Arguments)
}

// callCoreutilDirect handles external-mode tools/call for individual coreutils.
func callCoreutilDirect(ctx context.Context, name string, rawArgs json.RawMessage) (any, *rpcError) {
	var args struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	}
	if len(rawArgs) > 0 {
		if err := json.Unmarshal(rawArgs, &args); err != nil {
			return nil, &rpcError{Code: -32602, Message: "invalid tool arguments"}
		}
	}
	var stdout, stderr limitedBuffer
	err := coreutils.Run(ctx, name, args.Args, bytes.NewBufferString(args.Stdin), &stdout, &stderr)
	text := stdout.String()
	if stderr.Len() > 0 {
		text += stderr.String()
	}
	if err != nil {
		if errors.Is(err, errOutputLimit) {
			err = fmt.Errorf("output exceeded %d bytes", maxOutputSize)
		}
		if text != "" && text[len(text)-1] != '\n' {
			text += "\n"
		}
		return mcpError(text + err.Error()), nil
	}
	return mcpContent(text), nil
}

// callAgentTool handles agent-mode tools/call for the full agent toolset.
func callAgentTool(ctx context.Context, name string, rawArgs json.RawMessage, cfg Config) (any, *rpcError) {
	result := toolResult{Success: false}

	switch name {
	case "run_coreutil":
		var input struct {
			Utility string   `json:"utility"`
			Args    []string `json:"args"`
			Stdin   string   `json:"stdin"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		if input.Utility == "" {
			result.Error = "malformed tool arguments: utility is required"
			return mcpContent(marshalResult(result)), nil
		}
		var stdout, stderr bytes.Buffer
		err := coreutils.Run(ctx, input.Utility, input.Args, bytes.NewBufferString(input.Stdin), &stdout, &stderr)
		if err != nil {
			result.Error = err.Error()
			result.Data = map[string]any{"utility": input.Utility, "stdout": stdout.String(), "stderr": stderr.String()}
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = map[string]any{"utility": input.Utility, "stdout": stdout.String(), "stderr": stderr.String()}
		return mcpContent(marshalResult(result)), nil

	case "list_files":
		var input struct {
			Path    string   `json:"path"`
			Depth   int      `json:"depth"`
			Include []string `json:"include"`
			Exclude []string `json:"exclude"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		list, err := cfg.Workspace.ListFiles(workspace.ListOptions{Path: input.Path, Depth: input.Depth, Include: input.Include, Exclude: input.Exclude})
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = list
		return mcpContent(marshalResult(result)), nil

	case "read_file":
		var input struct {
			Path      string `json:"path"`
			StartLine int    `json:"start_line"`
			EndLine   int    `json:"end_line"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		if strings.TrimSpace(input.Path) == "" {
			result.Error = "path is required"
			return mcpContent(marshalResult(result)), nil
		}
		read, err := cfg.Workspace.ReadFile(input.Path, input.StartLine, input.EndLine)
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = read
		return mcpContent(marshalResult(result)), nil

	case "search_files":
		var input struct {
			Query      string `json:"query"`
			Path       string `json:"path"`
			MaxResults int    `json:"max_results"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		search, err := cfg.Workspace.SearchFiles(workspace.SearchOptions{Query: input.Query, Path: input.Path, MaxResults: input.MaxResults})
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = search
		return mcpContent(marshalResult(result)), nil

	case "git_status":
		status, err := gittools.Status(cfg.Workspace.Root, cfg.Workspace.Limits.MaxOutputBytes)
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = map[string]string{"text": status}
		return mcpContent(marshalResult(result)), nil

	case "git_diff":
		diff, err := gittools.Diff(cfg.Workspace.Root, cfg.Workspace.Limits.MaxOutputBytes)
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = map[string]string{"text": diff}
		return mcpContent(marshalResult(result)), nil

	case "write_file":
		var input struct {
			Path    string `json:"path"`
			Content string `json:"content"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		preview := previewWrite(input.Path, input.Content)
		allowed, denied := evaluateMutation(cfg, "write_file", preview)
		if !allowed {
			return mcpContent(marshalResult(denied)), nil
		}
		if err := cfg.Workspace.WriteFile(input.Path, input.Content); err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = map[string]string{"path": input.Path}
		return mcpContent(marshalResult(result)), nil

	case "apply_patch":
		var input struct {
			Patch string `json:"patch"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		if strings.TrimSpace(input.Patch) == "" {
			result.Error = "patch is required"
			return mcpContent(marshalResult(result)), nil
		}
		preview := previewPatch(input.Patch, cfg.Workspace.Limits.MaxOutputBytes)
		allowed, denied := evaluateMutation(cfg, "apply_patch", preview)
		if !allowed {
			return mcpContent(marshalResult(denied)), nil
		}
		applyResult, err := cfg.Workspace.ApplyPatch(input.Patch)
		if err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = applyResult
		return mcpContent(marshalResult(result)), nil

	case "mkdir":
		var input struct {
			Path string `json:"path"`
		}
		if err := decodeArgs(rawArgs, &input); err != nil {
			result.Error = "malformed tool arguments: " + err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		allowed, denied := evaluateMutation(cfg, "mkdir", "mkdir "+input.Path)
		if !allowed {
			return mcpContent(marshalResult(denied)), nil
		}
		if err := cfg.Workspace.Mkdir(input.Path); err != nil {
			result.Error = err.Error()
			return mcpContent(marshalResult(result)), nil
		}
		result.Success = true
		result.Data = map[string]string{"path": input.Path}
		return mcpContent(marshalResult(result)), nil

	default:
		result.Error = fmt.Sprintf("unsupported tool %q", name)
		return mcpContent(marshalResult(result)), nil
	}
}

// evaluateMutation checks the policy and optionally prompts the user.
func evaluateMutation(cfg Config, toolName, preview string) (bool, toolResult) {
	var policy approval.Policy
	if cfg.Policy != nil {
		policy = *cfg.Policy
	}
	decision := policy.EvaluateMutation(toolName)
	if decision.Allowed {
		if cfg.OnEvent != nil {
			cfg.OnEvent(toolName, true, nil, "", "")
		}
		return true, toolResult{}
	}
	if decision.NeedsApproval {
		if cfg.Prompt == nil {
			r := toolResult{Success: false, Error: "approval prompt is unavailable", Code: "approval_prompt_unavailable"}
			if cfg.OnEvent != nil {
				cfg.OnEvent(toolName, false, nil, r.Code, r.Error)
			}
			return false, r
		}
		approved, err := cfg.Prompt(preview)
		if err != nil {
			r := toolResult{Success: false, Error: fmt.Sprintf("approval failed: %v", err), Code: "approval_prompt_error"}
			if cfg.OnEvent != nil {
				cfg.OnEvent(toolName, false, nil, r.Code, r.Error)
			}
			return false, r
		}
		b := approved
		if cfg.OnEvent != nil {
			cfg.OnEvent(toolName, approved, &b, "", "")
		}
		if !approved {
			return false, toolResult{Success: false, Error: "mutation denied by user", Code: "approval_denied"}
		}
		return true, toolResult{}
	}
	r := toolResult{Success: false, Error: decision.DeniedReason, Code: decision.StructuredCode}
	if cfg.OnEvent != nil {
		cfg.OnEvent(toolName, false, nil, decision.StructuredCode, decision.DeniedReason)
	}
	return false, r
}

func previewWrite(path, content string) string {
	const max = 120
	if len(content) > max {
		return fmt.Sprintf("write_file %s (%d bytes)\n%.120s…", path, len(content), content)
	}
	return fmt.Sprintf("write_file %s\n%s", path, content)
}

func previewPatch(patch string, max int) string {
	if max <= 0 || len(patch) <= max {
		return patch
	}
	return patch[:max] + "\n…"
}

func decodeArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(dst)
}

var errOutputLimit = errors.New("output limit exceeded")

type limitedBuffer struct {
	bytes.Buffer
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	remaining := maxOutputSize - buffer.Len()
	if remaining <= 0 {
		return 0, errOutputLimit
	}
	if len(data) > remaining {
		_, _ = buffer.Buffer.Write(data[:remaining])
		return remaining, errOutputLimit
	}
	return buffer.Buffer.Write(data)
}
