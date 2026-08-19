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
	"os"
	"strings"
	"sync"
	"time"

	"github.com/groovy-sky/go-core-mcp/coreutils"
)

const (
	defaultModel        = "gpt-4o-mini"
	defaultBaseURL      = "https://api.openai.com/v1"
	maxToolCallAttempts = 8
)

const systemPrompt = "You are a terminal AI assistant. Use the run_coreutil tool for file/text operations when needed. Tools run in the current working directory and only support listed utilities; there is no shell access."

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
	Model    string           `json:"model"`
	Messages []message        `json:"messages"`
	Tools    []toolDefinition `json:"tools,omitempty"`
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
	Complete(context.Context, []message, []toolDefinition) (message, error)
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

// Run starts the interactive terminal agent.
func Run(ctx context.Context, input io.Reader, output, errOutput io.Writer) error {
	client, err := clientFromEnv()
	if err != nil {
		return err
	}
	tools := openAITools()
	fmt.Fprintln(output, "go-core-mcp agent mode. Type 'exit' to quit.")
	scanner := bufio.NewScanner(input)
	messages := []message{{Role: "system", Content: systemPrompt}}
	for {
		fmt.Fprint(output, "> ")
		if !scanner.Scan() {
			break
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		turn := append(append([]message{}, messages...), message{Role: "user", Content: line})
		updated, answer, err := completeTurn(ctx, client, turn, tools)
		if err != nil {
			fmt.Fprintf(errOutput, "agent error: %v\n", err)
			continue
		}
		messages = updated
		fmt.Fprintln(output, answer)
	}
	return scanner.Err()
}

func clientFromEnv() (*apiClient, error) {
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if apiKey == "" {
		return nil, errors.New("OPENAI_API_KEY is required")
	}
	model := strings.TrimSpace(os.Getenv("OPENAI_MODEL"))
	if model == "" {
		model = defaultModel
	}
	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &apiClient{
		httpClient: &http.Client{Timeout: 120 * time.Second},
		apiKey:     apiKey,
		model:      model,
		baseURL:    strings.TrimRight(baseURL, "/"),
	}, nil
}

func (client *apiClient) Complete(ctx context.Context, messages []message, tools []toolDefinition) (message, error) {
	requestBody, err := json.Marshal(chatRequest{Model: client.model, Messages: messages, Tools: tools})
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
	for i := 0; i < maxToolCallAttempts; i++ {
		assistantMessage, err := client.Complete(ctx, messages, tools)
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
			messages = append(messages, message{Role: "assistant", Content: text})
			return messages, text, nil
		}
		messages = append(messages, message{Role: "assistant", Content: assistantMessage.Content, ToolCalls: assistantMessage.ToolCalls})
		for _, call := range assistantMessage.ToolCalls {
			if call.ID == "" {
				return nil, "", errors.New("malformed tool call: missing id")
			}
			toolOutput := executeToolCall(ctx, call)
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

type toolInput struct {
	Utility string   `json:"utility"`
	Args    []string `json:"args"`
	Stdin   string   `json:"stdin"`
}

type toolOutput struct {
	Utility string `json:"utility"`
	Stdout  string `json:"stdout,omitempty"`
	Stderr  string `json:"stderr,omitempty"`
	Error   string `json:"error,omitempty"`
}

func executeToolCall(ctx context.Context, call toolCall) string {
	if call.Function.Name != "run_coreutil" {
		return marshalToolOutput(toolOutput{Error: fmt.Sprintf("unsupported tool %q", call.Function.Name)})
	}
	input, err := decodeToolInput(call.Function.Arguments)
	if err != nil {
		return marshalToolOutput(toolOutput{Error: fmt.Sprintf("malformed tool arguments: %v", err)})
	}
	if input.Utility == "" {
		return marshalToolOutput(toolOutput{Error: "malformed tool arguments: utility is required"})
	}
	if !supportsUtility(input.Utility) {
		return marshalToolOutput(toolOutput{Utility: input.Utility, Error: fmt.Sprintf("unsupported utility %q", input.Utility)})
	}
	var stdout, stderr bytes.Buffer
	err = coreutils.Run(ctx, input.Utility, input.Args, bytes.NewBufferString(input.Stdin), &stdout, &stderr)
	result := toolOutput{
		Utility: input.Utility,
		Stdout:  stdout.String(),
		Stderr:  stderr.String(),
	}
	if err != nil {
		result.Error = err.Error()
	}
	return marshalToolOutput(result)
}

func decodeToolInput(arguments any) (toolInput, error) {
	var payload []byte
	switch value := arguments.(type) {
	case string:
		payload = []byte(value)
	case map[string]any:
		data, err := json.Marshal(value)
		if err != nil {
			return toolInput{}, err
		}
		payload = data
	default:
		return toolInput{}, fmt.Errorf("unexpected arguments type %T", arguments)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var input toolInput
	if err := decoder.Decode(&input); err != nil {
		return toolInput{}, err
	}
	return input, nil
}

func marshalToolOutput(result toolOutput) string {
	data, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		errorText, textErr := json.Marshal(result.Error)
		if textErr != nil {
			return `{"error":"failed to marshal tool result"}`
		}
		return `{"error":` + string(errorText) + `}`
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
		{
			Type: "function",
			Function: toolSpec{
				Name:        "run_coreutil",
				Description: "Run one available core utility with optional CLI args and optional stdin text.",
				Parameters: map[string]any{
					"type":                 "object",
					"additionalProperties": false,
					"required":             []string{"utility"},
					"properties": map[string]any{
						"utility": map[string]any{
							"type":        "string",
							"enum":        append([]string{}, utilityNames...),
							"description": "Name of the utility to run",
						},
						"args": map[string]any{
							"type":        "array",
							"description": "Command line args for the utility",
							"items":       map[string]string{"type": "string"},
						},
						"stdin": map[string]any{
							"type":        "string",
							"description": "Text passed to the utility on stdin",
						},
					},
				},
			},
		},
	}
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
