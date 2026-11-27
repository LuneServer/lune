//go:build !unix

package stream

import "os/exec"

func pauseProcess(_ *exec.Cmd) bool {
	return false
}

func resumeProcess(_ *exec.Cmd) {}
