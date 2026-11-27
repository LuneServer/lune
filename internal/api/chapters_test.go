package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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

func TestChapterThumbnailsEndpointAndDTOPopulation(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	moviesRoot := t.TempDir()
	videoPath := filepath.Join(moviesRoot, "Dune (2021).mp4")
	if err := os.WriteFile(videoPath, []byte("fake-mp4-content"), 0o644); err != nil {
		t.Fatalf("write video: %v", err)
	}

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
		"password": "admin-password-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	adminCookie := rec.Result().Cookies()[0]

	// Create two libraries: one with our movie, one empty for restricted-user ACL testing.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": moviesRoot,
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create movies lib = %d", rec.Code)
	}
	var moviesLib LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &moviesLib)

	otherRoot := t.TempDir()
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Other",
		"kind":      "movies",
		"root_path": otherRoot,
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create other lib = %d", rec.Code)
	}
	var otherLib LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &otherLib)

	// Create a restricted member who only has access to otherLib.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":      "restricted",
		"password":      "member-password-99",
		"role":          "member",
		"all_libraries": false,
		"library_ids":   []string{otherLib.ID},
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create restricted user = %d body=%s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "restricted",
		"password":    "member-password-99",
		"client_kind": "web",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("login restricted user = %d", rec.Code)
	}
	restrictedCookie := rec.Result().Cookies()[0]

	// Insert a media item with two chapters; only chapter 0 has a still on disk.
	chaptersJSON, _ := json.Marshal([]metadata.ChapterInfo{
		{Index: 0, Title: "Opening", StartMS: 0, EndMS: 90000, Kind: "intro"},
		{Index: 1, Title: "Main Story", StartMS: 90000, EndMS: 600000, Kind: "chapter"},
	})
	itemID := "item-chap-test"
	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID:           itemID,
		LibraryID:    moviesLib.ID,
		Kind:         "movie",
		Title:        "Dune",
		SortTitle:    "Dune",
		FilePath:     videoPath,
		Container:    "mp4",
		VideoCodec:   "h264",
		AudioCodec:   "aac",
		DurationMS:   600000,
		ChaptersJSON: string(chaptersJSON),
	}); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	jpegBytes := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0xFF, 0xD9}
	stillPath := filepath.Join(dataDir, "artwork", "chapters", itemID, "0.jpg")
	if err := os.MkdirAll(filepath.Dir(stillPath), 0o755); err != nil {
		t.Fatalf("mkdir chapters: %v", err)
	}
	if err := os.WriteFile(stillPath, jpegBytes, 0o644); err != nil {
		t.Fatalf("write still: %v", err)
	}

	// 1. Item detail populates thumbnail_url ONLY for chapter 0.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+itemID, nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET item detail = %d body=%s", rec.Code, rec.Body.String())
	}
	var detail MediaItemDetailDTO
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("unmarshal detail: %v", err)
	}
	if len(detail.Chapters) != 2 {
		t.Fatalf("expected 2 chapters, got %d", len(detail.Chapters))
	}
	wantURL := "/api/v1/items/" + itemID + "/artwork/chapters/0"
	if !strings.HasPrefix(detail.Chapters[0].ThumbnailURL, wantURL+"?v=") {
		t.Fatalf("detail chapter 0 thumbnail_url = %q, want %q", detail.Chapters[0].ThumbnailURL, wantURL)
	}
	if detail.Chapters[1].ThumbnailURL != "" {
		t.Fatalf("detail chapter 1 thumbnail_url = %q, want empty", detail.Chapters[1].ThumbnailURL)
	}

	// 2. Playback negotiation also populates thumbnail_url ONLY for chapter 0.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/playback", map[string]any{
		"containers":   []string{"mp4"},
		"video_codecs": []string{"h264"},
		"audio_codecs": []string{"aac"},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST playback = %d body=%s", rec.Code, rec.Body.String())
	}
	var plan stream.PlaybackPlan
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	if len(plan.Chapters) != 2 {
		t.Fatalf("expected 2 playback chapters, got %d", len(plan.Chapters))
	}
	if !strings.HasPrefix(plan.Chapters[0].ThumbnailURL, wantURL+"?v=") {
		t.Fatalf("playback chapter 0 thumbnail_url = %q, want %q", plan.Chapters[0].ThumbnailURL, wantURL)
	}
	if plan.Chapters[1].ThumbnailURL != "" {
		t.Fatalf("playback chapter 1 thumbnail_url = %q, want empty", plan.Chapters[1].ThumbnailURL)
	}
	if plan.Trickplay != nil {
		t.Fatalf("expected no trickplay before generation, got %+v", plan.Trickplay)
	}

	// 3. Unauthenticated request -> 401.
	req := httptest.NewRequest(http.MethodGet, wantURL, nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated chapter thumb status = %d, want 401", rec.Code)
	}

	// 4. Restricted user without library access -> 403.
	req = httptest.NewRequest(http.MethodGet, wantURL, nil)
	req.AddCookie(restrictedCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("restricted user chapter thumb status = %d, want 403", rec.Code)
	}

	// 5. Authorized user gets 200 JPEG with private 7-day Cache-Control.
	req = httptest.NewRequest(http.MethodGet, wantURL, nil)
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("authorized chapter thumb status = %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("Content-Type = %q, want image/jpeg", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=604800" {
		t.Fatalf("Cache-Control = %q, want private, max-age=604800", cc)
	}
	if string(rec.Body.Bytes()) != string(jpegBytes) {
		t.Fatalf("unexpected JPEG body length %d", rec.Body.Len())
	}

	// 6. Missing chapter still -> 404.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/items/"+itemID+"/artwork/chapters/1", nil)
	req.AddCookie(adminCookie)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing chapter thumb status = %d, want 404", rec.Code)
	}

	// 7. Invalid / path-traversal indices -> 404.
	for _, badIdx := range []string{"-1", "abc", "01", "..", "0.jpg"} {
		req = httptest.NewRequest(http.MethodGet, "/api/v1/items/"+itemID+"/artwork/chapters/"+badIdx, nil)
		req.AddCookie(adminCookie)
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("bad index %q status = %d, want 404", badIdx, rec.Code)
		}
	}

	// 8. Trickplay: manifest appears in the playback plan and sheets are served with ACLs.
	tpDir := metadata.TrickplayDir(dataDir, itemID)
	if err := os.MkdirAll(tpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifest, _ := json.Marshal(metadata.TrickplayManifest{
		Version: 1, IntervalMS: 10000, ThumbWidth: 320, ThumbHeight: 180, Columns: 10, Rows: 10,
		ThumbCount: 60, SheetCount: 1, ConfiguredWidth: 320, KeyframesOnly: true, GeneratedAt: 1700000000,
	})
	if err := os.WriteFile(filepath.Join(tpDir, metadata.TrickplayManifestName), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tpDir, "0.jpg"), jpegBytes, 0o644); err != nil {
		t.Fatal(err)
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/playback", map[string]any{
		"containers":   []string{"mp4"},
		"video_codecs": []string{"h264"},
		"audio_codecs": []string{"aac"},
	}, adminCookie, "")
	plan = stream.PlaybackPlan{}
	if err := json.Unmarshal(rec.Body.Bytes(), &plan); err != nil {
		t.Fatalf("unmarshal plan: %v", err)
	}
	sheetURL := "/api/v1/items/" + itemID + "/trickplay/0.jpg?v=1700000000"
	if plan.Trickplay == nil || plan.Trickplay.ThumbCount != 60 || len(plan.Trickplay.SheetURLs) != 1 || plan.Trickplay.SheetURLs[0] != sheetURL {
		t.Fatalf("unexpected trickplay in plan: %+v", plan.Trickplay)
	}

	get := func(path string, cookie *http.Cookie) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	if rec := get(sheetURL, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated trickplay status = %d, want 401", rec.Code)
	}
	if rec := get(sheetURL, restrictedCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("restricted trickplay status = %d, want 403", rec.Code)
	}
	rec = get(sheetURL, adminCookie)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/jpeg" || string(rec.Body.Bytes()) != string(jpegBytes) {
		t.Fatalf("authorized trickplay status = %d ct=%q", rec.Code, rec.Header().Get("Content-Type"))
	}
	for _, bad := range []string{"1.jpg", "manifest.json", "..%2Fmanifest.json", "0.png", "x.jpg"} {
		if rec := get("/api/v1/items/"+itemID+"/trickplay/"+bad, adminCookie); rec.Code != http.StatusNotFound {
			t.Fatalf("trickplay %q status = %d, want 404", bad, rec.Code)
		}
	}
}
