package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ErrProberUnavailable indicates ffprobe is not present on PATH.
var ErrProberUnavailable = errors.New("ffprobe binary not available")

// StreamInfo describes an individual video, audio, or subtitle stream in a media file.
type StreamInfo struct {
	Index          int    `json:"index"`
	Type           string `json:"type"` // "video" | "audio" | "subtitle"
	Codec          string `json:"codec"`
	Profile        string `json:"profile,omitempty"`
	PixFmt         string `json:"pix_fmt,omitempty"`
	CodecTag       string `json:"codec_tag,omitempty"`
	ColorSpace     string `json:"color_space,omitempty"`
	ColorTransfer  string `json:"color_transfer,omitempty"`
	ColorPrimaries string `json:"color_primaries,omitempty"`
	DVProfile      int    `json:"dv_profile,omitempty"`
	DVCompatID     int    `json:"dv_compat_id,omitempty"`
	Language       string `json:"language,omitempty"`
	Title          string `json:"title,omitempty"`
	Width          int    `json:"width,omitempty"`
	Height         int    `json:"height,omitempty"`
	HDR            string `json:"hdr,omitempty"`
	Channels       int    `json:"channels,omitempty"`
	ChannelLayout  string `json:"channel_layout,omitempty"`
	Default        bool   `json:"default,omitempty"`
	Forced         bool   `json:"forced,omitempty"`
}

// ChapterInfo describes a container chapter marker (commonly used in anime MKVs for OP/ED/Recap).
type ChapterInfo struct {
	Index        int    `json:"index"`
	Title        string `json:"title,omitempty"`
	StartMS      int64  `json:"start_ms"`
	EndMS        int64  `json:"end_ms"`
	Kind         string `json:"kind"` // "intro" | "outro" | "recap" | "preview" | "chapter"
	ThumbnailURL string `json:"thumbnail_url,omitempty"`
}

// PopulateChapterThumbnails inspects <dataDir>/artwork/chapters/<itemID> with a single ReadDir
// and populates ThumbnailURL on chapters whose <index>.jpg still image exists on disk.
func PopulateChapterThumbnails(dataDir, itemID string, chapters []ChapterInfo) {
	if dataDir == "" || itemID == "" || len(chapters) == 0 {
		return
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "artwork", "chapters", itemID))
	if err != nil || len(entries) == 0 {
		return
	}
	available := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		available[entry.Name()] = struct{}{}
	}
	for i := range chapters {
		if chapters[i].Index < 0 {
			continue
		}
		name := strconv.Itoa(chapters[i].Index) + ".jpg"
		if _, ok := available[name]; ok {
			chapters[i].ThumbnailURL = fmt.Sprintf("/api/v1/items/%s/artwork/chapters/%d", itemID, chapters[i].Index)
		}
	}
}

// ProbeResult summarizes container-level and stream-level technical metadata.
type ProbeResult struct {
	Container     string
	DurationMS    int64
	Bitrate       int64
	VideoCodec    string
	VideoWidth    int
	VideoHeight   int
	VideoHDR      string
	AudioCodec    string
	AudioChannels int
	Streams       []StreamInfo
	Chapters      []ChapterInfo
}

// Prober extracts technical metadata from a media file on disk.
type Prober interface {
	Probe(ctx context.Context, path string) (ProbeResult, error)
}

// FFProbe executes the system ffprobe binary to inspect media containers.
type FFProbe struct {
	Binary  string
	Timeout time.Duration
}

// NewFFProbe locates ffprobe on PATH and configures a bounded inspection timeout.
func NewFFProbe() *FFProbe {
	bin, _ := exec.LookPath("ffprobe")
	return &FFProbe{
		Binary:  bin,
		Timeout: 15 * time.Second,
	}
}

// Available reports whether ffprobe was found on the host.
func (p *FFProbe) Available() bool {
	return p != nil && p.Binary != ""
}

// Probe runs ffprobe on path and parses its JSON stream/format metadata.
func (p *FFProbe) Probe(ctx context.Context, path string) (ProbeResult, error) {
	if !p.Available() {
		return ProbeResult{
			Container: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
		}, ErrProberUnavailable
	}
	if !filepath.IsAbs(path) {
		return ProbeResult{}, fmt.Errorf("ffprobe requires an absolute file path")
	}

	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(pctx, p.Binary,
		"-v", "quiet",
		"-print_format", "json",
		"-show_format",
		"-show_streams",
		"-show_chapters",
		"--",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return ProbeResult{}, fmt.Errorf("ffprobe %s: %w", filepath.Base(path), err)
	}
	return parseFFProbeJSON(out, path)
}

