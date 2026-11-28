package stream

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ASS/SSA -> WebVTT conversion.
//
// Text clients (lune-web, the Apple TV AVPlayer path, AetherEngine sidecars) draw every WebVTT cue
// as its own boxed caption, so a literal one-event-per-cue translation of typeset anime subtitles
// looks broken: karaoke templates emit one event per syllable, typesetters split a multi-line sign
// into one event per line (plus duplicate border/blur layers), and FFmpeg's own WebVTT muxer
// turns `\k` karaoke into inline `<00:00:01.000>` timestamp tags. The converter therefore:
//
//   - strips every override tag, flattening karaoke (`\k`, `\kf`, `\K`, `\ko`) into plain text;
//   - never carries ASS colours, so dark sign text can never land on a dark caption box;
//   - groups positioned (`\pos`/`\move`) events that are shown at the same time and sit on the
//     same row (karaoke syllables) or directly above/below each other (split signs) into one cue,
//     joining rows with a newline and dropping duplicate layers;
//   - maps placement to clamped WebVTT `line`/`position` settings so cues stay on screen.
//
// Ordinary bottom-aligned dialogue is passed through exactly as before: one cue per event, sorted
// by start time, with no cue settings.

type assOptions struct {
	// formatting emits <b>/<i>/<u> from style flags and \b/\i/\u overrides. Embedded tracks use it
	// so their output matches what FFmpeg's WebVTT muxer used to produce; sidecars keep the
	// historical plain-text output.
	formatting bool
}

type assStyle struct {
	fontSize  float64
	bold      bool
	italic    bool
	underline bool
	alignment int
}

type assFormat struct {
	bold, italic, underline bool
}

type assRun struct {
	text string
	f    assFormat
}

type assEvent struct {
	seq        int
	start, end int64
	lines      [][]assRun
	plain      string
	alignment  int
	positioned bool
	x, y       float64
	fontSize   float64
	// space marks a positioned whitespace-only event. Karaoke templates emit one event per
	// character or syllable and the word gaps as events of their own, so these carry the
	// source spacing of the line.
	space bool
}

// vttCue is one output cue.
type vttCue struct {
	seq        int
	start, end int64
	settings   string
	text       string
}

// ASSToWebVTT converts SubStation Alpha (.ass / .ssa) subtitle scripts commonly used in anime
// into standards-compliant plain-text WebVTT without invoking FFmpeg.
func ASSToWebVTT(raw []byte) []byte {
	return convertASS(raw, assOptions{})
}

// ASSToWebVTTFormatted is ASSToWebVTT keeping bold/italic/underline as WebVTT <b>/<i>/<u> tags.
func ASSToWebVTTFormatted(raw []byte) []byte {
	return convertASS(raw, assOptions{formatting: true})
}

func convertASS(raw []byte, opts assOptions) []byte {
	raw = bytes.TrimPrefix(raw, []byte{0xEF, 0xBB, 0xBF})
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")

	p := newASSParser(opts)
	var events []*assEvent
	for _, line := range strings.Split(text, "\n") {
		if ev := p.parseLine(line); ev != nil {
			events = append(events, ev)
		}
	}
	// Sidecar scripts are often not sorted by time; grouping sweeps in start order.
	sort.SliceStable(events, func(i, j int) bool { return events[i].start < events[j].start })

	g := &assGrouper{p: p}
	var cues []vttCue
	for _, ev := range events {
		cues = append(cues, g.add(ev)...)
	}
	cues = append(cues, g.flushAll()...)
	sortVTTCues(cues)

	var out bytes.Buffer
	out.WriteString("WEBVTT\n\n")
	for _, c := range cues {
		writeVTTCue(&out, c)
	}
	return out.Bytes()
}

// StreamASSToWebVTT converts an ASS script read progressively from r (e.g. FFmpeg's `-f ass`
// output for an embedded track) into WebVTT on w. Cues are written as soon as no later event can
// still be grouped with them, so the first cues reach the player without reading the whole file.
//
// Nothing is written when r yields no data, so a caller can still report a failed extraction.
func StreamASSToWebVTT(r io.Reader, w io.Writer) error {
	p := newASSParser(assOptions{formatting: true})
	g := &assGrouper{p: p}
	write := func(cues []vttCue) error {
		if len(cues) == 0 {
			return nil
		}
		sortVTTCues(cues)
		var buf bytes.Buffer
		for _, c := range cues {
			writeVTTCue(&buf, c)
		}
		_, err := w.Write(buf.Bytes())
		return err
	}

	br := bufio.NewReaderSize(r, 64*1024)
	first := true
	for {
		line, readErr := br.ReadString('\n')
		if line != "" {
			if first {
				if _, err := io.WriteString(w, "WEBVTT\n\n"); err != nil {
					return err
				}
				line = strings.TrimPrefix(line, "\uFEFF")
				first = false
			}
			line = strings.TrimRight(line, "\r\n")
			if ev := p.parseLine(line); ev != nil {
				if err := write(g.add(ev)); err != nil {
					return err
				}
			}
		}
		if readErr != nil {
			if err := write(g.flushAll()); err != nil {
				return err
			}
			if readErr == io.EOF {
				return nil
			}
			return readErr
		}
	}
}

