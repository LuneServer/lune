package stream

import (
	"bytes"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	reSRTTimestamp = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}),(\d{3})(\s+-->\s+)(\d{2}:\d{2}:\d{2}),(\d{3})`)
	reASSTags      = regexp.MustCompile(`\{[^}]*\}`)
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

type assCue struct {
	startMS int64
	endMS   int64
	text    string
}

// ASSToWebVTT converts SubStation Alpha (.ass / .ssa) subtitle scripts commonly used in anime
// into standards-compliant WebVTT without invoking FFmpeg.
func ASSToWebVTT(raw []byte) []byte {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	inEvents := false
	startCol, endCol, textCol, totalCols := 1, 2, 9, 10
	var cues []assCue

	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inEvents = strings.EqualFold(trimmed, "[Events]")
			continue
		}
		if !inEvents {
			continue
		}
		if strings.HasPrefix(strings.ToLower(trimmed), "format:") {
			fields := strings.Split(trimmed[len("format:"):], ",")
			totalCols = len(fields)
			for i, f := range fields {
				switch strings.ToLower(strings.TrimSpace(f)) {
				case "start":
					startCol = i
				case "end":
					endCol = i
				case "text":
					textCol = i
				}
			}
			continue
		}
		if !strings.HasPrefix(strings.ToLower(trimmed), "dialogue:") {
			continue
		}
		payload := strings.TrimSpace(trimmed[len("dialogue:"):])
		cols := strings.SplitN(payload, ",", totalCols)
		if len(cols) <= textCol || len(cols) <= startCol || len(cols) <= endCol {
			continue
		}
		startMS, ok1 := parseASSTime(cols[startCol])
		endMS, ok2 := parseASSTime(cols[endCol])
		if !ok1 || !ok2 || endMS <= startMS {
			continue
		}
		rawDialogue := cols[textCol]
		// Skip vector drawing commands (e.g. {\p1}m 0 0 l 100 100...) used for typesetting shapes.
		if reASSDrawing.MatchString(rawDialogue) {
			continue
		}
		clean := cleanASSText(rawDialogue)
		if clean == "" {
			continue
		}
		cues = append(cues, assCue{startMS: startMS, endMS: endMS, text: clean})
	}

	sort.SliceStable(cues, func(i, j int) bool {
		if cues[i].startMS != cues[j].startMS {
			return cues[i].startMS < cues[j].startMS
		}
		return cues[i].endMS < cues[j].endMS
	})

	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		out.WriteString(formatVTTTime(c.startMS))
		out.WriteString(" --> ")
		out.WriteString(formatVTTTime(c.endMS))
		out.WriteByte('\n')
		out.WriteString(c.text)
		out.WriteString("\n\n")
	}
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

func cleanASSText(raw string) string {
	s := reASSTags.ReplaceAllString(raw, "")
	s = strings.NewReplacer(`\N`, "\n", `\n`, "\n", `\h`, " ").Replace(s)
	lines := strings.Split(s, "\n")
	var kept []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l != "" {
			kept = append(kept, l)
		}
	}
	return strings.Join(kept, "\n")
}