type ffprobeOutput struct {
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		BitRate    string `json:"bit_rate"`
	} `json:"format"`
	Streams []struct {
		Index          int    `json:"index"`
		CodecType      string `json:"codec_type"`
		CodecName      string `json:"codec_name"`
		CodecTagString string `json:"codec_tag_string"`
		Profile        string `json:"profile"`
		PixFmt         string `json:"pix_fmt"`
		Width          int    `json:"width"`
		Height         int    `json:"height"`
		ColorSpace     string `json:"color_space"`
		ColorTransfer  string `json:"color_transfer"`
		ColorPrimaries string `json:"color_primaries"`
		Channels       int    `json:"channels"`
		ChannelLayout  string `json:"channel_layout"`
		Disposition    struct {
			Default     int `json:"default"`
			Forced      int `json:"forced"`
			AttachedPic int `json:"attached_pic"`
		} `json:"disposition"`
		Tags         map[string]string `json:"tags"`
		SideDataList []ffprobeSideData `json:"side_data_list"`
	} `json:"streams"`
	Chapters []struct {
		ID        int               `json:"id"`
		StartTime string            `json:"start_time"`
		EndTime   string            `json:"end_time"`
		Tags      map[string]string `json:"tags"`
	} `json:"chapters"`
}

func parseFFProbeJSON(raw []byte, path string) (ProbeResult, error) {
	var parsed ffprobeOutput
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ProbeResult{}, fmt.Errorf("decode ffprobe json: %w", err)
	}

	res := ProbeResult{
		Container: strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
	}
	if res.Container == "" && parsed.Format.FormatName != "" {
		res.Container = strings.Split(parsed.Format.FormatName, ",")[0]
	}
	if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil && d > 0 {
		res.DurationMS = int64(d * 1000)
	}
	if b, err := strconv.ParseInt(parsed.Format.BitRate, 10, 64); err == nil && b > 0 {
		res.Bitrate = b
	}

	for _, st := range parsed.Streams {
		if st.Disposition.AttachedPic == 1 {
			continue
		}
		if st.CodecType != "video" && st.CodecType != "audio" && st.CodecType != "subtitle" {
			continue
		}

		info := StreamInfo{
			Index:          st.Index,
			Type:           st.CodecType,
			Codec:          strings.ToLower(st.CodecName),
			Profile:        st.Profile,
			PixFmt:         strings.ToLower(strings.TrimSpace(st.PixFmt)),
			CodecTag:       strings.TrimSpace(st.CodecTagString),
			ColorSpace:     strings.ToLower(strings.TrimSpace(st.ColorSpace)),
			ColorTransfer:  strings.ToLower(strings.TrimSpace(st.ColorTransfer)),
			ColorPrimaries: strings.ToLower(strings.TrimSpace(st.ColorPrimaries)),
			Language:       tagCI(st.Tags, "language"),
			Title:          tagCI(st.Tags, "title"),
			Width:          st.Width,
			Height:         st.Height,
			Channels:       st.Channels,
			ChannelLayout:  st.ChannelLayout,
			Default:        st.Disposition.Default == 1,
			Forced:         st.Disposition.Forced == 1,
		}

		if st.CodecType == "video" {
			info.HDR, info.DVProfile, info.DVCompatID = detectHDR(st.ColorTransfer, st.ColorPrimaries, st.SideDataList)
			if res.VideoCodec == "" {
				res.VideoCodec = info.Codec
				res.VideoWidth = info.Width
				res.VideoHeight = info.Height
				res.VideoHDR = info.HDR
			}
		}
		if st.CodecType == "audio" && res.AudioCodec == "" {
			res.AudioCodec = info.Codec
			res.AudioChannels = info.Channels
		}

		res.Streams = append(res.Streams, info)
	}

	for i, ch := range parsed.Chapters {
		startSec, err1 := strconv.ParseFloat(ch.StartTime, 64)
		endSec, err2 := strconv.ParseFloat(ch.EndTime, 64)
		if err1 != nil || err2 != nil || endSec <= startSec {
			continue
		}
		title := tagCI(ch.Tags, "title")
		res.Chapters = append(res.Chapters, ChapterInfo{
			Index:   i,
			Title:   title,
			StartMS: int64(startSec * 1000),
			EndMS:   int64(endSec * 1000),
			Kind:    ClassifyChapterKind(title),
		})
	}
	return res, nil
}