func sortVTTCues(cues []vttCue) {
	sort.SliceStable(cues, func(i, j int) bool {
		if cues[i].start != cues[j].start {
			return cues[i].start < cues[j].start
		}
		if cues[i].end != cues[j].end {
			return cues[i].end < cues[j].end
		}
		return cues[i].seq < cues[j].seq
	})
}

func writeVTTCue(out *bytes.Buffer, c vttCue) {
	out.WriteString(formatVTTTime(c.start))
	out.WriteString(" --> ")
	out.WriteString(formatVTTTime(c.end))
	if c.settings != "" {
		out.WriteByte(' ')
		out.WriteString(c.settings)
	}
	out.WriteByte('\n')
	out.WriteString(c.text)
	out.WriteString("\n\n")
}

// ---------------------------------------------------------------------------------------------
// Script parsing

type assParser struct {
	opts      assOptions
	section   string
	legacySSA bool

	playResX, playResY float64

	styleCols map[string]int
	styles    map[string]assStyle

	startCol, endCol, styleCol, textCol, totalCols int
	seq                                            int
}

func newASSParser(opts assOptions) *assParser {
	return &assParser{
		opts:     opts,
		styles:   map[string]assStyle{},
		startCol: 1, endCol: 2, styleCol: 3, textCol: 9, totalCols: 10,
	}
}

// resolution returns the script's play resolution using libass's defaults for missing values.
func (p *assParser) resolution() (float64, float64) {
	x, y := p.playResX, p.playResY
	switch {
	case x <= 0 && y <= 0:
		return 384, 288
	case y <= 0:
		if x == 1280 {
			return x, 1024
		}
		return x, x * 3 / 4
	case x <= 0:
		if y == 1024 {
			return 1280, y
		}
		return y * 4 / 3, y
	}
	return x, y
}

func (p *assParser) parseLine(line string) *assEvent {
	trimmed := strings.TrimSpace(line)
	if trimmed == "" || strings.HasPrefix(trimmed, ";") {
		return nil
	}
	if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
		p.section = strings.ToLower(trimmed)
		if p.section == "[v4 styles]" {
			p.legacySSA = true
		}
		return nil
	}
	lower := strings.ToLower(trimmed)
	switch p.section {
	case "[script info]":
		if k, v, ok := strings.Cut(trimmed, ":"); ok {
			switch strings.ToLower(strings.TrimSpace(k)) {
			case "playresx":
				p.playResX, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			case "playresy":
				p.playResY, _ = strconv.ParseFloat(strings.TrimSpace(v), 64)
			}
		}
	case "[v4+ styles]", "[v4 styles]", "[v4 styles+]":
		if strings.HasPrefix(lower, "format:") {
			p.styleCols = map[string]int{}
			for i, f := range strings.Split(trimmed[len("format:"):], ",") {
				p.styleCols[strings.ToLower(strings.TrimSpace(f))] = i
			}
		} else if strings.HasPrefix(lower, "style:") {
			p.parseStyle(trimmed[len("style:"):])
		}
	case "[events]":
		if strings.HasPrefix(lower, "format:") {
			fields := strings.Split(trimmed[len("format:"):], ",")
			p.totalCols = len(fields)
			for i, f := range fields {
				switch strings.ToLower(strings.TrimSpace(f)) {
				case "start":
					p.startCol = i
				case "end":
					p.endCol = i
				case "style":
					p.styleCol = i
				case "text":
					p.textCol = i
				}
			}
			return nil
		}
		if strings.HasPrefix(lower, "dialogue:") {
			// Keep trailing white space: a karaoke word gap is an event whose text is " ".
			raw := strings.TrimLeftFunc(line, unicode.IsSpace)
			return p.parseDialogue(strings.TrimLeftFunc(raw[len("dialogue:"):], unicode.IsSpace))
		}
	}
	return nil
}

func (p *assParser) parseStyle(payload string) {
	cols := strings.Split(payload, ",")
	get := func(name string) (string, bool) {
		if p.styleCols == nil {
			return "", false
		}
		i, ok := p.styleCols[name]
		if !ok || i >= len(cols) {
			return "", false
		}
		return strings.TrimSpace(cols[i]), true
	}
	name, ok := get("name")
	if !ok {
		return
	}
	st := assStyle{alignment: 2}
	if v, ok := get("fontsize"); ok {
		st.fontSize, _ = strconv.ParseFloat(v, 64)
	}
	flag := func(v string) bool {
		n, err := strconv.Atoi(v)
		return err == nil && n != 0
	}
	if v, ok := get("bold"); ok {
		st.bold = flag(v)
	}
	if v, ok := get("italic"); ok {
		st.italic = flag(v)
	}
	if v, ok := get("underline"); ok {
		st.underline = flag(v)
	}
	if v, ok := get("alignment"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			if p.legacySSA {
				n = legacySSAAlignment(n)
			}
			if n >= 1 && n <= 9 {
				st.alignment = n
			}
		}
	}
	p.styles[strings.ToLower(strings.TrimPrefix(name, "*"))] = st
}

// legacySSAAlignment maps SSA v4 alignment (1-3 bottom, 5-7 top, 9-11 middle) to numpad layout.
func legacySSAAlignment(n int) int {
	switch {
	case n >= 1 && n <= 3:
		return n
	case n >= 5 && n <= 7:
		return n + 2
	case n >= 9 && n <= 11:
		return n - 5
	}
	return 0
}

func (p *assParser) style(name string) assStyle {
	name = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(name), "*"))
	if st, ok := p.styles[name]; ok {
		return st
	}
	if st, ok := p.styles["default"]; ok {
		return st
	}
	return assStyle{alignment: 2}
}

