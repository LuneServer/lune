package library

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SubtitleTrack represents a verified local sidecar subtitle file (.srt, .vtt, .ass, or .ssa).
type SubtitleTrack struct {
	Index    int    `json:"index"`
	Language string `json:"language"`
	Label    string `json:"label"`
	Format   string `json:"format"` // "srt" | "vtt" | "ass" | "ssa"
	Forced   bool   `json:"forced,omitempty"`
	SDH      bool   `json:"sdh,omitempty"`
	Path     string `json:"path"`
}

// DiscoverSubtitles finds sidecar .srt, .vtt, .ass, and .ssa files in the same directory matching mediaPath's stem.
func DiscoverSubtitles(mediaPath string) []SubtitleTrack {
	dir := filepath.Dir(mediaPath)
	stem := strings.TrimSuffix(filepath.Base(mediaPath), filepath.Ext(mediaPath))
	stemLower := strings.ToLower(stem)

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	var tracks []SubtitleTrack
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		ext := strings.ToLower(filepath.Ext(name))
		if ext != ".srt" && ext != ".vtt" && ext != ".ass" && ext != ".ssa" {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		baseLower := strings.ToLower(base)
		if baseLower != stemLower && !strings.HasPrefix(baseLower, stemLower+".") && !strings.HasPrefix(baseLower, stemLower+"_") {
			continue
		}

		suffix := strings.TrimPrefix(base[len(stem):], ".")
		suffix = strings.TrimPrefix(suffix, "_")
		lang, forced, sdh, signs := parseSubtitleSuffix(suffix)
		label := formatSubtitleLabel(lang, forced, sdh, signs)

		tracks = append(tracks, SubtitleTrack{
			Language: lang,
			Label:    label,
			Format:   strings.TrimPrefix(ext, "."),
			Forced:   forced || signs,
			SDH:      sdh,
			Path:     filepath.Join(dir, name),
		})
	}

	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].Language != tracks[j].Language {
			return tracks[i].Language < tracks[j].Language
		}
		return tracks[i].Label < tracks[j].Label
	})
	for i := range tracks {
		tracks[i].Index = i
	}
	return tracks
}

func parseSubtitleSuffix(suffix string) (lang string, forced, sdh, signs bool) {
	lang = "und"
	if suffix == "" {
		return lang, false, false, false
	}
	parts := strings.FieldsFunc(strings.ToLower(suffix), func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	for _, p := range parts {
		switch p {
		case "forced":
			forced = true
		case "signs", "songs":
			signs = true
		case "sdh", "cc", "hi":
			sdh = true
		case "default", "full", "dialogue", "honorifics":
			// recognized anime subtitle flags
		default:
			if lang == "und" && (len(p) == 2 || len(p) == 3) {
				lang = normalizeLang(p)
			}
		}
	}
	return lang, forced, sdh, signs
}

func normalizeLang(code string) string {
	switch code {
	case "eng":
		return "en"
	case "fre", "fra":
		return "fr"
	case "spa":
		return "es"
	case "ger", "deu":
		return "de"
	case "ita":
		return "it"
	case "jpn":
		return "ja"
	case "kor":
		return "ko"
	case "por":
		return "pt"
	case "zho", "chi":
		return "zh"
	default:
		return code
	}
}

func formatSubtitleLabel(lang string, forced, sdh, signs bool) string {
	names := map[string]string{
		"en": "English", "fr": "French", "es": "Spanish", "de": "German",
		"it": "Italian", "ja": "Japanese", "ko": "Korean", "pt": "Portuguese",
		"zh": "Chinese", "und": "Subtitles",
	}
	label := names[lang]
	if label == "" {
		label = strings.ToUpper(lang)
	}
	if signs {
		label += " (Signs & Songs)"
	} else if forced {
		label += " (Forced)"
	}
	if sdh {
		label += " (SDH)"
	}
	return label
}
