package stream

import (
	"strings"
	"testing"
)

func TestASSToWebVTT(t *testing.T) {
	ass := `[Script Info]
Title: Test Anime

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:05.50,0:00:08.25,Default,,0,0,0,,{\i1}Second line{\i0}\NNext row
Dialogue: 0,0:00:01.00,0:00:03.40,Default,,0,0,0,,{\an8}First line, with comma!
Dialogue: 0,0:00:02.00,0:00:04.00,Sign,,0,0,0,,{\p1}m 0 0 l 100 0 100 100{\p0}
`
	vtt := string(ASSToWebVTT([]byte(ass)))
	if !strings.HasPrefix(vtt, "WEBVTT\n\n") {
		t.Fatalf("missing WEBVTT header: %q", vtt)
	}
	idxFirst := strings.Index(vtt, "00:00:01.000 --> 00:00:03.400\nFirst line, with comma!")
	idxSecond := strings.Index(vtt, "00:00:05.500 --> 00:00:08.250\nSecond line\nNext row")
	if idxFirst < 0 || idxSecond < 0 {
		t.Fatalf("missing expected cues in output:\n%s", vtt)
	}
	if idxFirst > idxSecond {
		t.Fatalf("expected cues sorted by start time, got:\n%s", vtt)
	}
	if strings.Contains(vtt, "m 0 0 l 100") || strings.Contains(vtt, `{\an8}`) || strings.Contains(vtt, `{\i1}`) {
		t.Fatalf("expected drawing commands and override tags stripped, got:\n%s", vtt)
	}
}