func (p *assParser) parseDialogue(payload string) *assEvent {
	cols := strings.SplitN(payload, ",", p.totalCols)
	if len(cols) <= p.textCol || len(cols) <= p.startCol || len(cols) <= p.endCol {
		return nil
	}
	startMS, ok1 := parseASSTime(cols[p.startCol])
	endMS, ok2 := parseASSTime(cols[p.endCol])
	if !ok1 || !ok2 || endMS <= startMS {
		return nil
	}
	rawText := cols[p.textCol]
	// Skip vector drawing commands (e.g. {\p1}m 0 0 l 100 100...) used for typesetting shapes.
	if reASSDrawing.MatchString(rawText) {
		return nil
	}
	styleName := ""
	if p.styleCol < len(cols) {
		styleName = cols[p.styleCol]
	}
	st := p.style(styleName)

	ev := &assEvent{start: startMS, end: endMS, alignment: st.alignment, fontSize: st.fontSize}
	hadText := p.parseText(ev, rawText, st)
	if len(ev.lines) == 0 {
		if !hadText || !ev.positioned {
			return nil
		}
		ev.space, ev.plain = true, " "
	}
	if ev.fontSize <= 0 {
		_, resY := p.resolution()
		ev.fontSize = resY / 18
	}
	p.seq++
	ev.seq = p.seq
	return ev
}

func (s assStyle) format() assFormat {
	return assFormat{bold: s.bold, italic: s.italic, underline: s.underline}
}

// parseText splits ASS dialogue text into formatted runs per visual line, applying override
// blocks. Unknown tags (karaoke, colours, transforms, clips, fades...) are dropped.
// parseText reports whether the event had any text at all before trimming (so a whitespace-only
// positioned event can be kept as a spacing marker).
func (p *assParser) parseText(ev *assEvent, raw string, st assStyle) bool {
	cur := st.format()
	lines := [][]assRun{nil}
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		l := lines[len(lines)-1]
		if n := len(l); n > 0 && l[n-1].f == cur {
			l[n-1].text += b.String()
		} else {
			l = append(l, assRun{text: b.String(), f: cur})
		}
		lines[len(lines)-1] = l
		b.Reset()
	}
	alignSet, posSet := false, false

	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case c == '{':
			end := strings.IndexByte(raw[i+1:], '}')
			if end < 0 {
				b.WriteByte(c)
				continue
			}
			block := raw[i+1 : i+1+end]
			i += end + 1
			// Text written so far keeps the formatting it was written with.
			flush()
			p.applyOverrides(block, st, &cur, ev, &alignSet, &posSet)
		case c == '\\' && i+1 < len(raw) && (raw[i+1] == 'N' || raw[i+1] == 'n'):
			flush()
			lines = append(lines, nil)
			i++
		case c == '\\' && i+1 < len(raw) && raw[i+1] == 'h':
			b.WriteByte(' ')
			i++
		default:
			b.WriteByte(c)
		}
	}
	flush()

	hadText := false
	for _, l := range lines {
		if len(l) > 0 {
			hadText = true
		}
	}
	var kept [][]assRun
	var plain []string
	for _, l := range lines {
		l = trimRuns(l)
		if len(l) == 0 {
			continue
		}
		kept = append(kept, l)
		plain = append(plain, runsPlain(l))
	}
	ev.lines = kept
	ev.plain = strings.Join(plain, "\n")
	return hadText
}

