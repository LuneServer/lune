package stream

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// The fixtures below reproduce the event structure of Aegisub karaoke templates as used in fansub
// openings/endings (lyrics are made up): every glyph or syllable is its own \pos event, word gaps
// are separate whitespace events, and lines overlap during their lead-in/fade-out.

const karaokeHeader = `[Script Info]
PlayResX: 1920
PlayResY: 1080

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,72,&H00FFFFFF,&H000000FF,&H00000000,&H80000000,0,0,0,0,100,100,0,0,1,3.6,1.5,2,120,120,60,1
Style: OPR,Century Schoolbook,62,&H00111618,&H000000FF,&H00F8FAFB,&H00000000,-1,0,0,0,100,100,0,0,1,4,0,5,10,10,10,1
Style: OPE,Century Schoolbook,62,&H00111618,&H000000FF,&H00F8FAFB,&H00000000,-1,0,0,0,100,100,0,0,1,4,0,5,10,10,10,1
Style: EDE,Kronika,72,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,4,0,5,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
`

// fontWidth is a "real font" glyph width that deliberately differs from the converter's table.
func fontWidth(r rune, fs float64) float64 {
	return glyphEm(r) * fs * (1 + 0.18*math.Sin(float64(r)*1.7))
}

func assTime(ms int64) string {
	return fmt.Sprintf("%d:%02d:%02d.%02d", ms/3_600_000, ms/60_000%60, ms/1000%60, ms/10%100)
}

type karaokeBuilder struct{ b strings.Builder }

func (k *karaokeBuilder) event(layer int, start, end int64, style string, x, y float64, tags, text string) {
	fmt.Fprintf(&k.b, "Dialogue: %d,%s,%s,%s,,0,0,0,fx,{\\pos(%.1f, %.0f)%s}%s\n",
		layer, assTime(start), assTime(end), style, x, y, tags, text)
}

// charLine emits a "template char" line: every glyph (spaces included when withSpaces) revealed
// 30ms apart, staying until the line's fade-out ends.
func (k *karaokeBuilder) charLine(style string, y, fs float64, start, end int64, text string, withSpaces bool) {
	var total float64
	for _, r := range text {
		total += fontWidth(r, fs)
	}
	left := 960 - total/2
	for ci, r := range []rune(text) {
		w := fontWidth(r, fs)
		if r != ' ' || withSpaces {
			for layer := 0; layer < 2; layer++ {
				k.event(layer, start-300+int64(ci)*30, end+300, style, left+w/2, y, `\blur1\fad(0,300)`, string(r))
			}
		}
		left += w
	}
}

// sylLine emits the romaji structure: per-character events until each syllable is sung, then a
// highlight event while it is sung, then the syllable until the line fades out. Syllables of a
// word touch; words are separated by a space glyph.
func (k *karaokeBuilder) sylLine(style string, y, fs float64, start, end int64, words [][]string, withSpaces bool) {
	type syl struct {
		text string
		x    float64
		left float64
	}
	var sylls []syl
	var total float64
	for wi, w := range words {
		for _, s := range w {
			for _, r := range s {
				total += fontWidth(r, fs)
			}
		}
		if wi < len(words)-1 {
			total += fontWidth(' ', fs)
		}
	}
	left := 960 - total/2
	for wi, w := range words {
		for _, s := range w {
			var sw float64
			for _, r := range s {
				sw += fontWidth(r, fs)
			}
			sylls = append(sylls, syl{text: s, x: left + sw/2, left: left})
			left += sw
		}
		if wi < len(words)-1 {
			sw := fontWidth(' ', fs)
			if withSpaces {
				// The char template also emits the word gap, until the next syllable is sung.
				sylls = append(sylls, syl{text: " ", x: left + sw/2, left: left})
			}
			left += sw
		}
	}
	step := (end - start) / int64(len(sylls))
	ci := 0
	for i, s := range sylls {
		sStart, sEnd := start+int64(i)*step, start+int64(i+1)*step
		if s.text == " " {
			for layer := 0; layer < 2; layer++ {
				k.event(layer, start-300+int64(ci)*30, sStart, style, s.x, y, `\alpha&HFF&`, " ")
			}
			ci++
			continue
		}
		cl := s.left
		for _, r := range s.text {
			w := fontWidth(r, fs)
			for layer := 0; layer < 2; layer++ {
				k.event(layer, start-300+int64(ci)*30, sStart, style, cl+w/2, y, `\alpha&HFF&\t(0,200,\alpha&H00&)`, string(r))
			}
			cl += w
			ci++
		}
		k.event(0, sStart, sEnd, style, s.x, y, `\bord7\blur4\t(\bord4\blur1)`, s.text)
		k.event(1, sStart, sEnd, style, s.x, y, `\bord0\c&H1735E6&\t(\c&H111618&)`, s.text)
		for layer := 0; layer < 2; layer++ {
			k.event(layer, sEnd, end+300, style, s.x, y, `\blur1\fad(0,300)`, s.text)
		}
	}
}

