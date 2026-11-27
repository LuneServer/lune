//go:build !unix

package maintenance

import "os/exec"

func lowerProcessPriority(*exec.Cmd) {}

// pauseProcess is unsupported off Unix; callers fall back to stopping the job.
func pauseProcess(*exec.Cmd) bool { return false }

func resumeProcess(*exec.Cmd) {}
