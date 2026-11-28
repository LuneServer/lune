package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

type animeStubProber struct{}

func (animeStubProber) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{
		Container:     "mkv",
		DurationMS:    1440000,
		Bitrate:       8000000,
		VideoCodec:    "h264",
		VideoWidth:    1920,
		VideoHeight:   1080,
		VideoHDR:      "sdr",
		AudioCodec:    "aac",
		AudioChannels: 2,
		Streams: []metadata.StreamInfo{
			{Index: 0, Type: "video", Codec: "h264", Width: 1920, Height: 1080, Default: true},
			{Index: 1, Type: "audio", Codec: "aac", Channels: 2, Language: "jpn", Title: "Japanese", Default: true},
			{Index: 2, Type: "audio", Codec: "aac", Channels: 2, Language: "eng", Title: "English Dub"},
			{Index: 3, Type: "subtitle", Codec: "ass", Language: "eng", Title: "Signs & Songs", Forced: true},
			{Index: 4, Type: "subtitle", Codec: "ass", Language: "eng", Title: "Full Subtitles", Default: true},
		},
		Chapters: []metadata.ChapterInfo{
			{Index: 0, Title: "OP", StartMS: 0, EndMS: 90000, Kind: "intro"},
			{Index: 1, Title: "Main", StartMS: 90000, EndMS: 1350000, Kind: "chapter"},
			{Index: 2, Title: "ED", StartMS: 1350000, EndMS: 1440000, Kind: "outro"},
		},
	}, nil
}

