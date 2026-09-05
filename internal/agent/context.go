package agent

import (
	"errors"

	"github.com/groovy-sky/groovy-agent/internal/llm"
)

// maxResultTokensPerMessage bounds a single tool result kept in history.
const maxToolResultBytes = 4 << 10

// estimateTokens is a deliberately conservative character based estimate. The
// MVP never asks the model to summarize its own history.
func estimateTokens(text string) int {
	return (len(text) + 3) / 4
}

func messageTokens(message llm.Message) int {
	total := estimateTokens(message.Content) + 8
	for _, call := range message.ToolCalls {
		total += estimateTokens(call.Function.Name) + estimateTokens(call.Function.Arguments) + 8
	}
	return total
}

func toolsTokens(tools []llm.Tool) int {
	total := 0
	for _, tool := range tools {
		total += estimateTokens(tool.Function.Name) + estimateTokens(tool.Function.Description) + 16
		for key := range tool.Function.Parameters {
			total += estimateTokens(key) + 4
		}
		total += 48
	}
	return total
}

// pruneMessages bounds the conversation for a 4096-token context. It keeps the
// system message, the current user request, and unresolved tool activity, drops
// redundant earlier assistant text and obsolete tool results, and truncates
// oversized results.
func pruneMessages(messages []llm.Message, tools []llm.Tool) ([]llm.Message, error) {
	if len(messages) == 0 {
		return nil, errors.New("conversation is empty")
	}

	system := messages[0]
	var user llm.Message
	userIndex := -1
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			user, userIndex = messages[index], index
			break
		}
	}
	if userIndex < 0 {
		return nil, errors.New("conversation has no user request")
	}

	// Everything after the current user request is tool activity for this
	// request; earlier assistant text is redundant.
	tail := make([]llm.Message, 0, len(messages))
	for _, message := range messages[userIndex+1:] {
		if message.Role == "tool" {
			content, _ := clampText(message.Content, maxToolResultBytes)
			message.Content = content
		}
		tail = append(tail, message)
	}

	budget := ContextTokens - ReservedTokens - toolsTokens(tools) - messageTokens(system) - messageTokens(user)
	if budget <= 0 {
		return nil, errors.New("the request does not fit into the model context")
	}

	// Drop the oldest resolved tool activity first, always keeping complete
	// assistant/tool pairs so tool results never precede their tool call.
	for {
		used := 0
		for _, message := range tail {
			used += messageTokens(message)
		}
		if used <= budget {
			break
		}
		if len(tail) == 0 {
			return nil, errors.New("the request does not fit into the model context")
		}
		tail = dropOldestExchange(tail)
	}

	pruned := make([]llm.Message, 0, len(tail)+2)
	pruned = append(pruned, system, user)
	pruned = append(pruned, tail...)
	return pruned, nil
}

// dropOldestExchange removes the oldest assistant tool-call message together
// with its tool results.
func dropOldestExchange(tail []llm.Message) []llm.Message {
	if len(tail) == 0 {
		return tail
	}
	index := 1
	for index < len(tail) && tail[index].Role == "tool" {
		index++
	}
	return tail[index:]
}

func clampText(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	return text[:limit] + "…[truncated]", true
}
