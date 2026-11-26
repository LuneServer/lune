package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

type stubProber struct{}

func (stubProber) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{
		Container:     "mkv",
		DurationMS:    7200000,
		Bitrate:       24800000,
		VideoCodec:    "hevc",
		VideoWidth:    3840,
		VideoHeight:   2160,
		VideoHDR:      "hdr10",
		AudioCodec:    "eac3",
		AudioChannels: 6,
		Streams: []metadata.StreamInfo{
			{Index: 0, Type: "video", Codec: "hevc", Width: 3840, Height: 2160, HDR: "hdr10", Default: true},
			{Index: 1, Type: "audio", Codec: "eac3", Channels: 6, Language: "eng", Default: true},
		},
	}, nil
}

func TestLibraryScanBrowseAndIncrementalPrune(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	movieDir := filepath.Join(mediaRoot, "Interstellar (2014)")
	_ = os.MkdirAll(movieDir, 0o755)
	movieFile := filepath.Join(movieDir, "Interstellar (2014) 2160p.mkv")
	_ = os.WriteFile(movieFile, []byte("fake-mkv-payload"), 0o644)
	_ = os.WriteFile(filepath.Join(movieDir, "poster.jpg"), []byte("fake-jpeg-bytes"), 0o644)
	// Sample file must be ignored by scanner.
	_ = os.WriteFile(filepath.Join(movieDir, "Interstellar-sample.mkv"), []byte("sample"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
		StartedAt: time.Now(),
	})

	// Setup admin & obtain session cookie.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	cookie := rec.Result().Cookies()[0]

	// 1. Register library.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": mediaRoot,
	}, cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create library status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)

	// 2. Run sync scan.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var scanRes library.ScanStatus
	_ = json.Unmarshal(rec.Body.Bytes(), &scanRes)
	if scanRes.Added != 1 || scanRes.Scanned != 1 || scanRes.Errors != 0 {
		t.Fatalf("unexpected first scan stats: %+v", scanRes)
	}

	// 3. Second scan with unchanged file skips cleanly.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &scanRes)
	if scanRes.Skipped != 1 || scanRes.Added != 0 || scanRes.Updated != 0 {
		t.Fatalf("expected unchanged file to be skipped, got %+v", scanRes)
	}

	// 4. Browse & inspect item detail + poster artwork.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?q=interstellar", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list items status = %d", rec.Code)
	}
	var listResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
		Total int                   `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if listResp.Total != 1 || listResp.Items[0].Title != "Interstellar" || listResp.Items[0].VideoHDR != "hdr10" {
		t.Fatalf("unexpected items response: %+v", listResp)
	}

	itemID := listResp.Items[0].ID
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+itemID, nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("get item detail status = %d", rec.Code)
	}
	var detail MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if len(detail.Streams) != 2 || detail.PosterURL == "" {
		t.Fatalf("unexpected item detail: %+v", detail)
	}

	rec = doJSON(t, h, http.MethodGet, detail.PosterURL, nil, cookie, "")
	if rec.Code != http.StatusOK || rec.Body.String() != "fake-jpeg-bytes" {
		t.Fatalf("poster fetch status=%d body=%q", rec.Code, rec.Body.String())
	}

	// 5. Removing file from disk prunes it on the next scan.
	_ = os.Remove(movieFile)
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &scanRes)
	if scanRes.Removed != 1 {
		t.Fatalf("expected 1 pruned item, got %+v", scanRes)
	}
}

type stubLazyArtEnricher struct {
	fn func(ctx context.Context, localPath string) (string, error)
}

func (stubLazyArtEnricher) EnrichMovie(context.Context, string, string, *int) (metadata.EnrichedInfo, error) {
	return metadata.EnrichedInfo{}, nil
}

func (stubLazyArtEnricher) EnrichEpisode(context.Context, string, string, int, int, *int) (metadata.EnrichedInfo, error) {
	return metadata.EnrichedInfo{}, nil
}

func (s stubLazyArtEnricher) EnsureArtwork(ctx context.Context, localPath string) (string, error) {
	return s.fn(ctx, localPath)
}

func TestItemArtworkLazyResolutionAndStatusMapping(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	movieFile := filepath.Join(mediaRoot, "Interstellar (2014).mkv")
	_ = os.WriteFile(movieFile, []byte("fake-mkv-payload"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
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

	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": mediaRoot,
	}, cookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create library status = %d", rec.Code)
	}
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)

	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, cookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("scan status = %d", rec.Code)
	}

	items, _, err := db.ListMediaItems(ctx, store.MediaListFilter{LibraryID: libResp.ID, Limit: 10})
	if err != nil || len(items) != 1 {
		t.Fatalf("ListMediaItems: len=%d err=%v", len(items), err)
	}
	item := items[0]
	lazyBackdrop := filepath.Join(dataDir, "artwork", "tmdb", "w1280", "in", "InterstellarBG.jpg")
	item.BackdropPath = lazyBackdrop
	if err := db.UpsertMediaItem(ctx, item); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	artURL := "/api/v1/items/" + item.ID + "/artwork/backdrop"

	// 1. Not-found maps to 404.
	libSvc.SetEnricher(stubLazyArtEnricher{fn: func(context.Context, string) (string, error) {
		return "", metadata.ErrArtworkNotFound
	}})
	if r := doJSON(t, h, http.MethodGet, artURL, nil, cookie, ""); r.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for ErrArtworkNotFound, got %d", r.Code)
	}

	// 2. Transient upstream error maps to 503 with Cache-Control: no-store.
	libSvc.SetEnricher(stubLazyArtEnricher{fn: func(context.Context, string) (string, error) {
		return "", errors.New("upstream 503")
	}})
	if r := doJSON(t, h, http.MethodGet, artURL, nil, cookie, ""); r.Code != http.StatusServiceUnavailable || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected 503 + no-store, got %d (%q)", r.Code, r.Header().Get("Cache-Control"))
	}

	// 3. Upstream timeout maps to 504 with Cache-Control: no-store.
	libSvc.SetEnricher(stubLazyArtEnricher{fn: func(context.Context, string) (string, error) {
		return "", context.DeadlineExceeded
	}})
	if r := doJSON(t, h, http.MethodGet, artURL, nil, cookie, ""); r.Code != http.StatusGatewayTimeout || r.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("expected 504 + no-store, got %d (%q)", r.Code, r.Header().Get("Cache-Control"))
	}

	// 4. Successful lazy materialization serves the file with private cache header.
	libSvc.SetEnricher(stubLazyArtEnricher{fn: func(_ context.Context, p string) (string, error) {
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte("lazy-backdrop-bytes"), 0o644); err != nil {
			return "", err
		}
		return p, nil
	}})
	if r := doJSON(t, h, http.MethodGet, artURL, nil, cookie, ""); r.Code != http.StatusOK || r.Body.String() != "lazy-backdrop-bytes" || r.Header().Get("Cache-Control") != "private, max-age=86400" {
		t.Fatalf("expected 200 lazy backdrop, got status=%d cc=%q body=%q", r.Code, r.Header().Get("Cache-Control"), r.Body.String())
	}

	// 5. Directory path must not be served as artwork.
	item.BackdropPath = dataDir
	if err := db.UpsertMediaItem(ctx, item); err != nil {
		t.Fatalf("UpsertMediaItem dir: %v", err)
	}
	libSvc.SetEnricher(nil)
	if r := doJSON(t, h, http.MethodGet, artURL, nil, cookie, ""); r.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for directory artwork path, got %d", r.Code)
	}
}
