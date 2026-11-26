package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

func TestSeriesGroupingNextUpAndSeasonBridge(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	showsRoot := t.TempDir()
	s1 := filepath.Join(showsRoot, "Deep Space Echoes (2026)", "Season 01")
	s2 := filepath.Join(showsRoot, "Deep Space Echoes (2026)", "Season 02")
	_ = os.MkdirAll(s1, 0o755)
	_ = os.MkdirAll(s2, 0o755)
	_ = os.WriteFile(filepath.Join(showsRoot, "Deep Space Echoes (2026)", "poster.png"), []byte("png"), 0o644)
	_ = os.WriteFile(filepath.Join(s1, "Deep.Space.Echoes.S01E01.First.Signal.mp4"), []byte("ep1"), 0o644)
	_ = os.WriteFile(filepath.Join(s1, "Deep.Space.Echoes.S01E02.Orbital.Drift.mp4"), []byte("ep2"), 0o644)
	_ = os.WriteFile(filepath.Join(s2, "Deep.Space.Echoes.S02E01.New.Horizon.mp4"), []byte("ep3"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)
	streamSvc, _ := stream.NewService(ctx, db, dataDir, nil)
	defer streamSvc.Shutdown()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
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
	cookie := rec.Result().Cookies()[0]

	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "TV Shows",
		"kind":      "shows",
		"root_path": showsRoot,
	}, cookie, "")
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)
	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")

	// 1. /api/v1/series groups all 3 episodes under 1 series with 2 seasons.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/series", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list series status = %d", rec.Code)
	}
	var listResp struct {
		Series []SeriesSummaryDTO `json:"series"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Series) != 1 || listResp.Series[0].SeasonCount != 2 || listResp.Series[0].EpisodeCount != 3 {
		t.Fatalf("unexpected series summary: %+v", listResp.Series)
	}
	if listResp.Series[0].PosterURL == "" {
		t.Fatal("expected series poster URL")
	}

	// 2. /api/v1/series/detail returns ordered seasons + Next Up = S01E01.
	detailURL := "/api/v1/series/detail?library_id=" + url.QueryEscape(libResp.ID) + "&title=" + url.QueryEscape("Deep Space Echoes")
	rec = doJSON(t, h, http.MethodGet, detailURL, nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series detail status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var detail struct {
		SeasonCount  int                 `json:"season_count"`
		EpisodeCount int                 `json:"episode_count"`
		NextUp       *MediaItemSummaryDTO `json:"next_up"`
		Seasons      []SeriesSeasonDTO   `json:"seasons"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.SeasonCount != 2 || len(detail.Seasons[0].Episodes) != 2 || len(detail.Seasons[1].Episodes) != 1 {
		t.Fatalf("unexpected seasons structure: %+v", detail.Seasons)
	}
	ep1 := detail.Seasons[0].Episodes[0].Item
	ep2 := detail.Seasons[0].Episodes[1].Item
	ep3 := detail.Seasons[1].Episodes[0].Item
	if detail.NextUp == nil || detail.NextUp.ID != ep1.ID {
		t.Fatalf("expected NextUp=S01E01, got %+v", detail.NextUp)
	}

	// 3. Completing S01E01 advances Next Up to S01E02.
	completed := true
	doJSON(t, h, http.MethodPut, "/api/v1/items/"+ep1.ID+"/progress", map[string]any{
		"position_ms": 7_100_000,
		"completed":   completed,
	}, cookie, "")
	rec = doJSON(t, h, http.MethodGet, detailURL, nil, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.NextUp == nil || detail.NextUp.ID != ep2.ID {
		t.Fatalf("expected NextUp=S01E02 after completing S01E01, got %+v", detail.NextUp)
	}

	// 4. /api/v1/items/{ep2}/next bridges Season 01 -> Season 02 (ep3), and ep3 -> nil.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+ep2.ID+"/next", nil, cookie, "")
	var nextResp struct {
		NextItem *MediaItemSummaryDTO `json:"next_item"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &nextResp)
	if nextResp.NextItem == nil || nextResp.NextItem.ID != ep3.ID {
		t.Fatalf("expected S01E02 -> S02E01 bridge, got %+v", nextResp.NextItem)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+ep3.ID+"/next", nil, cookie, "")
	nextResp.NextItem = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &nextResp)
	if nextResp.NextItem != nil {
		t.Fatalf("expected nil after final episode, got %+v", nextResp.NextItem)
	}
}
