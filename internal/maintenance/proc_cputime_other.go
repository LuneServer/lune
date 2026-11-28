//go:build !linux

package maintenance

import (
	"os/exec"
	"time"
)

// processCPUTime is unavailable off Linux; callers fall back to unpaused wall time.
func processCPUTime(*exec.Cmd) (time.Duration, bool) { return 0, false }
