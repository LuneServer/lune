//go:build unix

package maintenance

import (
	"os/exec"
	"syscall"
)

// lowerProcessPriority renices a background FFmpeg job so playback and the API always win
// CPU contention.
func lowerProcessPriority(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Setpriority(syscall.PRIO_PROCESS, cmd.Process.Pid, 15)
}

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
