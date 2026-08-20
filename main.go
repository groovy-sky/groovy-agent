package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/groovy-sky/groovy-agent/coreutils"
	"github.com/groovy-sky/groovy-agent/internal/agent"
	"github.com/groovy-sky/groovy-agent/internal/mcp"
)

const (
	exitInvalidConfig = 2
	exitRuntimeError  = 1
	exitPolicyDenied  = 3
)

func main() {
	if len(os.Args) == 1 || os.Args[1] == "mcp" {
		if err := mcp.Serve(context.Background(), os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitRuntimeError)
		}
		return
	}
	switch os.Args[1] {
	case "agent":
		status := runAgent(context.Background(), os.Args[2:])
		os.Exit(status)
	case "run":
		status := runHeadless(context.Background(), os.Args[2:])
		os.Exit(status)
	default:
		if err := coreutils.Run(context.Background(), os.Args[1], os.Args[2:], os.Stdin, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(exitRuntimeError)
		}
	}
}

func runAgent(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	workspacePath := flags.String("workspace", "", "Workspace root for agent operations")
	planMode := flags.Bool("plan", false, "Enable read-only planning mode")
	yolo := flags.Bool("yolo", false, "Automatically approve mutations")
	resumeID := flags.String("resume", "", "Resume from session id")
	if err := flags.Parse(args); err != nil {
		return exitInvalidConfig
	}
	if err := agent.Run(ctx, os.Stdin, os.Stdout, os.Stderr, agent.Options{
		WorkspacePath: *workspacePath,
		PlanMode:      *planMode,
		Yolo:          *yolo,
		ResumeID:      *resumeID,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return exitRuntimeError
	}
	return 0
}

func runHeadless(ctx context.Context, args []string) int {
	flags := flag.NewFlagSet("run", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	prompt := flags.String("p", "", "Prompt to run")
	workspacePath := flags.String("workspace", "", "Workspace root for agent operations")
	planMode := flags.Bool("plan", false, "Enable read-only planning mode")
	yolo := flags.Bool("yolo", false, "Automatically approve mutations")
	resumeID := flags.String("resume", "", "Resume from session id")
	outputFormat := flags.String("output", "text", "Output format: text or json")
	outputDir := flags.String("output-dir", outputDirDefault(), "Directory for persisted result JSON files")
	if err := flags.Parse(args); err != nil {
		return exitInvalidConfig
	}
	if *outputFormat != "text" && *outputFormat != "json" {
		fmt.Fprintln(os.Stderr, "invalid --output value, expected text|json")
		return exitInvalidConfig
	}
	result, err := agent.RunHeadless(ctx, *prompt, agent.Options{
		WorkspacePath: *workspacePath,
		PlanMode:      *planMode,
		Yolo:          *yolo,
		ResumeID:      *resumeID,
		OutputDir:     *outputDir,
	})
	if err != nil {
		if *outputFormat == "json" {
			payload, _ := json.Marshal(result)
			fmt.Println(string(payload))
		}
		fmt.Fprintln(os.Stderr, err)
		if isPolicyDenied(err) {
			return exitPolicyDenied
		}
		if errors.Is(err, flag.ErrHelp) {
			return exitInvalidConfig
		}
		return exitRuntimeError
	}
	if *outputFormat == "json" {
		payload, marshalErr := json.Marshal(result)
		if marshalErr != nil {
			fmt.Fprintln(os.Stderr, marshalErr)
			return exitRuntimeError
		}
		fmt.Println(string(payload))
	} else {
		fmt.Println(result.Answer)
	}
	return 0
}

func isPolicyDenied(err error) bool {
	if err == nil {
		return false
	}
	text := err.Error()
	return strings.Contains(text, "approval_required_non_interactive") || strings.Contains(text, "plan_mode_denied")
}

// outputDirDefault returns the value of AGENT_OUTPUT_DIR if set, otherwise the
// package-level default ("output"). This lets operators override the path via
// environment without requiring a CLI flag.
func outputDirDefault() string {
	if v := os.Getenv("AGENT_OUTPUT_DIR"); v != "" {
		return v
	}
	return agent.DefaultOutputDir
}
