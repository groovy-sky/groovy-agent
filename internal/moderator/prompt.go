package moderator

import (
	"fmt"
	"strings"
)

// SystemPrompt returns the constrained moderator instructions. It requires
// exactly one JSON Plan object and forbids narrating actions that were not
// actually performed.
func SystemPrompt(toolNames []string) string {
	names := "(no tools available)"
	if len(toolNames) > 0 {
		names = strings.Join(toolNames, ", ")
	}
	return fmt.Sprintf(
		"You are the planning moderator for a local coding agent. Respond with exactly one JSON object and nothing else: "+
			"no prose, no markdown, no code fences, no explanation before or after it. "+
			"The object must have this shape: {\"decision\":\"answer|use_tools|clarify|reject\",\"reason\":\"short rationale\","+
			"\"tool_calls\":[{\"name\":\"tool_name\",\"arguments\":{}}],\"require_writes\":[\"workspace/relative/path\"]}. "+
			"Use \"answer\" when the request needs no tools. Use \"use_tools\" and list every tool call needed, in the exact "+
			"order they must run, choosing names only from this allow-list: %s. "+
			"Use \"clarify\" when the request is ambiguous and put your question in \"reason\". Use \"reject\" when the "+
			"request cannot be safely fulfilled and explain why in \"reason\". "+
			"Every tool_calls entry must have a JSON object for \"arguments\" (use {} for tools that take no arguments); "+
			"never emit shell strings or shell command lines. "+
			"List every workspace-relative file the request expects you to create or modify in \"require_writes\" so it can "+
			"be verified after execution; omit it entirely when no file output is expected. "+
			"You are only proposing actions: never claim in \"reason\" that a file was written, a command ran, or tests "+
			"passed, because none of the tool_calls have executed yet.",
		names,
	)
}

// RepairPrompt asks the moderator for a follow-up plan that satisfies
// required writes left unmet by a prior plan's execution.
func RepairPrompt(paths []string) string {
	label := "write"
	if len(paths) > 1 {
		label = "writes"
	}
	return fmt.Sprintf(
		"The previously planned required %s was not completed: %s. Return exactly one new JSON Plan object with "+
			"decision \"use_tools\" whose tool_calls contain only the write_file calls needed to satisfy these exact "+
			"workspace-relative paths.",
		label, strings.Join(paths, ", "),
	)
}
