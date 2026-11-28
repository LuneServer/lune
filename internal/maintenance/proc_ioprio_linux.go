//go:build linux

package maintenance

import "syscall"

const (
	ioprioWhoProcess    = 1
	ioprioClassShift    = 13
	ioprioClassBE       = 2 // best-effort
	ioprioLowestBELevel = 7
)

// lowerIOPriority puts a background FFmpeg job at the lowest best-effort disk priority, so
// a stream reading from the same disk wins contention. The idle class is avoided on purpose:
// under steady playback reads it can starve the job until it is mistaken for a stalled one.
// Schedulers that ignore I/O priorities (e.g. none/mq-deadline) make this a no-op.
func lowerIOPriority(pid int) {
	_, _, _ = syscall.Syscall(syscall.SYS_IOPRIO_SET, ioprioWhoProcess, uintptr(pid),
		ioprioClassBE<<ioprioClassShift|ioprioLowestBELevel)
}
