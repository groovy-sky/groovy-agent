package moderator

import (
	"errors"
	"strings"
	"testing"
)

func TestParsePlanValidJSON(t *testing.T) {
	plan, err := ParsePlan(`{"decision":"use_tools","reason":"write the date","tool_calls":[{"name":"write_file","arguments":{"path":"time.txt","content":"2026-09-04\n"}}],"require_writes":["time.txt"]}`)
	if err != nil {
		t.Fatalf("ParsePlan error: %v", err)
	}
	if plan.Decision != DecisionUseTools {
		t.Fatalf("decision = %q", plan.Decision)
	}
	if len(plan.ToolCalls) != 1 || plan.ToolCalls[0].Name != "write_file" {
		t.Fatalf("tool_calls = %+v", plan.ToolCalls)
	}
	if plan.ToolCalls[0].Arguments["path"] != "time.txt" {
		t.Fatalf("arguments = %+v", plan.ToolCalls[0].Arguments)
	}
	if len(plan.RequireWrites) != 1 || plan.RequireWrites[0] != "time.txt" {
		t.Fatalf("require_writes = %+v", plan.RequireWrites)
	}
}

func TestParsePlanAcceptsFencedJSON(t *testing.T) {
	plan, err := ParsePlan("```json\n{\"decision\":\"answer\",\"reason\":\"no tools needed\"}\n```")
	if err != nil {
		t.Fatalf("ParsePlan error: %v", err)
	}
	if plan.Decision != DecisionAnswer {
		t.Fatalf("decision = %q", plan.Decision)
	}
}

func TestParsePlanRejectsFreeFormProse(t *testing.T) {
	_, err := ParsePlan("Sure! I'll go ahead and read the date, then write it to time.txt for you.")
	if err == nil {
		t.Fatal("expected error for free-form prose")
	}
}

func TestParsePlanRejectsNonObjectJSON(t *testing.T) {
	_, err := ParsePlan(`["answer", "use_tools"]`)
	if err == nil {
		t.Fatal("expected error for non-object JSON")
	}
}

func TestParsePlanRejectsUnknownFields(t *testing.T) {
	_, err := ParsePlan(`{"decision":"answer","extra_field":"nope"}`)
	if err == nil {
		t.Fatal("expected error for unknown field")
	}
}

func TestParsePlanRejectsUnknownDecision(t *testing.T) {
	_, err := ParsePlan(`{"decision":"do_whatever"}`)
	if err == nil {
		t.Fatal("expected error for unknown decision")
	}
}

func TestParsePlanRejectsTrailingContent(t *testing.T) {
	_, err := ParsePlan(`{"decision":"answer"} extra trailing text`)
	if err == nil {
		t.Fatal("expected error for trailing content")
	}
}

func TestValidateRejectsUnknownTool(t *testing.T) {
	plan := Plan{Decision: DecisionUseTools, ToolCalls: []ToolRequest{{Name: "delete_everything", Arguments: map[string]any{}}}}
	_, err := Validate(plan, ValidationOptions{AllowedTools: map[string]struct{}{"write_file": {}}})
	if err == nil {
		t.Fatal("expected error for unknown tool")
	}
	if !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateRejectsMissingArguments(t *testing.T) {
	plan := Plan{Decision: DecisionUseTools, ToolCalls: []ToolRequest{{Name: "write_file"}}}
	_, err := Validate(plan, ValidationOptions{AllowedTools: map[string]struct{}{"write_file": {}}})
	if err == nil {
		t.Fatal("expected error for missing arguments object")
	}
}

func TestValidateRejectsEmptyToolCallsForUseTools(t *testing.T) {
	plan := Plan{Decision: DecisionUseTools}
	_, err := Validate(plan, ValidationOptions{})
	if err == nil {
		t.Fatal("expected error for empty tool_calls")
	}
}

func TestValidateRejectsToolCallsOnAnswerDecision(t *testing.T) {
	plan := Plan{Decision: DecisionAnswer, ToolCalls: []ToolRequest{{Name: "write_file", Arguments: map[string]any{}}}}
	_, err := Validate(plan, ValidationOptions{})
	if err == nil {
		t.Fatal("expected error for tool_calls on answer decision")
	}
}

func TestValidateNormalizesAndDedupesRequireWrites(t *testing.T) {
	plan := Plan{Decision: DecisionAnswer, RequireWrites: []string{"./time.txt", "time.txt"}}
	normalize := func(path string) (string, error) {
		return strings.TrimPrefix(path, "./"), nil
	}
	validated, err := Validate(plan, ValidationOptions{NormalizePath: normalize})
	if err != nil {
		t.Fatalf("Validate error: %v", err)
	}
	if len(validated.RequireWrites) != 1 || validated.RequireWrites[0] != "time.txt" {
		t.Fatalf("require_writes = %+v", validated.RequireWrites)
	}
}

func TestValidateRejectsUnsafeRequireWritePath(t *testing.T) {
	plan := Plan{Decision: DecisionAnswer, RequireWrites: []string{"../escape.txt"}}
	normalize := func(path string) (string, error) {
		if strings.Contains(path, "..") {
			return "", errors.New("path escapes workspace")
		}
		return path, nil
	}
	_, err := Validate(plan, ValidationOptions{NormalizePath: normalize})
	if err == nil {
		t.Fatal("expected error for unsafe require_writes path")
	}
}

func TestBuildReportTracksChangedFilesCommandsAndTests(t *testing.T) {
	facts := []ExecutionFact{
		{Tool: "run_coreutil", Success: true},
		{Tool: "write_file", Path: "time.txt", Success: true},
		{Tool: "run_tests", Success: true},
	}
	report := BuildReport(facts, nil)
	if len(report.ChangedFiles) != 1 || report.ChangedFiles[0] != "time.txt" {
		t.Fatalf("changed files = %+v", report.ChangedFiles)
	}
	if len(report.CommandsRun) != 1 || report.CommandsRun[0] != "run_coreutil" {
		t.Fatalf("commands run = %+v", report.CommandsRun)
	}
	if !report.TestsRun || !report.TestsPassed {
		t.Fatalf("tests run/passed = %v/%v", report.TestsRun, report.TestsPassed)
	}
}

func TestBuildReportDateOnlyDoesNotClaimFileWrite(t *testing.T) {
	// This mirrors the reported failure: the model ran `date` but never
	// called write_file, so the report must not list time.txt as changed and
	// must surface it as an unmet required write.
	facts := []ExecutionFact{
		{Tool: "run_coreutil", Success: true},
	}
	report := BuildReport(facts, []string{"time.txt"})
	if len(report.ChangedFiles) != 0 {
		t.Fatalf("changed files should be empty, got %+v", report.ChangedFiles)
	}
	if len(report.UnmetWrites) != 1 || report.UnmetWrites[0] != "time.txt" {
		t.Fatalf("unmet writes = %+v", report.UnmetWrites)
	}
	rendered := report.Render()
	if strings.Contains(rendered, "Files changed: time.txt") {
		t.Fatalf("rendered report falsely claims time.txt was written: %s", rendered)
	}
	if !strings.Contains(rendered, "Unmet required writes: time.txt") {
		t.Fatalf("rendered report missing unmet writes: %s", rendered)
	}
}

func TestBuildReportFailedTestsAreNotPassed(t *testing.T) {
	facts := []ExecutionFact{{Tool: "run_tests", Success: false}}
	report := BuildReport(facts, nil)
	if !report.TestsRun {
		t.Fatal("expected tests run = true")
	}
	if report.TestsPassed {
		t.Fatal("expected tests passed = false")
	}
}