func (p *assParser) applyOverrides(block string, st assStyle, cur *assFormat, ev *assEvent, alignSet, posSet *bool) {
	for i := 0; i < len(block); {
		if block[i] != '\\' {
			i++
			continue
		}
		i++
		j := i
		for j < len(block) && block[j] != '\\' && block[j] != '(' {
			j++
		}
		name := strings.TrimSpace(block[i:j])
		args := ""
		if j < len(block) && block[j] == '(' {
			depth := 0
			k := j
			for ; k < len(block); k++ {
				if block[k] == '(' {
					depth++
				} else if block[k] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			if k >= len(block) {
				args = block[j+1:]
				j = len(block)
			} else {
				args = block[j+1 : k]
				j = k + 1
			}
		}
		i = j
		p.applyTag(name, args, st, cur, ev, alignSet, posSet)
	}
}

func (p *assParser) applyTag(name, args string, st assStyle, cur *assFormat, ev *assEvent, alignSet, posSet *bool) {
	switch {
	case name == "pos" || name == "move":
		if *posSet {
			return
		}
		parts := strings.Split(args, ",")
		if len(parts) < 2 {
			return
		}
		x, err1 := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
		y, err2 := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
		if err1 != nil || err2 != nil {
			return
		}
		ev.positioned, ev.x, ev.y = true, x, y
		*posSet = true
	case strings.HasPrefix(name, "an"):
		if n, err := strconv.Atoi(name[2:]); err == nil && n >= 1 && n <= 9 && !*alignSet {
			ev.alignment = n
			*alignSet = true
		}
	case strings.HasPrefix(name, "a") && isDigits(name[1:]):
		if n, err := strconv.Atoi(name[1:]); err == nil && !*alignSet {
			if a := legacySSAAlignment(n); a != 0 {
				ev.alignment = a
				*alignSet = true
			}
		}
	case strings.HasPrefix(name, "fs") && len(name) > 2:
		if v, err := strconv.ParseFloat(name[2:], 64); err == nil && v > 0 {
			ev.fontSize = math.Max(ev.fontSize, v)
		}
	case name == "b" || (strings.HasPrefix(name, "b") && isDigits(name[1:])):
		if name == "b" {
			cur.bold = st.bold
			return
		}
		n, _ := strconv.Atoi(name[1:])
		cur.bold = n == 1 || n >= 600
	case name == "i" || (strings.HasPrefix(name, "i") && isDigits(name[1:])):
		if name == "i" {
			cur.italic = st.italic
			return
		}
		cur.italic = name[1:] != "0"
	case name == "u" || (strings.HasPrefix(name, "u") && isDigits(name[1:])):
		if name == "u" {
			cur.underline = st.underline
			return
		}
		cur.underline = name[1:] != "0"
	case strings.HasPrefix(name, "r"):
		if name == "r" {
			*cur = st.format()
		} else {
			*cur = p.style(name[1:]).format()
		}
	}
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// trimRuns trims leading/trailing white space across a line's runs and drops empty runs.
func trimRuns(l []assRun) []assRun {
	for len(l) > 0 {
		l[0].text = strings.TrimLeftFunc(l[0].text, unicode.IsSpace)
		if l[0].text != "" {
			break
		}
		l = l[1:]
	}
	for len(l) > 0 {
		n := len(l) - 1
		l[n].text = strings.TrimRightFunc(l[n].text, unicode.IsSpace)
		if l[n].text != "" {
			break
		}
		l = l[:n]
	}
	return l
}

func runsPlain(l []assRun) string {
	var b strings.Builder
	for _, r := range l {
		b.WriteString(r.text)
	}
	return b.String()
}

var reVTTMarkup = regexp.MustCompile(`</?[biu]>`)

var vttEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func renderRuns(l []assRun, formatting bool) string {
	if !formatting {
		return vttEscaper.Replace(runsPlain(l))
	}
	var b strings.Builder
	var open assFormat
	closeAll := func() {
		if open.underline {
			b.WriteString("</u>")
		}
		if open.italic {
			b.WriteString("</i>")
		}
		if open.bold {
			b.WriteString("</b>")
		}
		open = assFormat{}
	}
	for _, r := range l {
		if r.text == "" {
			continue
		}
		if r.f != open {
			closeAll()
			if r.f.bold {
				b.WriteString("<b>")
			}
			if r.f.italic {
				b.WriteString("<i>")
			}
			if r.f.underline {
				b.WriteString("<u>")
			}
			open = r.f
		}
		b.WriteString(vttEscaper.Replace(r.text))
	}
	closeAll()
	return b.String()
}

// ---------------------------------------------------------------------------------------------
// Grouping

// assGrouper collects events in start order and emits cues once their group is complete: a
// group can only grow through events that overlap it in time, so once the newest event starts at
// or after a group's end, nothing later can join it. Groups are kept in an incremental union-find
// so a long karaoke chain (one event per character for a whole opening) stays linear per event.
type assGrouper struct {
	p *assParser
	// persist maps a positioned event to the time its text stays on screen at its spot through
	// the chain of events that redraw it (karaoke char -> highlight -> base); set per group.
	persist   map[*assEvent]int64
	pending   []*assEvent
	boxes     []assBox
	parent    []int
	lastStart int64
}

func (g *assGrouper) find(i int) int {
	for g.parent[i] != i {
		g.parent[i] = g.parent[g.parent[i]]
		i = g.parent[i]
	}
	return i
}

func (g *assGrouper) add(ev *assEvent) []vttCue {
	var out []vttCue
	if len(g.pending) > 0 && ev.start > g.lastStart {
		out = g.flush(ev.start, false)
	}
	if ev.start > g.lastStart {
		g.lastStart = ev.start
	}
	n := len(g.pending)
	g.pending = append(g.pending, ev)
	g.boxes = append(g.boxes, g.box(ev))
	g.parent = append(g.parent, n)
	for j := 0; j < n; j++ {
		if g.pending[j].end <= ev.start || g.find(j) == g.find(n) {
			continue
		}
		if linkASSEvents(g.pending[j], ev, g.boxes[j], g.boxes[n]) {
			g.parent[g.find(n)] = g.find(j)
		}
	}
	return out
}

func (g *assGrouper) flushAll() []vttCue {
	return g.flush(0, true)
}

func (g *assGrouper) flush(before int64, all bool) []vttCue {
	if len(g.pending) == 0 {
		return nil
	}
	maxEnd := map[int]int64{}
	for i, ev := range g.pending {
		r := g.find(i)
		if ev.end > maxEnd[r] {
			maxEnd[r] = ev.end
		}
	}
	if !all {
		ready := false
		for _, e := range maxEnd {
			if e <= before {
				ready = true
				break
			}
		}
		if !ready {
			return nil
		}
	}

	groups := map[int][]*assEvent{}
	var order []int
	var keepIdx []int
	for i, ev := range g.pending {
		r := g.find(i)
		if all || maxEnd[r] <= before {
			if _, ok := groups[r]; !ok {
				order = append(order, r)
			}
			groups[r] = append(groups[r], ev)
			continue
		}
		keepIdx = append(keepIdx, i)
	}
	var out []vttCue
	for _, r := range order {
		out = append(out, g.render(groups[r])...)
	}

	// Keep the unfinished groups, remapping union-find roots to the new indices.
	newIndex := make(map[int]int, len(keepIdx))
	for ni, oi := range keepIdx {
		newIndex[oi] = ni
	}
	pending := make([]*assEvent, len(keepIdx))
	boxes := make([]assBox, len(keepIdx))
	parent := make([]int, len(keepIdx))
	for ni, oi := range keepIdx {
		pending[ni] = g.pending[oi]
		boxes[ni] = g.boxes[oi]
		parent[ni] = newIndex[g.find(oi)]
	}
	g.pending, g.boxes, g.parent = pending, boxes, parent
	return out
}

// assBox is an event's approximate on-screen box in script (PlayRes) coordinates.
type assBox struct {
	left, top, right, bottom float64
}

func (b assBox) cx() float64 { return (b.left + b.right) / 2 }
func (b assBox) cy() float64 { return (b.top + b.bottom) / 2 }

// textUnits approximates rendered width in ems (font-size units) with a coarse per-glyph table
// for proportional Latin fonts; CJK/full-width glyphs are 1em. Word gaps in per-character
// karaoke are only a few pixels wider than a wide letter, so a flat "average glyph" width reads
// every `w`/`m` as a space.
func textUnits(s string) float64 {
	var n float64
	for _, r := range s {
		n += glyphEm(r)
	}
	return n
}

func glyphEm(r rune) float64 {
	switch {
	case isWideRune(r):
		return 1
	case r == ' ':
		return 0.28
	case strings.ContainsRune("ijl!',.:;|`’‘", r):
		return 0.28
	case strings.ContainsRune("ftrI()[]-\"/“”", r):
		return 0.36
	case r == 'm' || r == 'w':
		return 0.78
	case r == 'M' || r == 'W':
		return 0.92
	case r == 'J':
		return 0.45
	case r >= 'A' && r <= 'Z':
		return 0.68
	case r >= '0' && r <= '9':
		return 0.55
	case r >= 'a' && r <= 'z':
		return 0.52
	}
	return 0.55
}

func isWideRune(r rune) bool {
	return unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) ||
		unicode.Is(unicode.Hangul, r) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFF60)
}

