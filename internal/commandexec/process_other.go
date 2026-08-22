//go:build !unix

package commandexec

import "os/exec"

func configureCommand(cmd *exec.Cmd) {}

func terminateCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
