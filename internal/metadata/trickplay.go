package metadata

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// TrickplayManifestName is the manifest written beside an item's trickplay sprite sheets.
const TrickplayManifestName = "manifest.json"

// TrickplayDir returns <dataDir>/artwork/trickplay/<itemID>.
func TrickplayDir(dataDir, itemID string) string {
	return filepath.Join(dataDir, "artwork", "trickplay", itemID)
}

// TrickplaySheetPattern validates sheet file names served over HTTP (prevents traversal).
var TrickplaySheetPattern = regexp.MustCompile(`^\d{1,5}\.(avif|webp|jpg)$`)

// TrickplayContentType maps a validated sheet name to its MIME type.
func TrickplayContentType(name string) string {
	switch filepath.Ext(name) {
	case ".avif":
		return "image/avif"
	case ".webp":
		return "image/webp"
	default:
		return "image/jpeg"
	}
}

// TrickplayManifest describes an item's scrubbing preview sprite sheets. Each sheet is a
// Columns x Rows grid of ThumbWidth x ThumbHeight thumbnails in row-major order; thumbnail
// n covers media time [n*IntervalMS, (n+1)*IntervalMS) and lives on sheet n/(Columns*Rows).
type TrickplayManifest struct {
	Version         int    `json:"version"`
	Format          string `json:"format"` // sheet file extension: avif | webp | jpg
	IntervalMS      int64  `json:"interval_ms"`
	ThumbWidth      int    `json:"thumb_width"`
	ThumbHeight     int    `json:"thumb_height"`
	Columns         int    `json:"columns"`
	Rows            int    `json:"rows"`
	ThumbCount      int    `json:"thumb_count"`
	SheetCount      int    `json:"sheet_count"`
	SourceMtime     int64  `json:"source_mtime"` // media_items.file_mtime (ns) the sheets were built from
	KeyframesOnly   bool   `json:"keyframes_only"`
	ConfiguredWidth int    `json:"configured_width"` // trickplay_width setting the sheets were built with
	Crop            string `json:"crop,omitempty"`   // FFmpeg crop filter applied to remove black bars
	GeneratedAt     int64  `json:"generated_at"`     // unix seconds; used to cache-bust sheet URLs
	// TimestampsMS lists each tile's media time when near-duplicate tiles were dropped; when
	// absent, tile n is at n*IntervalMS.
	TimestampsMS []int64 `json:"timestamps_ms,omitempty"`
}

// TrickplayInfo is the client-facing trickplay description included in playback plans.
// Tile n is shown for media times from its timestamp (timestamps_ms[n] when present, else
// n*interval_ms) until the next tile's.
type TrickplayInfo struct {
	Format       string   `json:"format"`
	TimestampsMS []int64  `json:"timestamps_ms,omitempty"`
	IntervalMS   int64    `json:"interval_ms"`
	ThumbWidth   int      `json:"thumb_width"`
	ThumbHeight  int      `json:"thumb_height"`
	Columns      int      `json:"columns"`
	Rows         int      `json:"rows"`
	ThumbCount   int      `json:"thumb_count"`
	SheetURLs    []string `json:"sheet_urls"`
}

// ReadTrickplayManifest loads an item's manifest, or returns an error if absent/invalid.
func ReadTrickplayManifest(dataDir, itemID string) (TrickplayManifest, error) {
	var m TrickplayManifest
	raw, err := os.ReadFile(filepath.Join(TrickplayDir(dataDir, itemID), TrickplayManifestName))
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return m, err
	}
	if m.IntervalMS <= 0 || m.Columns <= 0 || m.Rows <= 0 || m.ThumbCount <= 0 || m.SheetCount <= 0 || m.ThumbWidth <= 0 || m.ThumbHeight <= 0 {
		return m, fmt.Errorf("invalid trickplay manifest")
	}
	return m, nil
}

// LoadTrickplay returns client-facing trickplay info for an item, or nil when none exists.
// It costs a single small file read and never triggers generation.
func LoadTrickplay(dataDir, itemID string) *TrickplayInfo {
	if dataDir == "" || itemID == "" {
		return nil
	}
	m, err := ReadTrickplayManifest(dataDir, itemID)
	if err != nil {
		return nil
	}
	if m.Format == "" {
		m.Format = "jpg"
	}
	if len(m.TimestampsMS) != 0 && len(m.TimestampsMS) != m.ThumbCount {
		return nil
	}
	urls := make([]string, m.SheetCount)
	for i := range urls {
		urls[i] = fmt.Sprintf("/api/v1/items/%s/trickplay/%d.%s?v=%d", itemID, i, m.Format, m.GeneratedAt)
	}
	return &TrickplayInfo{
		Format:       m.Format,
		TimestampsMS: m.TimestampsMS,
		IntervalMS:   m.IntervalMS,
		ThumbWidth:   m.ThumbWidth,
		ThumbHeight:  m.ThumbHeight,
		Columns:      m.Columns,
		Rows:         m.Rows,
		ThumbCount:   m.ThumbCount,
		SheetURLs:    urls,
	}
}
