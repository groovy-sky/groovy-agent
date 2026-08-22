package commandexec

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/groovy-sky/groovy-agent/internal/workspace"
)

// Options controls isolated command execution inside a workspace.
type Options struct {
	WorkspaceRoot string
	WorkingDir    string
	Executable    string
	Args          []string
	Stdin         string
	Timeout       time.Duration
	Env           map[string]string
	InheritEnv    bool
}

// Result is the structured execution output.
type Result struct {
	WorkspaceRoot string   `json:"workspace_root"`
	WorkingDir    string   `json:"working_dir"`
	Executable    string   `json:"executable"`
	Args          []string `json:"args,omitempty"`
	ExitCode      int      `json:"exit_code"`
	Stdout        string   `json:"stdout"`
	Stderr        string   `json:"stderr"`
	TimedOut      bool     `json:"timed_out,omitempty"`
	Canceled      bool     `json:"canceled,omitempty"`
	DurationMs    int64    `json:"duration_ms"`
}

// Execute runs a command with explicit executable+arguments in a workspace.
func Execute(ctx context.Context, options Options) (Result, error) {
	if strings.TrimSpace(options.WorkspaceRoot) == "" {
		return Result{}, errors.New("workspace root is required")
	}
	if strings.TrimSpace(options.Executable) == "" {
		return Result{}, errors.New("executable is required")
	}
	if options.Timeout < 0 {
		return Result{}, errors.New("timeout must be zero or positive")
	}

	ws, err := workspace.New(options.WorkspaceRoot, workspace.DefaultLimits())
	if err != nil {
		return Result{}, err
	}
	workingDir, err := resolveWorkingDir(ws, options.WorkingDir)
	if err != nil {
		return Result{}, err
	}

	runCtx := ctx
	cancel := func() {}
	if options.Timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, options.Timeout)
	}
	defer cancel()

	cmd := exec.Command(options.Executable, options.Args...)
	configureCommand(cmd)
	cmd.Dir = workingDir
	cmd.Env = buildEnv(options.InheritEnv, options.Env)
	if options.Stdin != "" {
		cmd.Stdin = strings.NewReader(options.Stdin)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	started := time.Now()
	result := Result{
		WorkspaceRoot: ws.Root,
		WorkingDir:    filepath.Clean(workingDir),
		Executable:    options.Executable,
		Args:          append([]string{}, options.Args...),
		ExitCode:      -1,
	}

	if err := cmd.Start(); err != nil {
		return result, fmt.Errorf("start command: %w", err)
	}

	waitCh := make(chan error, 1)
	go func() {
		waitCh <- cmd.Wait()
	}()

	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-runCtx.Done():
		terminateCommand(cmd)
		waitErr = <-waitCh
	}

	result.DurationMs = time.Since(started).Milliseconds()
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()

	if err := runCtx.Err(); err != nil {
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		result.Canceled = errors.Is(err, context.Canceled)
	}

	if cmd.ProcessState != nil {
		result.ExitCode = cmd.ProcessState.ExitCode()
	}
	if waitErr == nil {
		if result.ExitCode < 0 {
			result.ExitCode = 0
		}
		return result, nil
	}

	var exitErr *exec.ExitError
	if errors.As(waitErr, &exitErr) {
		if result.ExitCode < 0 {
			result.ExitCode = exitErr.ExitCode()
		}
		return result, nil
	}
	return result, fmt.Errorf("wait for command: %w", waitErr)
}

func resolveWorkingDir(ws *workspace.Workspace, workingDir string) (string, error) {
	if strings.TrimSpace(workingDir) == "" {
		return ws.Root, nil
	}
	resolved, err := ws.ResolveExistingPath(workingDir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("access working directory %q: %w", workingDir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory %q is not a directory", workingDir)
	}
	return resolved, nil
}

func buildEnv(inherit bool, extra map[string]string) []string {
	values := map[string]string{}
	if inherit {
		for _, entry := range os.Environ() {
			idx := strings.IndexByte(entry, '=')
			if idx <= 0 {
				continue
			}
			values[entry[:idx]] = entry[idx+1:]
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, key := range keys {
		env = append(env, key+"="+values[key])
	}
	return env
}