// anchorFractions returns the horizontal/vertical anchor fractions for an ASS numpad alignment.
func anchorFractions(alignment int) (float64, float64) {
	ax := 0.5
	switch alignment {
	case 1, 4, 7:
		ax = 0
	case 3, 6, 9:
		ax = 1
	}
	ay := 1.0
	switch alignment {
	case 7, 8, 9:
		ay = 0
	case 4, 5, 6:
		ay = 0.5
	}
	return ax, ay
}

func (g *assGrouper) box(ev *assEvent) assBox {
	if !ev.positioned {
		return assBox{}
	}
	var w float64
	for _, l := range ev.lines {
		w = math.Max(w, textUnits(runsPlain(l))*ev.fontSize)
	}
	if ev.space {
		w = textUnits(" ") * ev.fontSize
	}
	h := float64(lineCount(ev)) * ev.fontSize
	ax, ay := anchorFractions(ev.alignment)
	left := ev.x - ax*w
	top := ev.y - ay*h
	return assBox{left: left, top: top, right: left + w, bottom: top + h}
}

func lineCount(ev *assEvent) int {
	return max(1, len(ev.lines))
}

func timeOverlapMajority(a, b *assEvent) bool {
	ov := min(a.end, b.end) - max(a.start, b.start)
	if ov <= 0 {
		return false
	}
	shorter := min(a.end-a.start, b.end-b.start)
	return ov*2 >= shorter
}

// linkASSEvents reports whether two events belong in the same cue.
//
// Positioned events only need to be on screen together at some instant: the group is later cut
// into time slices (see renderPositioned), so linking a whole karaoke chain is harmless.
func linkASSEvents(a, b *assEvent, ba, bb assBox) bool {
	if a.positioned != b.positioned {
		return false
	}
	if !a.positioned && !timeOverlapMajority(a, b) {
		return false
	}
	if min(a.end, b.end) <= max(a.start, b.start) {
		return false
	}
	if !a.positioned {
		// Duplicate layers (border/blur/highlight copies) of the same unpositioned line.
		return a.plain == b.plain && a.alignment == b.alignment
	}
	fs := math.Max(a.fontSize, b.fontSize)
	if a.plain == b.plain && math.Abs(ba.cx()-bb.cx()) <= fs*0.15 && math.Abs(ba.cy()-bb.cy()) <= fs*0.15 {
		return true
	}
	if a.space || b.space {
		// A spacing marker only joins the row it sits in.
		return math.Abs(ba.cy()-bb.cy()) <= fs*0.5 && math.Max(bb.left-ba.right, ba.left-bb.right) <= fs*1.5
	}
	hgap := math.Max(bb.left-ba.right, ba.left-bb.right)
	// Same row, e.g. karaoke syllables or words typeset as separate events.
	if lineCount(a) == 1 && lineCount(b) == 1 && math.Abs(ba.cy()-bb.cy()) <= fs*0.5 {
		return hgap <= fs*1.5
	}
	// Stacked rows of one sign, e.g. "Delicious" above "Gyokuro Tea".
	vgap := math.Max(bb.top-ba.bottom, ba.top-bb.bottom)
	return vgap <= fs*0.6 && hgap <= fs
}

