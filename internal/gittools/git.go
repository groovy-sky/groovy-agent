package gittools

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var ErrGitUnavailable = errors.New("git is not available")

func Status(workspaceRoot string, maxOutputBytes int) (string, error) {
	return runGit(workspaceRoot, maxOutputBytes, "status", "--short")
}

func Diff(workspaceRoot string, maxOutputBytes int) (string, error) {
	return runGit(workspaceRoot, maxOutputBytes, "diff", "--no-color")
}

func runGit(workspaceRoot string, maxOutputBytes int, args ...string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", ErrGitUnavailable
	}
	fullArgs := append([]string{"-C", workspaceRoot}, args...)
	command := exec.Command("git", fullArgs...)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	output := stdout.String()
	if strings.TrimSpace(output) == "" && stderr.Len() > 0 {
		output = stderr.String()
	}
	if maxOutputBytes > 0 && len(output) > maxOutputBytes {
		output = output[:maxOutputBytes]
	}
	if err != nil {
		if strings.Contains(stderr.String(), "not a git repository") {
			return "", fmt.Errorf("workspace is not a git repository")
		}
		return "", fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}
	return output, nil
}
