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

	"github.com/groovy-sky/go-core-mcp/coreutils"
)

const (
	protocolVersion = "2025-06-18"
	maxMessageSize  = 16 << 20
	maxOutputSize   = 4 << 20
)

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

type tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

type callParams struct {
	Name      string `json:"name"`
	Arguments struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
	} `json:"arguments"`
}

// Serve processes newline-delimited JSON-RPC requests until input closes.
func Serve(ctx context.Context, input io.Reader, output io.Writer) error {
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
		result, rpcErr := handle(ctx, req)
		if len(req.ID) == 0 {
			continue
		}
		if err := encoder.Encode(response{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr}); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func handle(ctx context.Context, req request) (any, *rpcError) {
	if req.JSONRPC != "2.0" || req.Method == "" {
		return nil, &rpcError{Code: -32600, Message: "invalid request"}
	}
	switch req.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]string{"name": "go-core-mcp", "version": "0.1.0"},
		}, nil
	case "ping":
		return map[string]any{}, nil
	case "notifications/initialized", "notifications/cancelled":
		return nil, nil
	case "tools/list":
		commands := coreutils.Commands()
		tools := make([]tool, 0, len(commands))
		for _, command := range commands {
			tools = append(tools, tool{
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
		return map[string]any{"tools": tools}, nil
	case "tools/call":
		return callTool(ctx, req.Params)
	default:
		return nil, &rpcError{Code: -32601, Message: "method not found"}
	}
}

func callTool(ctx context.Context, raw json.RawMessage) (any, *rpcError) {
	var params callParams
	if err := json.Unmarshal(raw, &params); err != nil || params.Name == "" {
		return nil, &rpcError{Code: -32602, Message: "invalid tool parameters"}
	}
	var stdout, stderr limitedBuffer
	err := coreutils.Run(
		ctx,
		params.Name,
		params.Arguments.Args,
		bytes.NewBufferString(params.Arguments.Stdin),
		&stdout,
		&stderr,
	)
	text := stdout.String()
	if stderr.Len() > 0 {
		text += stderr.String()
	}
	result := map[string]any{
		"content": []map[string]string{{"type": "text", "text": text}},
	}
	if err != nil {
		if errors.Is(err, errOutputLimit) {
			err = fmt.Errorf("output exceeded %d bytes", maxOutputSize)
		}
		if text != "" && text[len(text)-1] != '\n' {
			text += "\n"
		}
		result["content"] = []map[string]string{{"type": "text", "text": text + err.Error()}}
		result["isError"] = true
	}
	return result, nil
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
