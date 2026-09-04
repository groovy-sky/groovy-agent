// Package moderator implements a planning/moderation layer that stands
// between the local OpenAI-compatible model and tool execution. Instead of
// letting the model freely choose and narrate tool use in a single loop, the
// model is required to return exactly one structured JSON Plan. Go validates
// that plan before any tool executes, and a deterministic VerifiedReport is
// built from the actual execution outcomes rather than from model prose.
package moderator

// Decision is the mutually exclusive top-level action a moderator Plan
// selects for a single user turn.
type Decision string

const (
	// DecisionAnswer means the request needs no tools; a plain conversational
	// reply is sufficient.
	DecisionAnswer Decision = "answer"
	// DecisionUseTools means ToolCalls must be executed, in order, to satisfy
	// the request.
	DecisionUseTools Decision = "use_tools"
	// DecisionClarify means the request is ambiguous and Reason should hold a
	// clarifying question for the user.
	DecisionClarify Decision = "clarify"
	// DecisionReject means the request cannot be safely fulfilled; Reason
	// should explain why.
	DecisionReject Decision = "reject"
)

// ToolRequest is one ordered, proposed tool invocation. Arguments must decode
// as a JSON object; free-form strings, arrays, or shell command lines are
// rejected by Validate before any tool executes.
type ToolRequest struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

// Plan is the single structured JSON object the moderator requires from the
// local model instead of trusting free-form planning prose. Every field is
// strictly typed; Validate must be called before executing any ToolCalls.
type Plan struct {
	// Decision selects how this turn should be handled.
	Decision Decision `json:"decision"`
	// Reason is an optional short rationale, clarifying question, or
	// rejection explanation. It must never be treated as evidence that an
	// action has been completed.
	Reason string `json:"reason,omitempty"`
	// ToolCalls are the ordered tool invocations to execute when
	// Decision is DecisionUseTools.
	ToolCalls []ToolRequest `json:"tool_calls,omitempty"`
	// RequireWrites lists workspace-relative paths that a successful
	// write_file call must target for this turn to be considered complete.
	// It is merged with any CLI-supplied --require-write paths.
	RequireWrites []string `json:"require_writes,omitempty"`
}
