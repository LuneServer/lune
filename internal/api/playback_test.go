package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

func TestPlaybackSignedRangeStreamAndProgress(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	movieFile := filepath.Join(mediaRoot, "Arrival (2016).mp4")
	payload := "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	_ = os.WriteFile(movieFile, []byte(payload), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)
	streamSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
	defer streamSvc.Shutdown()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
		Stream:    streamSvc,
		StartedAt: time.Now(),
	})

	// Setup admin, create library, scan.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	cookie := rec.Result().Cookies()[0]

	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": mediaRoot,
	}, cookie, "")
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)

	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items", nil, cookie, "")
	var itemsResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	if len(itemsResp.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(itemsResp.Items))
	}
	itemID := itemsResp.Items[0].ID

	// 1. Negotiate Direct Play stream URL.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/playback", map[string]any{
		"client_name": "Lune Web",
	}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("playback negotiate status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var plan stream.PlaybackPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if plan.Mode != "direct" || plan.StreamURL == "" {
		t.Fatalf("unexpected playback plan: %+v", plan)
	}

	// 2. Fetch signed byte range WITHOUT cookies or Bearer auth (simulates native video player).
	rangeReq := httptest.NewRequest(http.MethodGet, plan.StreamURL, nil)
	rangeReq.Header.Set("Range", "bytes=0-9")
	rangeRec := httptest.NewRecorder()
	h.ServeHTTP(rangeRec, rangeReq)
	if rangeRec.Code != http.StatusPartialContent {
		t.Fatalf("range stream status = %d, want 206", rangeRec.Code)
	}
	if rangeRec.Body.String() != "0123456789" {
		t.Fatalf("range body = %q, want %q", rangeRec.Body.String(), "0123456789")
	}

	// Tampered stream token is rejected.
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, plan.StreamURL+"tampered", nil))
	if badRec.Code != http.StatusUnauthorized {
		t.Fatalf("tampered stream status = %d, want 401", badRec.Code)
	}

	// 3. Active streams endpoint reports the live session to admins.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/streams", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin streams status = %d", rec.Code)
	}
	var activeResp struct {
		Streams []stream.ActiveStream `json:"streams"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &activeResp)
	if len(activeResp.Streams) != 1 || activeResp.Streams[0].SessionID != plan.SessionID {
		t.Fatalf("expected active stream %s, got %+v", plan.SessionID, activeResp.Streams)
	}

	// 4. Update watch progress -> appears in Continue Watching & resume_position_ms.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/items/"+itemID+"/progress", map[string]any{
		"position_ms": 1_800_000, // 30m of 120m
	}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update progress status = %d", rec.Code)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/playback/continue", nil, cookie, "")
	var cwResp struct {
		Items []ContinueWatchingItemDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cwResp)
	if len(cwResp.Items) != 1 || cwResp.Items[0].Progress.PositionMS != 1_800_000 {
		t.Fatalf("unexpected continue watching: %+v", cwResp.Items)
	}

	// Re-negotiating returns resume_position_ms = 1_800_000.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/playback", map[string]any{}, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if plan.ResumePositionMS != 1_800_000 {
		t.Fatalf("expected resume_position_ms=1800000, got %d", plan.ResumePositionMS)
	}

	// 5. Clearing progress via DELETE removes an in-progress item from Continue Watching.
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/items/"+itemID+"/progress", nil, cookie, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("clear progress status = %d, want 204", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/playback/continue", nil, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &cwResp)
	if len(cwResp.Items) != 0 {
		t.Fatalf("expected cleared item removed from continue watching, got %d", len(cwResp.Items))
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+itemID+"/progress", nil, cookie, "")
	var clearedProg ProgressDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &clearedProg)
	if clearedProg.PositionMS != 0 || clearedProg.Completed {
		t.Fatalf("expected cleared progress reset to 0, got %+v", clearedProg)
	}

	// 6. Add item to both Watchlist and Favorites, then reach >=92% completion:
	//    it auto-completes, leaves Continue Watching, auto-clears Watchlist, and preserves Favorites.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/users/me/watchlist/"+itemID, nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add watchlist status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/users/me/favorites/"+itemID, nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add favorite status = %d", rec.Code)
	}

	rec = doJSON(t, h, http.MethodPut, "/api/v1/items/"+itemID+"/progress", map[string]any{
		"position_ms": 6_900_000, // >95% of 7_200_000
	}, cookie, "")
	var prog ProgressDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &prog)
	if !prog.Completed || prog.PlayCount != 1 {
		t.Fatalf("expected completed=true play_count=1, got %+v", prog)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/playback/continue", nil, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &cwResp)
	if len(cwResp.Items) != 0 {
		t.Fatalf("expected completed item removed from continue watching, got %d", len(cwResp.Items))
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+itemID, nil, cookie, "")
	var detailAfter MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &detailAfter)
	if detailAfter.InWatchlist {
		t.Fatalf("expected watchlist mark auto-cleared on completion")
	}
	if !detailAfter.IsFavorite {
		t.Fatalf("expected favorite mark preserved on completion")
	}
}

type stubPersonArtEnricher struct {
	err error
}

func (s stubPersonArtEnricher) EnrichMovie(context.Context, string, string, *int) (metadata.EnrichedInfo, error) {
	return metadata.EnrichedInfo{}, nil
}

func (s stubPersonArtEnricher) EnrichEpisode(context.Context, string, string, int, int, *int) (metadata.EnrichedInfo, error) {
	return metadata.EnrichedInfo{}, nil
}

func (s stubPersonArtEnricher) FetchPersonPhoto(context.Context, string) (string, error) {
	return "", s.err
}

func TestNextUpEndpointAndPersonArtworkErrors(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	showDir := filepath.Join(mediaRoot, "Andor", "Season 01")
	_ = os.MkdirAll(showDir, 0o755)
	_ = os.WriteFile(filepath.Join(showDir, "Andor - S01E01 - Kassa.mp4"), []byte("ep1"), 0o644)
	_ = os.WriteFile(filepath.Join(showDir, "Andor - S01E02 - That Would Be Me.mp4"), []byte("ep2"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)
	streamSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
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
		"name":      "Shows",
		"kind":      "shows",
		"root_path": mediaRoot,
	}, cookie, "")
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)

	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?library_id="+libResp.ID, nil, cookie, "")
	var itemsResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	var ep1ID, ep2ID string
	for _, it := range itemsResp.Items {
		if it.EpisodeNumber != nil && *it.EpisodeNumber == 1 {
			ep1ID = it.ID
		}
		if it.EpisodeNumber != nil && *it.EpisodeNumber == 2 {
			ep2ID = it.ID
		}
	}
	if ep1ID == "" || ep2ID == "" {
		t.Fatalf("expected ep1 and ep2 scanned, got %+v", itemsResp.Items)
	}

	// Complete S01E01 -> GET /api/v1/playback/next-up returns S01E02.
	completed := true
	rec = doJSON(t, h, http.MethodPut, "/api/v1/items/"+ep1ID+"/progress", map[string]any{
		"position_ms": 7_200_000,
		"completed":   completed,
	}, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update progress status = %d", rec.Code)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/playback/next-up?limit=10", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("next-up status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var nextUpResp struct {
		Items []NextUpItemDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &nextUpResp)
	if len(nextUpResp.Items) != 1 || nextUpResp.Items[0].Item.ID != ep2ID || nextUpResp.Items[0].LastPlayedAt.IsZero() {
		t.Fatalf("unexpected next-up response: %+v", nextUpResp.Items)
	}

	// Seed an indexed cast photo so /api/v1/artwork/people/test.jpg passes the indexed-cast allowlist.
	ep1, err := db.GetMediaItem(ctx, ep1ID)
	if err != nil {
		t.Fatalf("GetMediaItem: %v", err)
	}
	ep1.CastJSON = `[{"name":"Diego Luna","role":"Cassian Andor","photo_path":"/test.jpg"}]`
	if err := db.UpsertMediaItem(ctx, ep1); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	// Verify person artwork HTTP status mapping: 404 vs 503 vs 504.
	fetchArt := func(file string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/artwork/people/"+file, nil)
		req.AddCookie(cookie)
		r := httptest.NewRecorder()
		h.ServeHTTP(r, req)
		return r
	}

	// Unindexed filename is rejected with 404 without hitting upstream.
	libSvc.SetEnricher(stubPersonArtEnricher{err: errors.New("upstream should not be called")})
	if r := fetchArt("unindexed.jpg"); r.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for unindexed person photo, got %d", r.Code)
	}

	libSvc.SetEnricher(stubPersonArtEnricher{err: metadata.ErrArtworkNotFound})
	if r := fetchArt("test.jpg"); r.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ErrArtworkNotFound, got %d", r.Code)
	}

	libSvc.SetEnricher(stubPersonArtEnricher{err: errors.New("upstream 503")})
	if r := fetchArt("test.jpg"); r.Code != http.StatusServiceUnavailable || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected 503 + no-store for transient error, got %d (%q)", r.Code, r.Header().Get("Cache-Control"))
	}

	libSvc.SetEnricher(stubPersonArtEnricher{err: context.DeadlineExceeded})
	if r := fetchArt("test.jpg"); r.Code != http.StatusGatewayTimeout || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected 504 + no-store for deadline exceeded, got %d (%q)", r.Code, r.Header().Get("Cache-Control"))
	}
}

func TestAdminStreamingConfigEndpoints(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)
	streamSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
	defer streamSvc.Shutdown()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
		Stream:    streamSvc,
		StartedAt: time.Now(),
	})

	// Setup admin user.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "admin",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	adminCookie := rec.Result().Cookies()[0]

	// Create a non-admin member and log in as member.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":      "member1",
		"password":      "member-secret-99",
		"role":          "member",
		"all_libraries": true,
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "member1",
		"password":    "member-secret-99",
		"client_kind": "web",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	memberCookie := rec.Result().Cookies()[0]

	// 1. Unauthenticated requests -> 401.
	if r := doJSON(t, h, http.MethodGet, "/api/v1/admin/streaming/config", nil, nil, ""); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET status = %d, want 401", r.Code)
	}
	if r := doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 4,
		"max_user_transcodes":   3,
		"idle_timeout_minutes":  10,
	}, nil, ""); r.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated PUT status = %d, want 401", r.Code)
	}

	// 2. Non-admin member requests -> 403.
	if r := doJSON(t, h, http.MethodGet, "/api/v1/admin/streaming/config", nil, memberCookie, ""); r.Code != http.StatusForbidden {
		t.Fatalf("member GET status = %d, want 403", r.Code)
	}
	if r := doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 4,
		"max_user_transcodes":   3,
		"idle_timeout_minutes":  10,
	}, memberCookie, ""); r.Code != http.StatusForbidden {
		t.Fatalf("member PUT status = %d, want 403", r.Code)
	}

	// 3. Admin GET returns default config (unlimited global=0, per-user=3, idle=10).
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/streaming/config", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var cfg stream.StreamingConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg.MaxGlobalTranscodes != 0 || cfg.MaxUserTranscodes != 3 || cfg.IdleTimeoutMinutes != 10 {
		t.Fatalf("unexpected default streaming config: %+v", cfg)
	}

	// 4. Invalid values return 422.
	if r := doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 1001,
		"max_user_transcodes":   3,
		"idle_timeout_minutes":  10,
	}, adminCookie, ""); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("out-of-range PUT status = %d, want 422", r.Code)
	}
	if r := doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 0,
		"max_user_transcodes":   101,
		"idle_timeout_minutes":  10,
	}, adminCookie, ""); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("out-of-range max_user PUT status = %d, want 422", r.Code)
	}
	if r := doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 0,
		"max_user_transcodes":   3,
		"idle_timeout_minutes":  121,
	}, adminCookie, ""); r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("out-of-range idle PUT status = %d, want 422", r.Code)
	}

	// 5. Valid PUT updates live config, persists to DB, and records audit event.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/streaming/config", map[string]any{
		"max_global_transcodes": 6,
		"max_user_transcodes":   2,
		"idle_timeout_minutes":  15,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var updated stream.StreamingConfig
	_ = json.Unmarshal(rec.Body.Bytes(), &updated)
	if updated.MaxGlobalTranscodes != 6 || updated.MaxUserTranscodes != 2 || updated.IdleTimeoutMinutes != 15 {
		t.Fatalf("unexpected updated streaming config: %+v", updated)
	}
	if updated.Transcoder == nil {
		t.Fatalf("expected transcoder diagnostics in streaming config response")
	}

	// Live service reflects update immediately.
	if live := streamSvc.GetStreamingConfig(); live.MaxGlobalTranscodes != updated.MaxGlobalTranscodes ||
		live.MaxUserTranscodes != updated.MaxUserTranscodes ||
		live.IdleTimeoutMinutes != updated.IdleTimeoutMinutes {
		t.Fatalf("live streaming config = %+v, want %+v", live, updated)
	}

	// Persisted setting is loaded by a fresh stream.NewService instance.
	restartedSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("restarted stream.NewService: %v", err)
	}
	defer restartedSvc.Shutdown()
	if loaded := restartedSvc.GetStreamingConfig(); loaded.MaxGlobalTranscodes != updated.MaxGlobalTranscodes ||
		loaded.MaxUserTranscodes != updated.MaxUserTranscodes ||
		loaded.IdleTimeoutMinutes != updated.IdleTimeoutMinutes {
		t.Fatalf("reloaded streaming config = %+v, want %+v", loaded, updated)
	}

	// Audit event recorded.
	events, err := db.ListAuditEvents(ctx, 20)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	foundAudit := false
	for _, ev := range events {
		if ev.EventType == "admin.streaming_config_updated" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("expected admin.streaming_config_updated audit event, got %+v", events)
	}
}
