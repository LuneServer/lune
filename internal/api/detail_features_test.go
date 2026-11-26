package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

func TestMediaDetailProgressDirectLinkDiscoveryAndSeriesBulkWatched(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	moviesRoot := t.TempDir()
	showsRoot := t.TempDir()

	// Create two sci-fi movies with NFO metadata (sharing genre + director) + sidecar subtitle
	m1Dir := filepath.Join(moviesRoot, "Interstellar (2014)")
	m2Dir := filepath.Join(moviesRoot, "Arrival (2016)")
	_ = os.MkdirAll(m1Dir, 0o755)
	_ = os.MkdirAll(m2Dir, 0o755)
	_ = os.WriteFile(filepath.Join(m1Dir, "Interstellar (2014).mp4"), []byte("movie1-bytes"), 0o644)
	_ = os.WriteFile(filepath.Join(m1Dir, "Interstellar (2014).en.srt"), []byte("1\n00:00:01,000 --> 00:00:03,000\nHello\n"), 0o644)
	_ = os.WriteFile(filepath.Join(m1Dir, "Interstellar (2014).nfo"), []byte(`
		<movie>
			<title>Interstellar</title>
			<year>2014</year>
			<plot>Wormhole journey.</plot>
			<genre>Sci-Fi</genre>
			<genre>Drama</genre>
			<director>Christopher Nolan</director>
			<studio>Syncopy</studio>
			<actor><name>Matthew McConaughey</name><role>Cooper</role></actor>
		</movie>`), 0o644)

	_ = os.WriteFile(filepath.Join(m2Dir, "Arrival (2016).mp4"), []byte("movie2-bytes"), 0o644)
	_ = os.WriteFile(filepath.Join(m2Dir, "Arrival (2016).nfo"), []byte(`
		<movie>
			<title>Arrival</title>
			<year>2016</year>
			<plot>First contact linguistics.</plot>
			<genre>Sci-Fi</genre>
			<director>Denis Villeneuve</director>
			<studio>Paramount</studio>
			<actor><name>Amy Adams</name><role>Louise Banks</role></actor>
		</movie>`), 0o644)

	// Create a 2-season series for bulk watched tests
	s1 := filepath.Join(showsRoot, "Orbit Nine (2025)", "Season 01")
	s2 := filepath.Join(showsRoot, "Orbit Nine (2025)", "Season 02")
	_ = os.MkdirAll(s1, 0o755)
	_ = os.MkdirAll(s2, 0o755)
	_ = os.WriteFile(filepath.Join(s1, "Orbit.Nine.S01E01.Pilot.mp4"), []byte("s1e1"), 0o644)
	_ = os.WriteFile(filepath.Join(s1, "Orbit.Nine.S01E02.Second.mp4"), []byte("s1e2"), 0o644)
	_ = os.WriteFile(filepath.Join(s2, "Orbit.Nine.S02E01.Return.mp4"), []byte("s2e1"), 0o644)

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

	// Setup admin + create non-admin member to check file_path visibility
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "admin",
		"password": "admin-password-99",
	}, nil, "")
	adminCookie := rec.Result().Cookies()[0]

	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":      "viewer",
		"password":      "viewer-password-99",
		"role":          "member",
		"all_libraries": true,
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "viewer",
		"password":    "viewer-password-99",
		"client_kind": "web",
	}, nil, "")
	memberCookie := rec.Result().Cookies()[0]

	// Register and scan both libraries
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": moviesRoot,
	}, adminCookie, "")
	var moviesLib LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &moviesLib)
	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+moviesLib.ID+"/scan?sync=true", nil, adminCookie, "")

	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Shows",
		"kind":      "shows",
		"root_path": showsRoot,
	}, adminCookie, "")
	var showsLib LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &showsLib)
	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+showsLib.ID+"/scan?sync=true", nil, adminCookie, "")

	// Discover by genre, person, and studio
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?genre=Sci-Fi", nil, adminCookie, "")
	var listResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Items) != 2 {
		t.Fatalf("expected 2 Sci-Fi movies, got %d", len(listResp.Items))
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?person="+url.QueryEscape("Christopher Nolan"), nil, adminCookie, "")
	listResp.Items = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].Title != "Interstellar" {
		t.Fatalf("expected Interstellar for Christopher Nolan, got %+v", listResp.Items)
	}
	interstellarID := listResp.Items[0].ID

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?studio=Syncopy", nil, adminCookie, "")
	listResp.Items = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	if len(listResp.Items) != 1 || listResp.Items[0].ID != interstellarID {
		t.Fatalf("expected Interstellar for studio Syncopy, got %+v", listResp.Items)
	}

	// Update progress and check GET /api/v1/items/{id} returns progress, tracks, and admin-only file_path
	doJSON(t, h, http.MethodPut, "/api/v1/items/"+interstellarID+"/progress", map[string]any{
		"position_ms": 60000,
	}, adminCookie, "")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+interstellarID, nil, adminCookie, "")
	var adminDetail MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &adminDetail)
	if adminDetail.Progress == nil || adminDetail.Progress.PositionMS != 60000 {
		t.Fatalf("expected progress on item detail, got %+v", adminDetail.Progress)
	}
	if len(adminDetail.Subtitles) != 1 {
		t.Fatalf("expected 1 sidecar subtitle on item detail, got %+v", adminDetail.Subtitles)
	}
	if adminDetail.FilePath == "" {
		t.Fatal("expected file_path to be populated for admin")
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+interstellarID, nil, memberCookie, "")
	var memberDetail MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &memberDetail)
	if memberDetail.FilePath != "" {
		t.Fatalf("expected file_path to be omitted for non-admin member, got %q", memberDetail.FilePath)
	}

	// Similar items should return Arrival via shared Sci-Fi genre
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+interstellarID+"/similar", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("similar status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var simResp struct {
		Items  []MediaItemSummaryDTO `json:"items"`
		Series []SeriesSummaryDTO    `json:"series"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &simResp)
	if len(simResp.Items) != 1 || simResp.Items[0].Title != "Arrival" {
		t.Fatalf("expected Arrival as similar item, got %+v", simResp.Items)
	}

	// Direct link + attachment download
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+interstellarID+"/direct-link", nil, memberCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("direct-link status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var linkResp stream.DirectLink
	_ = json.Unmarshal(rec.Body.Bytes(), &linkResp)
	if !strings.HasPrefix(linkResp.StreamURL, "/stream/direct/") || !strings.HasSuffix(linkResp.DownloadURL, "?download=1") {
		t.Fatalf("unexpected direct link: %+v", linkResp)
	}

	dlReq := httptest.NewRequest(http.MethodGet, linkResp.DownloadURL, nil)
	dlRec := httptest.NewRecorder()
	h.ServeHTTP(dlRec, dlReq)
	if dlRec.Code != http.StatusOK {
		t.Fatalf("download status = %d", dlRec.Code)
	}
	if cd := dlRec.Header().Get("Content-Disposition"); !strings.Contains(cd, "attachment;") {
		t.Fatalf("expected attachment Content-Disposition, got %q", cd)
	}
	if dlRec.Body.String() != "movie1-bytes" {
		t.Fatalf("unexpected download body: %q", dlRec.Body.String())
	}

	// Bulk mark Season 1 watched, then entire series unwatched
	rec = doJSON(t, h, http.MethodPost, "/api/v1/series/watched", map[string]any{
		"library_id":    showsLib.ID,
		"title":         "Orbit Nine",
		"season_number": 1,
		"completed":     true,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series watched status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var bulkResp struct {
		Updated int `json:"updated"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &bulkResp)
	if bulkResp.Updated != 2 {
		t.Fatalf("expected 2 episodes updated for season 1, got %d", bulkResp.Updated)
	}

	detailURL := "/api/v1/series/detail?library_id=" + url.QueryEscape(showsLib.ID) + "&title=" + url.QueryEscape("Orbit Nine")
	rec = doJSON(t, h, http.MethodGet, detailURL, nil, adminCookie, "")
	var serDetail struct {
		WatchedCount         int                  `json:"watched_count"`
		RepresentativeItemID string               `json:"representative_item_id"`
		NextUp               *MediaItemSummaryDTO `json:"next_up"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &serDetail)
	if serDetail.WatchedCount != 2 || serDetail.RepresentativeItemID == "" {
		t.Fatalf("expected watched_count=2 and representative_item_id, got %+v", serDetail)
	}
	if serDetail.NextUp == nil || serDetail.NextUp.SeasonNumber == nil || *serDetail.NextUp.SeasonNumber != 2 {
		t.Fatalf("expected NextUp to advance to Season 2, got %+v", serDetail.NextUp)
	}

	rec = doJSON(t, h, http.MethodPost, "/api/v1/series/watched", map[string]any{
		"library_id": showsLib.ID,
		"title":      "Orbit Nine",
		"completed":  false,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("series unwatched status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, detailURL, nil, adminCookie, "")
	serDetail.WatchedCount = -1
	_ = json.Unmarshal(rec.Body.Bytes(), &serDetail)
	if serDetail.WatchedCount != 0 {
		t.Fatalf("expected watched_count=0 after bulk unwatched, got %d", serDetail.WatchedCount)
	}
}
