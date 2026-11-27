package stream

import (
	"context"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestStreamSubtitleVTTEmbeddedASS runs the real embedded-track path (FFmpeg `-f ass` + Go
// conversion) against an MKV muxed from the anime sign fixture. Skipped without ffmpeg.
func TestStreamSubtitleVTTEmbeddedASS(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	dir := t.TempDir()
	mkv := filepath.Join(dir, "ep01.mkv")
	src, _ := filepath.Abs("testdata/anime_signs.ass")
	mux := exec.Command(ffmpeg, "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=black:s=64x36:d=50:r=1",
		"-i", src, "-map", "0", "-map", "1", "-c:v", "ffv1", "-c:s", "copy", mkv)
	if out, err := mux.CombinedOutput(); err != nil {
		t.Skipf("cannot mux test MKV: %v (%s)", err, out)
	}

	tr := NewTranscoder(dir, nil)
	defer tr.Close()
	if !tr.Available() {
		t.Skip("transcoder reports ffmpeg unavailable")
	}
	want := string(ASSToWebVTTFormatted(readTestdata(t, "anime_signs.ass")))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/stream/sub/x/embedded-1.vtt", nil)
	if err := tr.StreamSubtitleVTT(context.Background(), rec, req, "item_1", mkv, 1, "ass", 0); err != nil {
		t.Fatal(err)
	}
	if got := rec.Body.String(); got != want {
		t.Fatalf("embedded ASS output\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/vtt") {
		t.Fatalf("content type %q", ct)
	}
	cached := filepath.Join(dir, "transcode", "subs", "item_1_1.ass2.vtt")
	if b, err := os.ReadFile(cached); err != nil || string(b) != want {
		entries, _ := filepath.Glob(filepath.Join(dir, "*", "*", "*"))
		t.Fatalf("expected converted cache at %s (err %v); files: %v", cached, err, entries)
	}

	path, err := tr.ExtractSubtitleVTT(context.Background(), "item_2", mkv, 1, "ass")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != want {
		t.Fatalf("ExtractSubtitleVTT output\n%s", b)
	}
}
