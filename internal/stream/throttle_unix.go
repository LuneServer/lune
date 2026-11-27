//go:build unix

package stream

import (
	"os/exec"
	"syscall"
)

func pauseProcess(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.Process == nil {
		return false
	}
	return syscall.Kill(cmd.Process.Pid, syscall.SIGSTOP) == nil
}

func resumeProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(cmd.Process.Pid, syscall.SIGCONT)
}
