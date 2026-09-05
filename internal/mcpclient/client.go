// Package mcpclient implements a minimal, correct MCP client speaking
// JSON-RPC 2.0 over a newline delimited stdio transport.
package mcpclient

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"sync"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/mcpproto"
)

// maxMessageBytes bounds a single protocol message read from the server.
const maxMessageBytes = 1 << 20

// Client is a synchronous MCP client. A single agent uses it sequentially.
type Client struct {
	writer io.WriteCloser
	reader io.ReadCloser
	stop   func()

	messages chan *mcpproto.Message
	readErr  chan error

	mutex     sync.Mutex
	nextID    int
	closeOnce sync.Once
}

// New wires a client to an already established transport. stop is invoked once
// during Close after the streams are closed.
func New(reader io.ReadCloser, writer io.WriteCloser, stop func()) *Client {
	client := &Client{
		writer:   writer,
		reader:   reader,
		stop:     stop,
		messages: make(chan *mcpproto.Message, 8),
		readErr:  make(chan error, 1),
	}
	go client.readLoop()
	return client
}

// StartProcess launches an MCP server as a child process and connects to its
// stdio streams. The child's stderr is forwarded to the agent's stderr.
func StartProcess(ctx context.Context, command string, args []string, workspace string, stderr io.Writer) (*Client, error) {
	if stderr == nil {
		stderr = io.Discard
	}
	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workspace
	cmd.Stderr = stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("start MCP server: %w", err)
	}

	stop := func() {
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			<-done
		}
	}
	return New(stdout, stdin, stop), nil
}

func (c *Client) readLoop() {
	scanner := bufio.NewScanner(c.reader)
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessageBytes)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		message := &mcpproto.Message{}
		if err := json.Unmarshal(line, message); err != nil {
			c.readErr <- fmt.Errorf("malformed MCP message: %w", err)
			close(c.messages)
			return
		}
		c.messages <- message
	}
	if err := scanner.Err(); err != nil {
		c.readErr <- err
	} else {
		c.readErr <- io.EOF
	}
	close(c.messages)
}

func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.nextID++
	id := strconv.Itoa(c.nextID)
	encodedID, err := json.Marshal(id)
	if err != nil {
		return err
	}
	if err := c.write(mcpproto.Message{JSONRPC: "2.0", ID: encodedID, Method: method, Params: mustParams(params)}); err != nil {
		return err
	}

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-c.readErr:
			return fmt.Errorf("MCP transport closed: %w", err)
		case message, ok := <-c.messages:
			if !ok {
				return errors.New("MCP transport closed")
			}
			if len(message.ID) == 0 || string(message.ID) != string(encodedID) {
				// Ignore notifications and unrelated responses.
				continue
			}
			if message.Error != nil {
				return fmt.Errorf("MCP error: %s", message.Error.Message)
			}
			if result == nil {
				return nil
			}
			return json.Unmarshal(message.Result, result)
		}
	}
}

func (c *Client) notify(method string, params any) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.write(mcpproto.Message{JSONRPC: "2.0", Method: method, Params: mustParams(params)})
}

func (c *Client) write(message mcpproto.Message) error {
	encoded, err := json.Marshal(message)
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	if _, err := c.writer.Write(encoded); err != nil {
		return fmt.Errorf("write MCP message: %w", err)
	}
	return nil
}

func mustParams(params any) json.RawMessage {
	if params == nil {
		return nil
	}
	encoded, err := json.Marshal(params)
	if err != nil {
		return nil
	}
	return encoded
}

// Initialize performs the MCP lifecycle handshake and verifies that the server
// advertises tool support.
func (c *Client) Initialize(ctx context.Context) (mcpproto.InitializeResult, error) {
	var result mcpproto.InitializeResult
	params := mcpproto.InitializeParams{
		ProtocolVersion: mcpproto.Version,
		ClientInfo:      mcpproto.Implementation{Name: "groovy-agent", Version: "1.0.0"},
	}
	if err := c.call(ctx, "initialize", params, &result); err != nil {
		return result, err
	}
	if result.Capabilities.Tools == nil {
		return result, errors.New("MCP server does not advertise tool capability")
	}
	if err := c.notify("notifications/initialized", map[string]any{}); err != nil {
		return result, err
	}
	return result, nil
}

// ListTools discovers every tool, following pagination cursors, and rejects
// malformed or duplicate definitions.
func (c *Client) ListTools(ctx context.Context) ([]mcpproto.Tool, error) {
	const maxPages = 10
	tools := make([]mcpproto.Tool, 0, 16)
	seen := make(map[string]struct{})
	cursor := ""
	for page := 0; page < maxPages; page++ {
		var result mcpproto.ListToolsResult
		if err := c.call(ctx, "tools/list", mcpproto.ListToolsParams{Cursor: cursor}, &result); err != nil {
			return nil, err
		}
		for _, tool := range result.Tools {
			if tool.Name == "" || len(tool.InputSchema) == 0 {
				return nil, fmt.Errorf("malformed tool definition %q", tool.Name)
			}
			var schema map[string]any
			if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
				return nil, fmt.Errorf("malformed input schema for tool %q", tool.Name)
			}
			if _, duplicate := seen[tool.Name]; duplicate {
				return nil, fmt.Errorf("duplicate tool definition %q", tool.Name)
			}
			seen[tool.Name] = struct{}{}
			tools = append(tools, tool)
		}
		if result.NextCursor == "" {
			return tools, nil
		}
		cursor = result.NextCursor
	}
	return nil, errors.New("tool discovery exceeded the pagination limit")
}

// CallTool executes a tool through MCP.
func (c *Client) CallTool(ctx context.Context, name string, arguments json.RawMessage) (mcpproto.CallToolResult, error) {
	var result mcpproto.CallToolResult
	err := c.call(ctx, "tools/call", mcpproto.CallToolParams{Name: name, Arguments: arguments}, &result)
	return result, err
}

// Close closes the MCP session and releases the transport, ensuring that a
// child process does not survive the agent.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		_ = c.writer.Close()
		if c.stop != nil {
			c.stop()
		}
		_ = c.reader.Close()
	})
}
