package commandexec

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestExecuteSuccessCapturesOutputAndUsesWorkspaceDir(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := Execute(context.Background(), helperOptions(t, root, "print", Options{
		WorkingDir: "work",
		Env: map[string]string{
			"HELPER_VALUE": "ok",
		},
		Timeout: 2 * time.Second,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if !strings.Contains(result.Stdout, "cwd="+filepath.Clean(workDir)) {
		t.Fatalf("stdout missing cwd: %q", result.Stdout)
	}
	if !strings.Contains(result.Stdout, "value=ok") {
		t.Fatalf("stdout missing helper env output: %q", result.Stdout)
	}
	if !strings.Contains(result.Stderr, "stderr=helper") {
		t.Fatalf("stderr missing helper output: %q", result.Stderr)
	}
}

func TestExecuteNonZeroExit(t *testing.T) {
	root := t.TempDir()
	result, err := Execute(context.Background(), helperOptions(t, root, "exit7", Options{
		Timeout: 2 * time.Second,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 7 {
		t.Fatalf("exit code = %d", result.ExitCode)
	}
	if result.TimedOut || result.Canceled {
		t.Fatalf("unexpected timeout/cancel flags: %+v", result)
	}
	if !strings.Contains(result.Stderr, "boom") {
		t.Fatalf("stderr missing failure text: %q", result.Stderr)
	}
}

func TestExecuteTimeout(t *testing.T) {
	root := t.TempDir()
	result, err := Execute(context.Background(), helperOptions(t, root, "sleep", Options{
		Timeout: 100 * time.Millisecond,
		Env: map[string]string{
			"HELPER_SLEEP_MS": "1000",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut {
		t.Fatalf("expected timeout result: %+v", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code for timeout: %+v", result)
	}
}

func TestExecuteCancellation(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(80 * time.Millisecond)
		cancel()
	}()

	result, err := Execute(ctx, helperOptions(t, root, "sleep", Options{
		Env: map[string]string{
			"HELPER_SLEEP_MS": "1000",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Canceled {
		t.Fatalf("expected cancellation result: %+v", result)
	}
	if result.ExitCode == 0 {
		t.Fatalf("expected non-zero exit code for cancellation: %+v", result)
	}
}

func TestExecuteRejectsWorkingDirOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	_, err := Execute(context.Background(), helperOptions(t, root, "print", Options{
		WorkingDir: "../outside",
	}))
	if err == nil || !strings.Contains(err.Error(), "..") {
		t.Fatalf("expected traversal error, got %v", err)
	}
}

func TestExecuteRejectsAbsoluteWorkingDirOutsideWorkspace(t *testing.T) {
	root := t.TempDir()
	_, err := Execute(context.Background(), helperOptions(t, root, "print", Options{
		WorkingDir: filepath.Dir(root),
	}))
	if err == nil || !strings.Contains(err.Error(), "outside workspace") {
		t.Fatalf("expected outside workspace error, got %v", err)
	}
}

func helperOptions(t *testing.T, workspaceRoot, mode string, overrides Options) Options {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		WorkspaceRoot: workspaceRoot,
		Executable:    exe,
		Args:          []string{"-test.run=TestCommandExecHelperProcess", "--", mode},
		InheritEnv:    true,
		Env:           map[string]string{"GO_WANT_COMMAND_EXEC_HELPER": "1"},
	}
	for key, value := range overrides.Env {
		opts.Env[key] = value
	}
	opts.WorkingDir = overrides.WorkingDir
	if overrides.Timeout != 0 {
		opts.Timeout = overrides.Timeout
	}
	if overrides.Stdin != "" {
		opts.Stdin = overrides.Stdin
	}
	if overrides.Executable != "" {
		opts.Executable = overrides.Executable
	}
	if len(overrides.Args) > 0 {
		opts.Args = append([]string{}, overrides.Args...)
	}
	return opts
}

func TestCommandExecHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_COMMAND_EXEC_HELPER") != "1" {
		return
	}
	mode := ""
	for idx, arg := range os.Args {
		if arg == "--" && idx+1 < len(os.Args) {
			mode = os.Args[idx+1]
			break
		}
	}
	switch mode {
	case "print":
		cwd, _ := os.Getwd()
		_, _ = fmt.Fprintf(os.Stdout, "cwd=%s\n", filepath.Clean(cwd))
		_, _ = fmt.Fprintf(os.Stdout, "value=%s\n", os.Getenv("HELPER_VALUE"))
		_, _ = fmt.Fprintln(os.Stderr, "stderr=helper")
		os.Exit(0)
	case "exit7":
		_, _ = fmt.Fprintln(os.Stderr, "boom")
		os.Exit(7)
	case "sleep":
		sleepMs, _ := strconv.Atoi(os.Getenv("HELPER_SLEEP_MS"))
		if sleepMs <= 0 {
			sleepMs = 1000
		}
		time.Sleep(time.Duration(sleepMs) * time.Millisecond)
		os.Exit(0)
	default:
		os.Exit(2)
	}
}
