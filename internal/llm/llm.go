// Package llm implements the OpenAI compatible chat-completions client used to
// talk to a local llama-server, including function/tool calls.
package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Defaults required by PLAN.md for a 4096-token Qwen2.5 profile.
const (
	RequestTimeout  = 180 * time.Second
	MaxOutputTokens = 256
)

// Message is a single chat message.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

// FunctionCall is the model's requested function invocation.
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolCall is a single tool call emitted by the model.
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// Function is the model-facing description of a tool.
type Function struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// Tool is the function-tool envelope expected by llama-server.
type Tool struct {
	Type     string   `json:"type"`
	Function Function `json:"function"`
}

type chatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Tools       []Tool    `json:"tools,omitempty"`
	ToolChoice  string    `json:"tool_choice,omitempty"`
	MaxTokens   int       `json:"max_tokens"`
	Temperature float64   `json:"temperature"`
	Stream      bool      `json:"stream"`
}

type chatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Client talks to a local llama-server.
type Client struct {
	baseURL string
	model   string
	http    *http.Client
}

// New creates a client for the given llama-server base URL.
func New(baseURL, model string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: RequestTimeout},
	}
}

// Ping verifies that llama-server is reachable before any child process is
// started.
func (c *Client) Ping(ctx context.Context) error {
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var lastErr error
	for _, path := range []string{"/health", "/v1/models"} {
		request, err := http.NewRequestWithContext(pingCtx, http.MethodGet, c.baseURL+path, nil)
		if err != nil {
			return err
		}
		response, err := c.http.Do(request)
		if err != nil {
			lastErr = err
			continue
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
		if response.StatusCode < http.StatusInternalServerError {
			return nil
		}
		lastErr = fmt.Errorf("llama-server responded with status %d", response.StatusCode)
	}
	if lastErr == nil {
		lastErr = errors.New("llama-server is not reachable")
	}
	return fmt.Errorf("llama-server is not reachable at %s: %w", c.baseURL, lastErr)
}

// Complete performs a bounded chat completion request.
func (c *Client) Complete(ctx context.Context, messages []Message, tools []Tool) (Message, error) {
	requestCtx, cancel := context.WithTimeout(ctx, RequestTimeout)
	defer cancel()

	body := chatRequest{
		Model:       c.model,
		Messages:    messages,
		Tools:       tools,
		MaxTokens:   MaxOutputTokens,
		Temperature: 0,
	}
	if len(tools) > 0 {
		body.ToolChoice = "auto"
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return Message{}, err
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, c.baseURL+"/v1/chat/completions", bytes.NewReader(encoded))
	if err != nil {
		return Message{}, err
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(request)
	if err != nil {
		return Message{}, fmt.Errorf("model request failed: %w", err)
	}
	defer response.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return Message{}, fmt.Errorf("model response could not be read: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		return Message{}, fmt.Errorf("model request failed with status %d", response.StatusCode)
	}
	decoded := chatResponse{}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return Message{}, errors.New("model response is not valid JSON")
	}
	if decoded.Error != nil {
		return Message{}, fmt.Errorf("model error: %s", decoded.Error.Message)
	}
	if len(decoded.Choices) == 0 {
		return Message{}, errors.New("model returned no choices")
	}
	message := decoded.Choices[0].Message
	if message.Role == "" {
		message.Role = "assistant"
	}
	return message, nil
}
