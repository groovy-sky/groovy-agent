package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/groovy-sky/groovy-agent/internal/moderator"
	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

// completeTurnWithModerator is the moderator/planner entry point used by Run
// and RunHeadless. Instead of letting the model freely choose and narrate
// tool use in a single loop, it first requires exactly one structured JSON
// plan (see internal/moderator) from the model, validates it in Go, and only
// then executes accepted tool requests through the existing dispatcher. The
// final answer for tool-using turns is a deterministic verified execution
// summary built from actual ToolEvents, never from model prose.
func completeTurnWithModerator(ctx context.Context, client chatClient, turn []message, tools []toolDefinition, dispatcher toolDispatcher, ws *workspace.Workspace, requiredWrites []string, events *[]ToolEvent, maxToolRounds int) ([]message, string, error) {
	maxToolRounds = normalizeMaxToolRounds(maxToolRounds)
	plan, err := requestModeratorPlan(ctx, client, turn, tools, ws)
	if err != nil {
		return turn, "", fmt.Errorf("moderator planning failed: %w", err)
	}
	switch plan.Decision {
	case moderator.DecisionClarify, moderator.DecisionReject:
		answer := strings.TrimSpace(plan.Reason)
		if answer == "" {
			if plan.Decision == moderator.DecisionClarify {
				answer = "Please clarify your request."
			} else {
				answer = "This request cannot be safely fulfilled."
			}
		}
		updated := append(append([]message{}, turn...), message{Role: "assistant", Content: answer})
		return updated, answer, nil
	case moderator.DecisionAnswer:
		response, err := client.Complete(ctx, turn, nil, chatCompleteOptions{ToolChoice: "none"})
		if err != nil {
			return turn, "", err
		}
		text, err := contentText(response.Content)
		if err != nil {
			return turn, "", err
		}
		if strings.TrimSpace(text) == "" {
			return turn, "", errors.New("assistant returned an empty response")
		}
		updated := append(append([]message{}, turn...), message{Role: "assistant", Content: text})
		return updated, text, nil
	case moderator.DecisionUseTools:
		return executeModeratedPlan(ctx, client, turn, plan, tools, dispatcher, ws, requiredWrites, events, maxToolRounds)
	default:
		return turn, "", fmt.Errorf("unsupported moderator decision %q", plan.Decision)
	}
}

// executeModeratedPlan deterministically executes an accepted use_tools plan
// through the existing dispatcher, performs at most one bounded repair
// attempt (re-planned, not free-form) if planner-derived or CLI-supplied
// required writes are unmet, and renders the final answer from a verified
// execution report rather than trusting model prose.
func executeModeratedPlan(ctx context.Context, client chatClient, turn []message, plan moderator.Plan, tools []toolDefinition, dispatcher toolDispatcher, ws *workspace.Workspace, requiredWrites []string, events *[]ToolEvent, maxToolRounds int) ([]message, string, error) {
	mergedWrites := mergeRequiredWrites(requiredWrites, plan.RequireWrites)

	updated, denyErr := applyPlanToolCalls(ctx, turn, plan.ToolCalls, dispatcher, events, "plan")
	if denyErr != nil {
		return updated, "", denyErr
	}

	unmet := unmetRequiredWrites(ws, eventSlice(events), mergedWrites)
	if len(unmet) > 0 && maxToolRounds > 1 {
		repairTurn := append(append([]message{}, updated...), message{Role: "user", Content: moderator.RepairPrompt(unmet)})
		repairPlan, planErr := requestModeratorPlan(ctx, client, repairTurn, tools, ws)
		if planErr == nil && repairPlan.Decision == moderator.DecisionUseTools {
			repaired, repairDenyErr := applyPlanToolCalls(ctx, repairTurn, repairPlan.ToolCalls, dispatcher, events, "repair")
			updated = repaired
			if repairDenyErr != nil {
				return updated, "", repairDenyErr
			}
		} else {
			updated = repairTurn
		}
		unmet = unmetRequiredWrites(ws, eventSlice(events), mergedWrites)
	}

	report := moderator.BuildReport(executionFacts(eventSlice(events)), unmet)
	answer := renderPlanAnswer(plan.Reason, report)
	updated = append(updated, message{Role: "assistant", Content: answer})
	if len(unmet) > 0 {
		return updated, answer, requiredWriteError(unmet[0])
	}
	return updated, answer, nil
}

