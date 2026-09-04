package moderator

import (
	"errors"
	"fmt"
	"strings"
)

// ValidationOptions configures Validate with the caller's tool allow-list and
// path normalization, so this package stays independent of the concrete
// workspace and MCP tool implementations.
type ValidationOptions struct {
	// AllowedTools is the set of known tool names (typically derived from the
	// live MCP tools/list result). A nil map skips the allow-list check.
	AllowedTools map[string]struct{}
	// NormalizePath converts a user-supplied path into a normalized
	// workspace-relative path, rejecting absolute paths, "..", and symlink
	// escapes. A nil func leaves RequireWrites paths as-is (only intended for
	// tests that do not exercise path handling).
	NormalizePath func(path string) (string, error)
}

// Validate checks a parsed Plan against policy before any tool executes. It
// rejects unknown tools, non-object arguments, and tool_calls on decisions
// that must not request any, and returns a copy of the plan with
// RequireWrites normalized and de-duplicated via NormalizePath.
func Validate(plan Plan, opts ValidationOptions) (Plan, error) {
	switch plan.Decision {
	case DecisionAnswer, DecisionClarify, DecisionReject:
		if len(plan.ToolCalls) > 0 {
			return Plan{}, fmt.Errorf("decision %q must not include tool_calls", plan.Decision)
		}
	case DecisionUseTools:
		if len(plan.ToolCalls) == 0 {
			return Plan{}, errors.New("use_tools decision requires at least one tool_calls entry")
		}
		for i, call := range plan.ToolCalls {
			name := strings.TrimSpace(call.Name)
			if name == "" {
				return Plan{}, fmt.Errorf("tool_calls[%d]: name is required", i)
			}
			if opts.AllowedTools != nil {
				if _, ok := opts.AllowedTools[name]; !ok {
					return Plan{}, fmt.Errorf("tool_calls[%d]: unknown tool %q", i, name)
				}
			}
			if call.Arguments == nil {
				return Plan{}, fmt.Errorf("tool_calls[%d]: arguments must be a JSON object", i)
			}
		}
	default:
		return Plan{}, fmt.Errorf("unsupported decision %q", plan.Decision)
	}
	normalized, err := normalizeRequireWrites(plan.RequireWrites, opts.NormalizePath)
	if err != nil {
		return Plan{}, fmt.Errorf("require_writes: %w", err)
	}
	plan.RequireWrites = normalized
	return plan, nil
}

func normalizeRequireWrites(paths []string, normalize func(string) (string, error)) ([]string, error) {
	if len(paths) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		value := path
		if normalize != nil {
			normalizedValue, err := normalize(path)
			if err != nil {
				return nil, err
			}
			value = normalizedValue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}
