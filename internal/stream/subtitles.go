package stream

import (
	"bytes"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

var (
	reSRTTimestamp = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})(\s+-->\s+)(\d{2}:\d{2}:\d{2}),(\d{3})`)
	reASSDrawing   = regexp.MustCompile(`\\p[1-9]`)
)

// SRTToWebVTT converts a SubRip (.srt) payload into a standards-compliant WebVTT file
// without invoking FFmpeg.
func SRTToWebVTT(raw []byte) []byte {
	// Strip UTF-8 BOM if present.
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	converted := reSRTTimestamp.ReplaceAllString(text, "$1.$2$3$4.$5")

	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")
	out.WriteString(strings.TrimLeft(converted, "\n"))
	return out.Bytes()
}

func parseASSTime(raw string) (int64, bool) {
	s := strings.TrimSpace(raw)
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.Atoi(parts[0])
	m, err2 := strconv.Atoi(parts[1])
	secParts := strings.Split(parts[2], ".")
	if err1 != nil || err2 != nil || len(secParts) == 0 {
		return 0, false
	}
	sec, err3 := strconv.Atoi(secParts[0])
	if err3 != nil {
		return 0, false
	}
	var ms int
	if len(secParts) > 1 {
		frac := secParts[1]
		if len(frac) == 1 {
			frac += "00"
		} else if len(frac) == 2 {
			frac += "0"
		} else if len(frac) > 3 {
			frac = frac[:3]
		}
		ms, _ = strconv.Atoi(frac)
	}
	totalMS := (int64(h)*3600+int64(m)*60+int64(sec))*1000 + int64(ms)
	return totalMS, true
}

func formatVTTTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	h := ms / 3_600_000
	ms %= 3_600_000
	m := ms / 60_000
	ms %= 60_000
	s := ms / 1000
	rem := ms % 1000
	return fmt.Sprintf("%02d:%02d:%02d.%03d", h, m, s, rem)
}
