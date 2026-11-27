package stream

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math"
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
			return p.parseDialogue(strings.TrimSpace(trimmed[len("dialogue:"):]))
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
	p.parseText(ev, rawText, st)
	if len(ev.lines) == 0 {
		return nil
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
func (p *assParser) parseText(ev *assEvent, raw string, st assStyle) {
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
// or after a group's end, nothing later can join it.
type assGrouper struct {
	p         *assParser
	pending   []*assEvent
	lastStart int64
}

func (g *assGrouper) add(ev *assEvent) []vttCue {
	var out []vttCue
	if len(g.pending) > 0 && ev.start > g.lastStart {
		out = g.flush(ev.start, false)
	}
	if ev.start > g.lastStart {
		g.lastStart = ev.start
	}
	g.pending = append(g.pending, ev)
	return out
}

func (g *assGrouper) flushAll() []vttCue {
	return g.flush(0, true)
}

func (g *assGrouper) flush(before int64, all bool) []vttCue {
	if len(g.pending) == 0 {
		return nil
	}
	groups := g.components(g.pending)
	var out []vttCue
	var keep []*assEvent
	for _, grp := range groups {
		var maxEnd int64
		for _, ev := range grp {
			if ev.end > maxEnd {
				maxEnd = ev.end
			}
		}
		if all || maxEnd <= before {
			if c, ok := g.render(grp); ok {
				out = append(out, c)
			}
			continue
		}
		keep = append(keep, grp...)
	}
	sort.SliceStable(keep, func(i, j int) bool { return keep[i].seq < keep[j].seq })
	g.pending = keep
	return out
}

// components partitions events into groups using union-find over the pairwise link rule.
func (g *assGrouper) components(evs []*assEvent) [][]*assEvent {
	parent := make([]int, len(evs))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	boxes := make([]assBox, len(evs))
	for i, ev := range evs {
		boxes[i] = g.box(ev)
	}
	for i := range evs {
		for j := i + 1; j < len(evs); j++ {
			if find(i) == find(j) {
				continue
			}
			if linkASSEvents(evs[i], evs[j], boxes[i], boxes[j]) {
				parent[find(j)] = find(i)
			}
		}
	}
	idx := map[int]int{}
	var out [][]*assEvent
	for i, ev := range evs {
		r := find(i)
		k, ok := idx[r]
		if !ok {
			k = len(out)
			idx[r] = k
			out = append(out, nil)
		}
		out[k] = append(out[k], ev)
	}
	return out
}

// assBox is an event's approximate on-screen box in script (PlayRes) coordinates.
type assBox struct {
	left, top, right, bottom float64
}

func (b assBox) cx() float64 { return (b.left + b.right) / 2 }
func (b assBox) cy() float64 { return (b.top + b.bottom) / 2 }

// textUnits approximates rendered width in "average glyph" units: CJK/full-width glyphs count 2.
func textUnits(s string) float64 {
	var n float64
	for _, r := range s {
		if isWideRune(r) {
			n += 2
		} else {
			n++
		}
	}
	return n
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
	cw := ev.fontSize * 0.5
	var w float64
	for _, l := range ev.lines {
		w = math.Max(w, textUnits(runsPlain(l))*cw)
	}
	h := float64(len(ev.lines)) * ev.fontSize
	ax, ay := anchorFractions(ev.alignment)
	left := ev.x - ax*w
	top := ev.y - ay*h
	return assBox{left: left, top: top, right: left + w, bottom: top + h}
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
func linkASSEvents(a, b *assEvent, ba, bb assBox) bool {
	if a.positioned != b.positioned || !timeOverlapMajority(a, b) {
		return false
	}
	if !a.positioned {
		// Duplicate layers (border/blur/highlight copies) of the same unpositioned line.
		return a.plain == b.plain && a.alignment == b.alignment
	}
	fs := math.Max(a.fontSize, b.fontSize)
	if a.plain == b.plain && math.Abs(ba.cx()-bb.cx()) <= fs && math.Abs(ba.cy()-bb.cy()) <= fs {
		return true
	}
	hgap := math.Max(bb.left-ba.right, ba.left-bb.right)
	// Same row, e.g. karaoke syllables or words typeset as separate events.
	if len(a.lines) == 1 && len(b.lines) == 1 && math.Abs(ba.cy()-bb.cy()) <= fs*0.5 {
		return hgap <= fs*1.5
	}
	// Stacked rows of one sign, e.g. "Delicious" above "Gyokuro Tea".
	vgap := math.Max(bb.top-ba.bottom, ba.top-bb.bottom)
	return vgap <= fs*0.6 && hgap <= fs
}

type assPiece struct {
	ev    *assEvent
	runs  []assRun
	plain string
	units float64
	left  float64
	cx    float64
	cy    float64
	fs    float64
}

// render turns one group of events into a single cue.
func (g *assGrouper) render(grp []*assEvent) (vttCue, bool) {
	cue := vttCue{seq: grp[0].seq, start: grp[0].start, end: grp[0].end}
	for _, ev := range grp {
		cue.start = min(cue.start, ev.start)
		cue.end = max(cue.end, ev.end)
		cue.seq = min(cue.seq, ev.seq)
	}
	formatting := g.p.opts.formatting

	if !grp[0].positioned {
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
		return cue, cue.text != ""
	}

	// Split every event into per-line pieces laid out the way libass would.
	var pieces []assPiece
	for _, ev := range grp {
		ax, ay := anchorFractions(ev.alignment)
		cw := ev.fontSize * 0.5
		top := ev.y - ay*float64(len(ev.lines))*ev.fontSize
		for li, l := range ev.lines {
			plain := runsPlain(l)
			units := textUnits(plain)
			w := units * cw
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
			tol := math.Max(pc.fs, u.fs)
			if u.plain == pc.plain && math.Abs(u.cx-pc.cx) <= tol && math.Abs(u.cy-pc.cy) <= tol*0.5 {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, pc)
		}
	}

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
		lines = append(lines, joinRow(row, formatting))
		for _, pc := range row {
			w := pc.units * pc.fs * 0.5
			minX = math.Min(minX, pc.left)
			maxX = math.Max(maxX, pc.left+w)
			minY = math.Min(minY, pc.cy-pc.fs/2)
			maxY = math.Max(maxY, pc.cy+pc.fs/2)
		}
	}
	cue.text = strings.Join(lines, "\n")
	resX, resY := g.p.resolution()
	cue.settings = positionedSettings((minX+maxX)/2/resX, (minY+maxY)/2/resY)
	return cue, cue.text != ""
}

// joinRow concatenates the pieces of one row, inserting a space where the gap between two
// pieces is wider than the gaps inside words. The glyph width is calibrated from the tightest
// pair (syllables of one word touch), capped at half the font size.
func joinRow(row []assPiece, formatting bool) string {
	cw := row[0].fs * 0.5
	for i := 0; i+1 < len(row); i++ {
		a, b := row[i], row[i+1]
		if u := (a.units + b.units) / 2; u > 0 {
			cw = math.Min(cw, (b.cx-a.cx)/u)
		}
	}
	cw = math.Max(cw, row[0].fs*0.2)
	var b strings.Builder
	for i, pc := range row {
		if i > 0 {
			prev := row[i-1]
			gap := (pc.cx - prev.cx) - cw*(prev.units+pc.units)/2
			if gap > pc.fs*0.25 && !strings.HasSuffix(prev.plain, " ") && !strings.HasPrefix(pc.plain, " ") {
				b.WriteByte(' ')
			}
		}
		b.WriteString(renderRuns(pc.runs, formatting))
	}
	return b.String()
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
