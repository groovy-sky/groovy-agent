package moderator

import (
	"fmt"
	"sort"
	"strings"
)

// ExecutionFact is one deterministic outcome recorded from an actual tool
// dispatch (derived from a real ToolEvent, never from model prose).
// VerifiedReport is built exclusively from these facts so operational claims
// stay grounded in what really happened.
type ExecutionFact struct {
	Tool    string
	Path    string
	Success bool
}

// VerifiedReport is the deterministic execution summary derived from
// ExecutionFacts and any required-write paths left unmet after execution. It
// is safe to render directly as the final response for tool-using turns,
// which is the recommended way to avoid trusting model prose for operational
// claims.
type VerifiedReport struct {
	ChangedFiles []string
	CommandsRun  []string
	TestsRun     bool
	TestsPassed  bool
	UnmetWrites  []string
}

// BuildReport derives a VerifiedReport from recorded execution facts and any
// required-write paths that remained unmet after execution.
func BuildReport(facts []ExecutionFact, unmetWrites []string) VerifiedReport {
	report := VerifiedReport{}
	if len(unmetWrites) > 0 {
		report.UnmetWrites = append([]string{}, unmetWrites...)
	}
	changed := make(map[string]struct{})
	commands := make(map[string]struct{})
	sawTests := false
	testsPassed := true
	for _, fact := range facts {
		if !fact.Success {
			if fact.Tool == "run_tests" {
				sawTests = true
				testsPassed = false
			}
			continue
		}
		switch fact.Tool {
		case "write_file", "apply_patch", "mkdir":
			if fact.Path != "" {
				changed[fact.Path] = struct{}{}
			}
		case "run_coreutil", "exec_command":
			commands[fact.Tool] = struct{}{}
		case "run_tests":
			sawTests = true
		}
	}
	for path := range changed {
		report.ChangedFiles = append(report.ChangedFiles, path)
	}
	sort.Strings(report.ChangedFiles)
	for name := range commands {
		report.CommandsRun = append(report.CommandsRun, name)
	}
	sort.Strings(report.CommandsRun)
	report.TestsRun = sawTests
	report.TestsPassed = sawTests && testsPassed
	return report
}

// Render renders the report as plain text suitable for use as (or as part
// of) a final agent response.
func (report VerifiedReport) Render() string {
	var builder strings.Builder
	builder.WriteString("Verified execution summary (derived only from actual tool results):\n")
	builder.WriteString("Files changed: ")
	if len(report.ChangedFiles) == 0 {
		builder.WriteString("none")
	} else {
		builder.WriteString(strings.Join(report.ChangedFiles, ", "))
	}
	builder.WriteString("\nCommands run: ")
	if len(report.CommandsRun) == 0 {
		builder.WriteString("none")
	} else {
		builder.WriteString(strings.Join(report.CommandsRun, ", "))
	}
	builder.WriteString("\n")
	switch {
	case !report.TestsRun:
		builder.WriteString("Tests: not run.")
	case report.TestsPassed:
		builder.WriteString("Tests: run_tests passed.")
	default:
		builder.WriteString("Tests: run_tests reported a failure.")
	}
	if len(report.UnmetWrites) > 0 {
		builder.WriteString(fmt.Sprintf("\nUnmet required writes: %s", strings.Join(report.UnmetWrites, ", ")))
	}
	return builder.String()
}
