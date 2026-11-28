package stream

import (
	"bytes"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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
	idxFirst := strings.Index(vtt, "00:00:01.000 --> 00:00:03.400 line:6%,start align:center\nFirst line, with comma!")
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

func readTestdata(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The sidecar golden was produced by the converter before ASS grouping/positioning existed:
// ordinary bottom-aligned dialogue must stay byte-for-byte identical.
func TestASSPlainDialogueSidecarGolden(t *testing.T) {
	got := ASSToWebVTT(readTestdata(t, "dialogue.ass"))
	want := readTestdata(t, "dialogue.sidecar.vtt")
	if !bytes.Equal(got, want) {
		t.Fatalf("plain dialogue output changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Embedded tracks used to go through FFmpeg's WebVTT muxer. The golden matches its cue text for
// the same script (FFmpeg writes MM:SS.mmm timestamps and leaves `\h` unconverted).
func TestASSPlainDialogueEmbeddedGolden(t *testing.T) {
	src := readTestdata(t, "dialogue.ass")
	got := ASSToWebVTTFormatted(src)
	want := readTestdata(t, "dialogue.embedded.vtt")
	if !bytes.Equal(got, want) {
		t.Fatalf("embedded plain dialogue output changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	var streamed bytes.Buffer
	if err := StreamASSToWebVTT(bytes.NewReader(src), &streamed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed.Bytes(), want) {
		t.Fatalf("streamed output differs from batch output\n%s", streamed.String())
	}
}

func TestASSFormattingTags(t *testing.T) {
	ass := `[Script Info]
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,72,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,-1,0,0,0,100,100,0,0,1,3.6,1.5,2,120,120,60,1
Style: Thoughts,Arial,72,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,0,1,0,0,100,100,0,0,1,3.6,1.5,2,120,120,60,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,Plain {\i1}italic{\i0} & <tag>\Nsecond
Dialogue: 0,0:00:03.00,0:00:04.00,Thoughts,,0,0,0,,I wonder...{\i0}not{\r} reset
Dialogue: 0,0:00:05.00,0:00:06.00,Default,,0,0,0,,{\b0\u1}under{\u0}
`
	want := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:02.000\n<b>Plain </b><b><i>italic</i></b><b> &amp; &lt;tag&gt;</b>\n<b>second</b>\n\n" +
		"00:00:03.000 --> 00:00:04.000\n<i>I wonder...</i>not<i> reset</i>\n\n" +
		"00:00:05.000 --> 00:00:06.000\n<u>under</u>\n\n"
	if got := string(ASSToWebVTTFormatted([]byte(ass))); got != want {
		t.Fatalf("formatted output\n--- got ---\n%q\n--- want ---\n%q", got, want)
	}
	plain := string(ASSToWebVTT([]byte(ass)))
	if strings.Contains(plain, "<b>") || strings.Contains(plain, "<i>") || !strings.Contains(plain, "Plain italic &amp; &lt;tag&gt;\nsecond") {
		t.Fatalf("sidecar output should be plain escaped text, got:\n%s", plain)
	}
}

type vttTestCue struct {
	start, end string
	settings   string
	text       string
}

func parseTestVTT(t *testing.T, vtt string) []vttTestCue {
	t.Helper()
	var cues []vttTestCue
	for _, block := range strings.Split(strings.TrimPrefix(vtt, "WEBVTT\n\n"), "\n\n") {
		if strings.TrimSpace(block) == "" {
			continue
		}
		timing, text, _ := strings.Cut(block, "\n")
		start, rest, ok := strings.Cut(timing, " --> ")
		if !ok {
			t.Fatalf("bad cue %q", block)
		}
		end, settings, _ := strings.Cut(rest, " ")
		cues = append(cues, vttTestCue{start: start, end: end, settings: settings, text: text})
	}
	return cues
}

var reVTTTag = regexp.MustCompile(`<[^>]+>`)

func cuesContaining(cues []vttTestCue, sub string) []vttTestCue {
	var out []vttTestCue
	for _, c := range cues {
		if strings.Contains(reVTTTag.ReplaceAllString(c.text, ""), sub) {
			out = append(out, c)
		}
	}
	return out
}

func TestASSAnimeSignsGolden(t *testing.T) {
	src := readTestdata(t, "anime_signs.ass")
	got := ASSToWebVTTFormatted(src)
	want := readTestdata(t, "anime_signs.vtt")
	if !bytes.Equal(got, want) {
		t.Fatalf("anime sign output changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	var streamed bytes.Buffer
	if err := StreamASSToWebVTT(bytes.NewReader(src), &streamed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed.Bytes(), want) {
		t.Fatalf("streamed output differs from batch output\n%s", streamed.String())
	}
}

func TestASSKaraokeFlattened(t *testing.T) {
	vtt := string(ASSToWebVTTFormatted(readTestdata(t, "anime_signs.ass")))
	if regexp.MustCompile(`<\d{2}:\d{2}`).MatchString(vtt) {
		t.Fatalf("karaoke must not produce inline timestamp tags:\n%s", vtt)
	}
	cues := parseTestVTT(t, vtt)

	// Inline \k karaoke: one plain cue.
	inline := cuesContaining(cues, "nara")
	if len(inline) != 2 {
		t.Fatalf("want one cue for the inline karaoke line and one for the templated one, got %+v", inline)
	}
	for _, c := range inline {
		if c.text != "latta tta tta kimi to nara" {
			t.Fatalf("karaoke line not flattened: %q", c.text)
		}
	}
	// Templated karaoke: nine syllable events plus highlight layers become one cue for the
	// whole line, with no stray single-syllable cues.
	templated := inline[1]
	if templated.start != "00:00:10.000" || templated.end != "00:00:14.000" {
		t.Fatalf("templated karaoke timing: %+v", templated)
	}
	for _, c := range cues {
		if c.text == "tta" || c.text == "la" {
			t.Fatalf("stray syllable cue: %+v", c)
		}
	}
}

func TestASSSignsGroupedAndClamped(t *testing.T) {
	cues := parseTestVTT(t, string(ASSToWebVTTFormatted(readTestdata(t, "anime_signs.ass"))))

	// Two events typeset as one sign become one two-line cue (not two overlapping boxes).
	can := cuesContaining(cues, "Delicious")
	if len(can) != 1 || reVTTTag.ReplaceAllString(can[0].text, "") != "Delicious\nGyokuro Tea" {
		t.Fatalf("can sign not merged: %+v", can)
	}
	// Dialogue shown during the sign stays its own default-placed cue.
	um := cuesContaining(cues, "Um...")
	if len(um) != 1 || um[0].settings != "" || um[0].text != "Um..." {
		t.Fatalf("dialogue during sign changed: %+v", um)
	}
	// Border/blur duplicate layers collapse into one cue.
	if adopt := cuesContaining(cues, "Please Adopt Me"); len(adopt) != 1 || strings.Count(adopt[0].text, "Please") != 1 {
		t.Fatalf("duplicate sign layers not collapsed: %+v", adopt)
	}
	// Separate signs on the same row stay separate.
	if l, r := cuesContaining(cues, "LEFT SIGN"), cuesContaining(cues, "RIGHT SIGN"); len(l) != 1 || len(r) != 1 || l[0].text == r[0].text {
		t.Fatalf("left/right signs merged: %+v %+v", l, r)
	}

	rePct := regexp.MustCompile(`(line|position):([\d.]+)%`)
	for _, c := range cues {
		if strings.Contains(c.text, "color") || strings.Contains(c.text, "<c") {
			t.Fatalf("ASS colours must not reach WebVTT: %q", c.text)
		}
		for _, m := range rePct.FindAllStringSubmatch(c.settings, -1) {
			if v, _ := strconv.ParseFloat(m[2], 64); v < 0 || v > 100 {
				t.Fatalf("cue %q placed off screen: %s", c.text, c.settings)
			}
		}
	}
	// The \an9 episode title anchored at the frame's right edge hangs from its top-right corner;
	// its centre is pulled back on screen, so the top edge moves up with it.
	title := cuesContaining(cues, "Episode 1")
	if len(title) != 1 || title[0].settings != "line:85.5%,start position:99.5%,line-right align:right" || !strings.HasPrefix(reVTTTag.ReplaceAllString(title[0].text, ""), "『君と運命の出会い。』\nEpisode 1") {
		t.Fatalf("episode title: %+v", title)
	}
	// \an1 / \an3 signs keep their bottom-left / bottom-right corners on their \pos points.
	if l := cuesContaining(cues, "LEFT SIGN"); l[0].settings != "line:92.6%,end position:3.1%,line-left align:left" {
		t.Fatalf("left sign: %+v", l)
	}
	if r := cuesContaining(cues, "RIGHT SIGN"); r[0].settings != "line:92.6%,end position:96.9%,line-right align:right" {
		t.Fatalf("right sign: %+v", r)
	}
	// Centre-anchored and mixed signs stay centred on their box.
	if adopt := cuesContaining(cues, "Please Adopt Me"); !strings.HasSuffix(adopt[0].settings, "position:50%,center align:center") {
		t.Fatalf("centred sign: %+v", adopt)
	}
}

// A right-aligned episode title typeset as one \pos event per line (and the second line split
// into words) is joined into one cue right-aligned on the shared right edge, hanging from its top.
func TestASSSplitRightAlignedSignKeepsAnchor(t *testing.T) {
	ass := `[Script Info]
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Title,Arial,54,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,0,9,40,40,40,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Dialogue: 0,0:00:20.00,0:00:25.00,Title,,0,0,0,,{\an9\pos(1800,108)}Episode 3
Dialogue: 0,0:00:20.00,0:00:25.00,Title,,0,0,0,,{\an9\pos(1800,162)}The Day the Moon Fell
Dialogue: 0,0:00:20.00,0:00:25.00,Title,,0,0,0,,{\an9\pos(1800,216)}Into the Sea
`
	cues := parseTestVTT(t, string(ASSToWebVTT([]byte(ass))))
	if len(cues) != 1 {
		t.Fatalf("want one joined cue, got %+v", cues)
	}
	c := cues[0]
	if c.text != "Episode 3\nThe Day the Moon Fell\nInto the Sea" {
		t.Fatalf("text: %q", c.text)
	}
	if c.settings != "line:10%,start position:93.8%,line-right align:right" {
		t.Fatalf("settings: %q", c.settings)
	}

	// The same sign with mixed anchors falls back to centring on its box.
	mixed := strings.Replace(ass, `{\an9\pos(1800,216)}`, `{\an7\pos(1500,216)}`, 1)
	cues = parseTestVTT(t, string(ASSToWebVTT([]byte(mixed))))
	if len(cues) != 1 || !strings.Contains(cues[0].settings, ",start position:") || !strings.HasSuffix(cues[0].settings, ",center align:center") {
		t.Fatalf("mixed anchors: %+v", cues)
	}
}

func TestStreamASSToWebVTTIsProgressive(t *testing.T) {
	pr, pw := io.Pipe()
	out := &lockedBuffer{}
	done := make(chan error, 1)
	go func() { done <- StreamASSToWebVTT(pr, out) }()

	header := "[Script Info]\nPlayResX: 1920\nPlayResY: 1080\n\n[Events]\n" +
		"Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: 0,0:00:01.00,0:00:02.00,Default,,0,0,0,,First\n" +
		"Dialogue: 0,0:00:03.00,0:00:04.00,Default,,0,0,0,,Second\n"
	if _, err := io.WriteString(pw, header); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(out.String(), "First") {
		if time.Now().After(deadline) {
			t.Fatalf("first cue not written before input ended; got %q", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if strings.Contains(out.String(), "Second") {
		t.Fatalf("pending cue flushed too early")
	}
	_ = pw.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "00:00:03.000 --> 00:00:04.000\nSecond") {
		t.Fatalf("last cue missing: %q", out.String())
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