// applyPlanToolCalls executes a plan's ordered tool requests through the
// existing dispatcher (the same in-process MCP channel used by native
// tool_calls), appending an assistant/tool message pair per call so the
// transcript stays compatible with session persistence and resumption. It
// returns an error immediately if any call was denied by workspace
// confinement or the approval/Yolo/plan-mode policy.
func applyPlanToolCalls(ctx context.Context, base []message, calls []moderator.ToolRequest, dispatcher toolDispatcher, events *[]ToolEvent, idPrefix string) ([]message, error) {
	if len(calls) == 0 {
		return base, nil
	}
	toolCalls := make([]toolCall, 0, len(calls))
	for i, request := range calls {
		toolCalls = append(toolCalls, toolCall{
			ID:   fmt.Sprintf("%s_%d", idPrefix, i+1),
			Type: "function",
			Function: toolFunction{
				Name:      request.Name,
				Arguments: request.Arguments,
			},
		})
	}
	updated := append(append([]message{}, base...), message{Role: "assistant", ToolCalls: toolCalls})
	initialEventCount := len(eventSlice(events))
	for _, call := range toolCalls {
		output := dispatcher.Execute(ctx, call)
		updated = append(updated, message{Role: "tool", ToolCallID: call.ID, Name: call.Function.Name, Content: output})
	}
	for _, event := range eventSlice(events)[initialEventCount:] {
		switch event.DeniedCode {
		case "approval_required_non_interactive", "plan_mode_denied", "approval_denied":
			return updated, errors.New(event.DeniedCode)
		}
	}
	return updated, nil
}

// requestModeratorPlan sends the conversation to the local model with a
// constrained planning system prompt and requires exactly one structured
// JSON plan back. It retries once with corrective feedback if the model
// returns anything else, then gives up rather than falling back to
// executing unvalidated free-form prose.
func requestModeratorPlan(ctx context.Context, client chatClient, conversation []message, tools []toolDefinition, ws *workspace.Workspace) (moderator.Plan, error) {
	allowed := toolNameSet(tools)
	systemPrompt := moderator.SystemPrompt(toolNames(tools))
	planMessages := moderatorConversation(systemPrompt, conversation)
	plan, err := requestPlanOnce(ctx, client, planMessages, allowed, ws)
	if err == nil {
		return plan, nil
	}
	corrective := append(append([]message{}, planMessages...), message{
		Role:    "user",
		Content: fmt.Sprintf("Your previous response was rejected: %v. Return exactly one valid JSON Plan object as instructed, with no other text.", err),
	})
	plan, retryErr := requestPlanOnce(ctx, client, corrective, allowed, ws)
	if retryErr != nil {
		return moderator.Plan{}, fmt.Errorf("%w (retry: %v)", err, retryErr)
	}
	return plan, nil
}

func requestPlanOnce(ctx context.Context, client chatClient, messages []message, allowed map[string]struct{}, ws *workspace.Workspace) (moderator.Plan, error) {
	response, err := client.Complete(ctx, messages, nil, chatCompleteOptions{ToolChoice: "none"})
	if err != nil {
		return moderator.Plan{}, err
	}
	text, err := contentText(response.Content)
	if err != nil {
		return moderator.Plan{}, err
	}
	plan, err := moderator.ParsePlan(text)
	if err != nil {
		return moderator.Plan{}, err
	}
	validated, err := moderator.Validate(plan, moderator.ValidationOptions{AllowedTools: allowed, NormalizePath: ws.NormalizeRelativePath})
	if err != nil {
		return moderator.Plan{}, err
	}
	return validated, nil
}

// moderatorConversation builds the message list sent for a planning request:
// the constrained moderator system prompt followed by the conversation so
// far (with any existing system prompt dropped, since the moderator prompt
// replaces it for this request only).
func moderatorConversation(systemPrompt string, conversation []message) []message {
	out := make([]message, 0, len(conversation)+1)
	out = append(out, message{Role: "system", Content: systemPrompt})
	for _, m := range conversation {
		if m.Role == "system" {
			continue
		}
		out = append(out, m)
	}
	return out
}

func toolNames(tools []toolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Function.Name)
	}
	return names
}

func toolNameSet(tools []toolDefinition) map[string]struct{} {
	set := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		set[t.Function.Name] = struct{}{}
	}
	return set
}

// mergeRequiredWrites combines CLI-supplied --require-write paths (already
// normalized) with plan-derived require_writes (already normalized by
// moderator.Validate), de-duplicating while preserving order.
func mergeRequiredWrites(base, extra []string) []string {
	if len(extra) == 0 {
		return base
	}
	seen := make(map[string]struct{}, len(base)+len(extra))
	merged := make([]string, 0, len(base)+len(extra))
	for _, path := range base {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		merged = append(merged, path)
	}
	for _, path := range extra {
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		merged = append(merged, path)
	}
	return merged
}

// executionFacts converts recorded ToolEvents into moderator.ExecutionFact
// values so the verified report is built only from real dispatch outcomes.
func executionFacts(events []ToolEvent) []moderator.ExecutionFact {
	facts := make([]moderator.ExecutionFact, 0, len(events))
	for _, event := range events {
		facts = append(facts, moderator.ExecutionFact{Tool: event.Tool, Path: event.Path, Success: event.Success})
	}
	return facts
}

// renderPlanAnswer combines the plan's short rationale (if any) with the
// deterministic verified report, which is authoritative for what actually
// happened.
func renderPlanAnswer(reason string, report moderator.VerifiedReport) string {
	reason = strings.TrimSpace(reason)
	rendered := report.Render()
	if reason == "" {
		return rendered
	}
	return reason + "\n\n" + rendered
}