type assPiece struct {
	space bool
	ev    *assEvent
	runs  []assRun
	plain string
	units float64
	left  float64
	cx    float64
	cy    float64
	fs    float64
}

// render turns one group of events into cues: one cue for unpositioned duplicates, time-sliced
// cues for positioned groups.
func (g *assGrouper) render(grp []*assEvent) []vttCue {
	if grp[0].positioned {
		return g.renderPositioned(grp)
	}
	cue := vttCue{seq: grp[0].seq, start: grp[0].start, end: grp[0].end}
	for _, ev := range grp {
		cue.start = min(cue.start, ev.start)
		cue.end = max(cue.end, ev.end)
		cue.seq = min(cue.seq, ev.seq)
	}
	formatting := g.p.opts.formatting
	{
		// All members are duplicates of one line: keep the earliest.
		first := grp[0]
		for _, ev := range grp {
			if ev.seq < first.seq {
				first = ev
			}
		}
		lines := make([]string, 0, len(first.lines))
		for _, l := range first.lines {
			lines = append(lines, renderRuns(l, formatting))
		}
		cue.text = strings.Join(lines, "\n")
		cue.settings = unpositionedSettings(first.alignment)
		if cue.text == "" {
			return nil
		}
		return []vttCue{cue}
	}
}

// minStableSliceMS is the shortest time a positioned cue's text must stay unchanged to get a cue
// of its own. Shorter slices are character-by-character reveals or the hand-over between two
// lyric lines and are folded into the line that follows them.
const minStableSliceMS = 350

// renderPositioned cuts a positioned group at every event boundary, lays out the events visible in
// each slice (karaoke templates swap per-character, highlight and base events for the same
// syllable over time, so a union over the whole group would show them all at once), then merges
// slices that read the same and folds short transitional slices into the next line.
func (g *assGrouper) renderPositioned(grp []*assEvent) []vttCue {
	cuts := make([]int64, 0, 2*len(grp))
	for _, ev := range grp {
		cuts = append(cuts, ev.start, ev.end)
	}
	sort.Slice(cuts, func(i, j int) bool { return cuts[i] < cuts[j] })
	seq := grp[0].seq
	for _, ev := range grp {
		seq = min(seq, ev.seq)
	}

	g.persist = persistenceEnds(grp)
	defer func() { g.persist = nil }()

	var slices []vttCue
	var active []*assEvent
	type scoredText struct {
		text  string
		score int
	}
	spacing := map[string]scoredText{}
	for k := 0; k+1 < len(cuts); k++ {
		from, to := cuts[k], cuts[k+1]
		if to <= from {
			continue
		}
		active = active[:0]
		for _, ev := range grp {
			if ev.start <= from && ev.end >= to {
				active = append(active, ev)
			}
		}
		text, settings, marked := g.layoutPositioned(active)
		if text == "" {
			continue
		}
		// The same line is laid out from different pieces over time (characters, then
		// syllables). Spacing is most reliable from the source's word-gap events, then from the
		// finest-grained layout (per-character gaps are easier to measure), so every slice of a
		// line reuses the best one.
		score := len(active)
		if marked {
			score += 1 << 20
		}
		key := strings.ReplaceAll(text, " ", "")
		if prev, ok := spacing[key]; !ok || score > prev.score {
			spacing[key] = scoredText{text: text, score: score}
		}
		if n := len(slices); n > 0 && slices[n-1].end == from && slices[n-1].text == text {
			slices[n-1].end = to
			continue
		}
		slices = append(slices, vttCue{seq: seq, start: from, end: to, text: text, settings: settings})
	}

	for i := range slices {
		if best, ok := spacing[strings.ReplaceAll(slices[i].text, " ", "")]; ok {
			slices[i].text = best.text
		}
	}
	// Re-merge slices that now read the same.
	merged := slices[:0]
	for _, c := range slices {
		if n := len(merged); n > 0 && merged[n-1].end == c.start && merged[n-1].text == c.text {
			merged[n-1].end = c.end
			continue
		}
		merged = append(merged, c)
	}
	slices = merged

	// 1. Fold short slices (per-character reveal steps, line hand-overs) into the next slice.
	var stable []vttCue
	for i := 0; i < len(slices); i++ {
		c := slices[i]
		if c.end-c.start < minStableSliceMS {
			if i+1 < len(slices) && slices[i+1].start == c.end {
				slices[i+1].start = c.start
				continue
			}
			if n := len(stable); n > 0 && stable[n-1].end == c.start {
				stable[n-1].end = c.end
				continue
			}
		}
		stable = append(stable, c)
	}
	// 2. A line still being revealed shows as the finished line from its first step.
	var out []vttCue
	for i := 0; i < len(stable); i++ {
		c := stable[i]
		if i+1 < len(stable) && stable[i+1].start == c.end && revealsInto(c.text, stable[i+1].text) {
			stable[i+1].start = c.start
			continue
		}
		if n := len(out); n > 0 && out[n-1].end == c.start && out[n-1].text == c.text {
			out[n-1].end = c.end
			continue
		}
		out = append(out, c)
	}
	return out
}

