package maintenance

import (
	"bytes"
	"strconv"
	"time"
)

// cpuTimeOf reports a running FFmpeg's CPU time; tests replace it.
var cpuTimeOf = processCPUTime

// clockTicksPerSecond is Linux's USER_HZ, the unit of /proc/<pid>/stat CPU times. It is part
// of the kernel ABI and 100 on every supported architecture.
const clockTicksPerSecond = 100

// parseProcStatCPU extracts utime+stime (fields 14 and 15) from a /proc/<pid>/stat line. The
// command name (field 2) is parenthesised and may contain spaces, so fields are counted from
// the last ')'.
func parseProcStatCPU(data []byte) (time.Duration, bool) {
	end := bytes.LastIndexByte(data, ')')
	if end < 0 {
		return 0, false
	}
	// After ')' come field 3 (state) onwards; utime and stime are fields 14 and 15.
	fields := bytes.Fields(data[end+1:])
	if len(fields) < 13 {
		return 0, false
	}
	utime, err1 := strconv.ParseUint(string(fields[11]), 10, 64)
	stime, err2 := strconv.ParseUint(string(fields[12]), 10, 64)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return time.Duration(utime+stime) * time.Second / clockTicksPerSecond, true
}