func TestAnimeLibraryFirstClassFlow(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	animeRoot := t.TempDir()
	seriesDir := filepath.Join(animeRoot, "Sousou no Frieren")
	_ = os.MkdirAll(seriesDir, 0o755)
	ep1 := filepath.Join(seriesDir, "[SubsPlease] Sousou no Frieren - 01 (1080p) [A1B2C3D4].mkv")
	ep2 := filepath.Join(seriesDir, "[SubsPlease] Sousou no Frieren - 02 (1080p) [B2C3D4E5].mkv")
	_ = os.WriteFile(ep1, []byte("fake-anime-ep1"), 0o644)
	_ = os.WriteFile(ep2, []byte("fake-anime-ep2"), 0o644)

	// Add sidecar ASS subtitles (both full and signs & songs).
	assContent := "[Events]\nFormat: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\nDialogue: 0,0:00:01.00,0:00:04.00,Default,,0,0,0,,{\\an8}Beyond Journey's End\n"
	_ = os.WriteFile(strings.TrimSuffix(ep1, ".mkv")+".en.ass", []byte(assContent), 0o644)
	_ = os.WriteFile(strings.TrimSuffix(ep1, ".mkv")+".en.signs.ass", []byte(assContent), 0o644)

	// Add standalone anime movie in the same anime library.
	movieDir := filepath.Join(animeRoot, "Spirited Away (2001)")
	_ = os.MkdirAll(movieDir, 0o755)
	_ = os.WriteFile(filepath.Join(movieDir, "[EMBER] Spirited Away (2001) [1080p] [Dual Audio].mkv"), []byte("fake-anime-movie"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, animeStubProber{}, nil)
	streamSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
	defer streamSvc.Shutdown()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
		Stream:    streamSvc,
		StartedAt: time.Now(),
	})

	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]

	// 1. Register an "anime" library.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Anime",
		"kind":      "anime",
		"root_path": animeRoot,
	}, cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create anime library status = %d body = %s", rec.Code, rec.Body.String())
	}
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)
	if libResp.Kind != "anime" {
		t.Fatalf("expected library kind anime, got %q", libResp.Kind)
	}

	// 2. Scan library synchronously.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status = %d", rec.Code)
	}
	var scanRes library.ScanStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &scanRes)
	if scanRes.Added != 3 || scanRes.Errors != 0 {
		t.Fatalf("expected 3 indexed anime items, got %+v", scanRes)
	}

	// 3. Verify series grouping reports library_kind="anime" & dual_audio=true.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/series?library_id="+libResp.ID, nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list series status = %d", rec.Code)
	}
	var seriesResp struct {
		Series []SeriesSummaryDTO `json:"series"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &seriesResp)
	if len(seriesResp.Series) != 1 {
		t.Fatalf("expected 1 anime series, got %d", len(seriesResp.Series))
	}
	s0 := seriesResp.Series[0]
	if s0.Title != "Sousou no Frieren" || s0.LibraryKind != "anime" || !s0.DualAudio || s0.EpisodeCount != 2 {
		t.Fatalf("unexpected series summary: %+v", s0)
	}

	// 4. Verify standalone movie also exists in the anime library.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?library_id="+libResp.ID+"&kind=movie", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list anime movies status = %d", rec.Code)
	}
	var moviesResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &moviesResp)
	if len(moviesResp.Items) != 1 || moviesResp.Items[0].Title != "Spirited Away" || moviesResp.Items[0].ReleaseGroup != "EMBER" || !moviesResp.Items[0].DualAudio {
		t.Fatalf("unexpected anime movie: %+v", moviesResp.Items)
	}

	// 5. Verify series detail & Next Episode resolution for absolute-numbered fansub episodes.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/series/detail?library_id="+libResp.ID+"&title="+url.QueryEscape("Sousou no Frieren"), nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series detail status = %d", rec.Code)
	}
	var detailResp struct {
		LibraryKind string            `json:"library_kind"`
		DualAudio   bool              `json:"dual_audio"`
		Seasons     []SeriesSeasonDTO `json:"seasons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detailResp)
	if detailResp.LibraryKind != "anime" || !detailResp.DualAudio || len(detailResp.Seasons) != 1 || len(detailResp.Seasons[0].Episodes) != 2 {
		t.Fatalf("unexpected anime series detail: %+v", detailResp)
	}
	ep1ID := detailResp.Seasons[0].Episodes[0].Item.ID
	ep2ID := detailResp.Seasons[0].Episodes[1].Item.ID

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+ep1ID+"/next", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("next episode status = %d", rec.Code)
	}
	var nextResp struct {
		NextItem *MediaItemSummaryDTO `json:"next_item"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &nextResp)
	if nextResp.NextItem == nil || nextResp.NextItem.ID != ep2ID {
		t.Fatalf("expected next episode %s, got %+v", ep2ID, nextResp.NextItem)
	}

	// 6. Verify playback negotiation: smart Japanese audio + English FULL subtitles + chapters + ASS->VTT serving!
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+ep1ID+"/playback", map[string]any{}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("negotiate playback status = %d body = %s", rec.Code, rec.Body.String())
	}
	var plan stream.PlaybackPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if plan.SelectedAudioIndex == nil || *plan.SelectedAudioIndex != 1 {
		t.Fatalf("expected Japanese audio stream 1 selected by default for anime, got %v", plan.SelectedAudioIndex)
	}
	if plan.SelectedSubtitleIndex == nil {
		t.Fatalf("expected full English subtitle auto-selected for Japanese anime audio")
	}
	var selectedSub *stream.SubtitleStreamDTO
	for i := range plan.Subtitles {
		if plan.Subtitles[i].Index == *plan.SelectedSubtitleIndex {
			selectedSub = &plan.Subtitles[i]
			break
		}
	}
	if selectedSub == nil || selectedSub.Forced {
		t.Fatalf("expected non-forced full subtitle selected for subbed anime, got %+v", selectedSub)
	}
	if len(plan.Chapters) != 3 || plan.Chapters[0].Kind != "intro" || plan.Chapters[2].Kind != "outro" {
		t.Fatalf("expected 3 chapters with intro/outro, got %+v", plan.Chapters)
	}
	if len(plan.Segments) != 2 || plan.Segments[0].Kind != "intro" || plan.Segments[0].Source != "chapter" ||
		plan.Segments[1].Kind != "outro" {
		t.Fatalf("expected intro/outro chapter segments, got %+v", plan.Segments)
	}

	// Fetch sidecar ASS subtitle converted to WebVTT on the fly.
	var sidecarSub *stream.SubtitleStreamDTO
	for i := range plan.Subtitles {
		if plan.Subtitles[i].Source == "sidecar" && !plan.Subtitles[i].Forced {
			sidecarSub = &plan.Subtitles[i]
			break
		}
	}
	if sidecarSub == nil {
		t.Fatalf("expected sidecar .ass subtitle in playback plan")
	}
	rec = doJSON(t, h, http.MethodGet, sidecarSub.URL, nil, nil, "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "WEBVTT") || !strings.Contains(rec.Body.String(), "Beyond Journey's End") {
		t.Fatalf("unexpected ASS->VTT response: status=%d body=%q", rec.Code, rec.Body.String())
	}

	// 7. Switching user preference to English Dub should auto-select English audio + Forced Signs & Songs subtitle!
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"preferences": map[string]any{
			"audio_language":    "en",
			"subtitle_language": "en",
			"subtitles_enabled": false,
			"autoplay_next":     true,
		},
	}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update preferences status = %d body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+ep1ID+"/playback", map[string]any{}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("negotiate dub playback status = %d", rec.Code)
	}
	var dubPlan stream.PlaybackPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &dubPlan)
	if dubPlan.SelectedAudioIndex == nil || *dubPlan.SelectedAudioIndex != 2 {
		t.Fatalf("expected English dub audio stream 2, got %v", dubPlan.SelectedAudioIndex)
	}
	if dubPlan.SelectedSubtitleIndex == nil {
		t.Fatalf("expected forced Signs & Songs subtitle auto-selected for English dub")
	}
	var dubSub *stream.SubtitleStreamDTO
	for i := range dubPlan.Subtitles {
		if dubPlan.Subtitles[i].Index == *dubPlan.SelectedSubtitleIndex {
			dubSub = &dubPlan.Subtitles[i]
			break
		}
	}
	if dubSub == nil || !dubSub.Forced {
		t.Fatalf("expected forced subtitle for English dub, got %+v", dubSub)
	}
}