// persistenceEnds returns, per event, when its text leaves its spot: the end of the chain of
// same-text events at the same position that follow it without a gap.
func persistenceEnds(grp []*assEvent) map[*assEvent]int64 {
	byText := map[string][]*assEvent{}
	for _, ev := range grp {
		byText[ev.plain] = append(byText[ev.plain], ev)
	}
	out := make(map[*assEvent]int64, len(grp))
	for _, evs := range byText {
		sort.Slice(evs, func(i, j int) bool { return evs[i].start < evs[j].start })
		for i, ev := range evs {
			end := ev.end
			tol := ev.fontSize * 0.15
			for _, nx := range evs[i+1:] {
				if nx.start > end+50 {
					break
				}
				if math.Abs(nx.x-ev.x) <= tol && math.Abs(nx.y-ev.y) <= tol && nx.end > end {
					end = nx.end
				}
			}
			out[ev] = end
		}
	}
	return out
}

// dropCrossfades removes an outgoing line that is still fading out where the next line fades in:
// a piece is covered when a newer piece with different text sits on the same spot of the same row,
// and the rest of the outgoing line (pieces ending together with a covered piece) goes with it.
// Without this the two lyric lines read as one.
func dropCrossfades(pieces []assPiece, persist map[*assEvent]int64) []assPiece {
	stays := func(pc assPiece) int64 {
		if e, ok := persist[pc.ev]; ok {
			return e
		}
		return pc.ev.end
	}
	covered := make([]bool, len(pieces))
	var outgoingEnds []int64
	for i, a := range pieces {
		wa := a.units * a.fs
		for j, b := range pieces {
			// The piece that stays on screen longer belongs to the incoming line.
			if i == j || a.space || b.space || a.plain == b.plain || stays(b) <= stays(a) {
				continue
			}
			fs := math.Max(a.fs, b.fs)
			if math.Abs(a.cy-b.cy) > fs*0.5 {
				continue
			}
			wb := b.units * b.fs
			narrow := math.Min(wa, wb)
			overlap := math.Min(a.left+wa, b.left+wb) - math.Max(a.left, b.left)
			if overlap > 0.5*narrow && math.Abs(a.cx-b.cx) < 0.5*narrow+fs*0.1 {
				covered[i] = true
				outgoingEnds = append(outgoingEnds, stays(a))
				break
			}
		}
	}
	if len(outgoingEnds) == 0 {
		return pieces
	}
	out := pieces[:0:0]
	for i, pc := range pieces {
		if covered[i] {
			continue
		}
		outgoing := false
		for _, e := range outgoingEnds {
			if d := stays(pc) - e; d > -60 && d < 60 {
				outgoing = true
				break
			}
		}
		if !outgoing {
			out = append(out, pc)
		}
	}
	return out
}