func lyricCues(t *testing.T, vtt string) []vttTestCue {
	t.Helper()
	var out []vttTestCue
	for _, c := range parseTestVTT(t, vtt) {
		if c.settings != "" {
			out = append(out, c)
		}
	}
	return out
}

func TestASSKaraokeTemplateLines(t *testing.T) {
	for _, withSpaces := range []bool{true, false} {
		t.Run(fmt.Sprintf("space_events=%v", withSpaces), func(t *testing.T) {
			k := &karaokeBuilder{}
			k.b.WriteString(karaokeHeader)
			english := []string{
				"La-ta-ta-ta, when I'm with you,",
				"I'll still follow the moonlit door.",
				"Swimming home with willow tea",
			}
			romaji := [][][]string{
				{{"la", "tta", "tta", "tta"}, {"ki", "mi"}, {"to"}, {"na", "ra"}},
				{{"tsu", "ki"}, {"no"}, {"to", "bi", "ra"}, {"a", "ke", "te"}},
				{{"ka", "e", "ri"}, {"mi", "chi"}, {"o", "yo", "gu"}},
			}
			var start int64 = 120_000
			for i := range english {
				end := start + 4000
				k.charLine("OPE", 1020, 62, start, end, english[i], withSpaces)
				k.sylLine("OPR", 60, 62, start, end, romaji[i], withSpaces)
				start = end + 200 // next line's lead-in overlaps this line's fade-out
			}
			k.b.WriteString("Dialogue: 0,0:02:01.00,0:02:03.00,Default,,0,0,0,,Dialogue under the song.\n")

			vtt := string(ASSToWebVTT([]byte(k.b.String())))
			var gotTop, gotBottom []string
			for _, c := range lyricCues(t, vtt) {
				if strings.HasPrefix(c.settings, "line:8%") {
					gotTop = append(gotTop, c.text)
				} else {
					gotBottom = append(gotBottom, c.text)
				}
			}
			wantTop := []string{"lattattatta kimi to nara", "tsuki no tobira akete", "kaeri michi oyogu"}
			check := func(kind string, got, want []string) {
				if withSpaces {
					// The source's word-gap events give the exact spacing.
					if strings.Join(got, "|") != strings.Join(want, "|") {
						t.Errorf("%s cues\n got %q\nwant %q\n%s", kind, got, want, vtt)
					}
					return
				}
				// Without gap events (what AetherEngine hands the tvOS overlay) spaces are
				// estimated from glyph widths: letters and line boundaries must be exact, and at
				// most one word gap per line may be misjudged under the ±18% width error.
				if len(got) != len(want) {
					t.Fatalf("%s: want one cue per line\n got %q\nwant %q\n%s", kind, got, want, vtt)
				}
				for i := range want {
					if strings.ReplaceAll(got[i], " ", "") != strings.ReplaceAll(want[i], " ", "") {
						t.Errorf("%s line %d letters\n got %q\nwant %q", kind, i, got[i], want[i])
					}
					if d := strings.Count(got[i], " ") - strings.Count(want[i], " "); d < -1 || d > 1 {
						t.Errorf("%s line %d spacing\n got %q\nwant %q", kind, i, got[i], want[i])
					}
				}
			}
			check("romaji", gotTop, wantTop)
			check("english", gotBottom, english)
			if !strings.Contains(vtt, "00:02:01.000 --> 00:02:03.000\nDialogue under the song.\n") {
				t.Errorf("dialogue during the song changed:\n%s", vtt)
			}
		})
	}
}

// Endings often crossfade whole lines at the same spot: the next line fades in while the previous
// one is still fading out.
func TestASSKaraokeCrossfadeLines(t *testing.T) {
	k := &karaokeBuilder{}
	k.b.WriteString(karaokeHeader)
	lines := []string{"The people drifting through town", "look as far away as ever.", "So stay right here."}
	var start int64 = 1_300_000
	for _, l := range lines {
		for layer := 0; layer < 2; layer++ {
			k.event(layer, start-300, start+3000+1000, "EDE", 960, 1020, `\blur20\t(0,300,\blur1)\fad(100,0)`, l)
		}
		start += 3200
	}
	vtt := string(ASSToWebVTT([]byte(k.b.String())))
	var got []string
	for _, c := range lyricCues(t, vtt) {
		got = append(got, c.text)
	}
	if strings.Join(got, "|") != strings.Join(lines, "|") {
		t.Fatalf("crossfaded lines\n got %q\nwant %q\n%s", got, lines, vtt)
	}
}
