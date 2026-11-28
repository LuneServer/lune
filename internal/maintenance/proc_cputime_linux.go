//go:build linux

package maintenance

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// processCPUTime reports the user+system CPU time a running process has used so far.
func processCPUTime(cmd *exec.Cmd) (time.Duration, bool) {
	if cmd == nil || cmd.Process == nil {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", cmd.Process.Pid))
	if err != nil {
		return 0, false
	}
	return parseProcStatCPU(data)
}