// revealsInto reports whether cue text a is an incomplete version of b: each line of a is an
// in-order subset of the characters of a distinct line of b, and b is longer (a karaoke line
// appearing character by character, possibly with a syllable mid-swap between template layers).
func revealsInto(a, b string) bool {
	pa := strings.Split(reVTTMarkup.ReplaceAllString(a, ""), "\n")
	pb := strings.Split(reVTTMarkup.ReplaceAllString(b, ""), "\n")
	if len(pa) > len(pb) || len(a) >= len(b) {
		return false
	}
	used := make([]bool, len(pb))
	for _, la := range pa {
		found := false
		for j, lb := range pb {
			if !used[j] && isSubsequence(strings.ReplaceAll(la, " ", ""), lb) {
				used[j], found = true, true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isSubsequence(a, b string) bool {
	ra := []rune(a)
	i := 0
	for _, r := range b {
		if i < len(ra) && ra[i] == r {
			i++
		}
	}
	return i == len(ra)
}

// layoutPositioned lays out positioned events shown together as one cue: rows top to bottom,
// pieces left to right, duplicate layers dropped.
//
// marked reports whether a row's spaces came from whitespace events rather than estimation.
func (g *assGrouper) layoutPositioned(grp []*assEvent) (text, settings string, marked bool) {
	formatting := g.p.opts.formatting

	// Split every event into per-line pieces laid out the way libass would.
	var pieces []assPiece
	for _, ev := range grp {
		ax, ay := anchorFractions(ev.alignment)
		top := ev.y - ay*float64(lineCount(ev))*ev.fontSize
		if ev.space {
			units := textUnits(" ")
			left := ev.x - ax*units*ev.fontSize
			pieces = append(pieces, assPiece{
				space: true, ev: ev, plain: " ", units: units,
				left: left, cx: left + units*ev.fontSize/2, cy: top + ev.fontSize/2, fs: ev.fontSize,
			})
			continue
		}
		for li, l := range ev.lines {
			plain := runsPlain(l)
			units := textUnits(plain)
			w := units * ev.fontSize
			left := ev.x - ax*w
			pieces = append(pieces, assPiece{
				ev: ev, runs: l, plain: plain, units: units,
				left: left, cx: left + w/2, cy: top + (float64(li)+0.5)*ev.fontSize, fs: ev.fontSize,
			})
		}
	}
	// Longest-lived copy wins when layers duplicate a piece.
	sort.SliceStable(pieces, func(i, j int) bool {
		di := pieces[i].ev.end - pieces[i].ev.start
		dj := pieces[j].ev.end - pieces[j].ev.start
		if di != dj {
			return di > dj
		}
		return pieces[i].ev.seq < pieces[j].ev.seq
	})
	var uniq []assPiece
	for _, pc := range pieces {
		dup := false
		for _, u := range uniq {
			// Layer copies sit on the same point; a repeated letter ("door") is a glyph apart.
			tol := math.Max(pc.fs, u.fs) * 0.15
			if u.plain == pc.plain && math.Abs(u.cx-pc.cx) <= tol && math.Abs(u.cy-pc.cy) <= tol {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, pc)
		}
	}

	uniq = dropCrossfades(uniq, g.persist)

	// Rows top to bottom, pieces left to right.
	sort.SliceStable(uniq, func(i, j int) bool { return uniq[i].cy < uniq[j].cy })
	var rows [][]assPiece
	for _, pc := range uniq {
		if n := len(rows); n > 0 && math.Abs(rows[n-1][0].cy-pc.cy) <= pc.fs*0.5 {
			rows[n-1] = append(rows[n-1], pc)
			continue
		}
		rows = append(rows, []assPiece{pc})
	}
	minX, maxX := math.Inf(1), math.Inf(-1)
	minY, maxY := math.Inf(1), math.Inf(-1)
	var lines []string
	for _, row := range rows {
		sort.SliceStable(row, func(i, j int) bool { return row[i].cx < row[j].cx })
		line, rowMarked := joinRow(row, formatting)
		if line == "" {
			continue
		}
		marked = marked || rowMarked
		lines = append(lines, line)
		for _, pc := range row {
			w := pc.units * pc.fs
			minX = math.Min(minX, pc.left)
			maxX = math.Max(maxX, pc.left+w)
			minY = math.Min(minY, pc.cy-pc.fs/2)
			maxY = math.Max(maxY, pc.cy+pc.fs/2)
		}
	}
	if len(lines) == 0 {
		return "", "", false
	}
	resX, resY := g.p.resolution()
	return strings.Join(lines, "\n"), positionedSettings((minX+maxX)/2/resX, (minY+maxY)/2/resY), marked
}

// joinRow concatenates the pieces of one row left to right. When the row carries whitespace
// events (karaoke templates emit word gaps as their own events) those are the spaces, exactly as
// in the source. Otherwise a space is inserted where the gap between two neighbours is wider than
// their estimated glyph widths allow (see textUnits); the em size is calibrated from the row
// itself (lower quartile, since most neighbours sit inside a word) within ±30% of the font size.
func joinRow(row []assPiece, formatting bool) (string, bool) {
	// Runs are concatenated and rendered once so formatting tags do not depend on how the row
	// happens to be split into pieces (characters vs syllables) at a given moment.
	var runs []assRun
	written := false
	space := func() { runs = append(runs, assRun{text: " ", f: runs[len(runs)-1].f}) }
	markers := false
	for _, pc := range row {
		markers = markers || pc.space
	}
	if markers {
		pending := false
		for _, pc := range row {
			if pc.space {
				pending = written
				continue
			}
			if pending && !strings.HasPrefix(pc.plain, " ") {
				space()
			}
			pending = false
			runs = append(runs, pc.runs...)
			written = true
		}
		return renderRuns(runs, formatting), true
	}

	// Anchor-to-anchor distance between neighbours = em * expected + gap, where expected
	// depends on which point of each piece the alignment anchors.
	sameAlign := true
	for _, pc := range row {
		sameAlign = sameAlign && pc.ev.alignment == row[0].ev.alignment
	}
	ax := 0.5
	if sameAlign {
		ax, _ = anchorFractions(row[0].ev.alignment)
	}
	anchorX := func(pc assPiece) float64 {
		if sameAlign {
			return pc.ev.x
		}
		return pc.cx
	}
	expected := func(i int) float64 {
		return (1-ax)*row[i].units + ax*row[i+1].units
	}
	fs := row[0].fs
	em := fs
	var implied []float64
	for i := 0; i+1 < len(row); i++ {
		if e := expected(i); e > 0 {
			implied = append(implied, (anchorX(row[i+1])-anchorX(row[i]))/e)
		}
	}
	if len(implied) >= 3 {
		sort.Float64s(implied)
		em = implied[len(implied)/4]
	}
	em = math.Min(math.Max(em, fs*0.7), fs*1.3)

	for i, pc := range row {
		if i > 0 {
			prev := row[i-1]
			// Width estimates err in proportion to the glyphs involved, so wider pairs need a
			// slightly wider gap (tuned on generated karaoke rows with ±18% glyph-width error).
			gap := (anchorX(pc) - anchorX(prev)) - em*expected(i-1)
			if gap > em*(0.12+0.04*expected(i-1)) && !strings.HasSuffix(prev.plain, " ") && !strings.HasPrefix(pc.plain, " ") {
				space()
			}
		}
		runs = append(runs, pc.runs...)
	}
	return renderRuns(runs, formatting), false
}

func clampPct(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return (lo + hi) / 2
	}
	return math.Min(math.Max(v, lo), hi)
}

// positionedSettings centres the cue on the group's box, clamped so it stays on screen.
func positionedSettings(fx, fy float64) string {
	x := clampPct(fx*100, 10, 90)
	y := clampPct(fy*100, 8, 92)
	return fmt.Sprintf("line:%s%%,center position:%s%%,center align:center", fmtPct(y), fmtPct(x))
}

func unpositionedSettings(alignment int) string {
	switch alignment {
	case 7, 8, 9:
		return "line:6%,start align:center"
	case 4, 5, 6:
		return "line:50%,center align:center"
	}
	return ""
}

func fmtPct(v float64) string {
	return strconv.FormatFloat(math.Round(v*10)/10, 'f', -1, 64)
}