// ClassifyChapterKind categorizes an MKV chapter title into intro, outro, recap, preview, or chapter.
func ClassifyChapterKind(title string) string {
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "" {
		return "chapter"
	}
	// Strip surrounding brackets or punctuation like "[OP]", "OP 1", "Opening Theme".
	clean := strings.Trim(t, "[]()-_:. ")
	switch clean {
	case "op", "op1", "op2", "opening", "intro", "opening theme", "opening credits", "opening song", "op theme", "ncop", "title sequence":
		return "intro"
	case "ed", "ed1", "ed2", "ending", "outro", "ending theme", "ending credits", "end credits", "credits", "ending song", "ed theme", "nced":
		return "outro"
	case "recap", "previously", "previously on", "summary":
		return "recap"
	case "preview", "next episode", "next time", "epilogue preview", "next episode preview", "trailer":
		return "preview"
	}
	if strings.HasPrefix(clean, "opening ") || strings.HasPrefix(clean, "op ") || strings.HasSuffix(clean, " opening") || strings.Contains(clean, "intro") {
		return "intro"
	}
	if strings.HasPrefix(clean, "ending ") || strings.HasPrefix(clean, "ed ") || strings.HasSuffix(clean, " ending") || strings.Contains(clean, "credits") || strings.Contains(clean, "outro") {
		return "outro"
	}
	if strings.Contains(clean, "recap") || strings.HasPrefix(clean, "previously") {
		return "recap"
	}
	if strings.Contains(clean, "preview") || strings.Contains(clean, "next episode") {
		return "preview"
	}
	return "chapter"
}

// NormalizeLanguage normalizes common ISO-639-2/B/T language codes to ISO-639-1 two-letter codes.
func NormalizeLanguage(code string) string {
	c := strings.ToLower(strings.TrimSpace(code))
	if idx := strings.IndexAny(c, "-_"); idx > 0 {
		c = c[:idx]
	}
	switch c {
	case "eng", "en", "english":
		return "en"
	case "jpn", "ja", "jap", "japanese":
		return "ja"
	case "fre", "fra", "fr", "french":
		return "fr"
	case "spa", "es", "spanish":
		return "es"
	case "ger", "deu", "de", "german":
		return "de"
	case "ita", "it", "italian":
		return "it"
	case "kor", "ko", "korean":
		return "ko"
	case "por", "pt", "portuguese":
		return "pt"
	case "zho", "chi", "zh", "chinese":
		return "zh"
	case "rus", "ru", "russian":
		return "ru"
	case "ara", "ar", "arabic":
		return "ar"
	case "und", "unknown", "":
		return ""
	default:
		return c
	}
}

// ExtractAudioLanguages returns deduplicated, order-preserved normalized audio languages from probed streams.
func ExtractAudioLanguages(streams []StreamInfo) []string {
	seen := make(map[string]bool)
	var langs []string
	for _, st := range streams {
		if st.Type != "audio" {
			continue
		}
		lang := NormalizeLanguage(st.Language)
		if lang == "" {
			continue
		}
		if !seen[lang] {
			seen[lang] = true
			langs = append(langs, lang)
		}
	}
	return langs
}

type ffprobeSideData struct {
	SideDataType string `json:"side_data_type"`
	DVProfile    int    `json:"dv_profile"`
	DVCompatID   int    `json:"dv_bl_signal_compatibility_id"`
}

func detectHDR(transfer, primaries string, sideData []ffprobeSideData) (hdr string, dvProfile int, dvCompatID int) {
	for _, sd := range sideData {
		lower := strings.ToLower(sd.SideDataType)
		if strings.Contains(lower, "dovi") || strings.Contains(lower, "dolby vision") {
			if sd.DVProfile == 7 || (sd.DVProfile == 8 && (sd.DVCompatID == 1 || sd.DVCompatID == 6)) {
				return "dovi_hdr10", sd.DVProfile, sd.DVCompatID
			}
			return "dovi", sd.DVProfile, sd.DVCompatID
		}
	}
	switch strings.ToLower(transfer) {
	case "smpte2084":
		return "hdr10", 0, 0
	case "arib-std-b67":
		return "hlg", 0, 0
	default:
		if strings.ToLower(primaries) == "bt2020" {
			return "hdr", 0, 0
		}
		return "sdr", 0, 0
	}
}

func tagCI(tags map[string]string, key string) string {
	for k, v := range tags {
		if strings.EqualFold(k, key) {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
