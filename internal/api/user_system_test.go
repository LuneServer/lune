package api

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
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

func makeTestPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			img.Set(x, y, color.RGBA{R: 100, G: 140, B: 220, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

func setupUserSystemFixture(t *testing.T) (http.Handler, *http.Cookie, string, string, string, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	// Create two libraries: Movies (family) and Shows (restricted).
	moviesRoot := t.TempDir()
	movieDir := filepath.Join(moviesRoot, "Arrival (2016)")
	_ = os.MkdirAll(movieDir, 0o755)
	_ = os.WriteFile(filepath.Join(movieDir, "Arrival (2016).mp4"), []byte("movie-bytes"), 0o644)
	_ = os.WriteFile(filepath.Join(movieDir, "poster.jpg"), []byte("movie-poster"), 0o644)

	showsRoot := t.TempDir()
	epDir := filepath.Join(showsRoot, "Dark Matter (2024)", "Season 01")
	_ = os.MkdirAll(epDir, 0o755)
	_ = os.WriteFile(filepath.Join(showsRoot, "Dark Matter (2024)", "poster.jpg"), []byte("show-poster"), 0o644)
	_ = os.WriteFile(filepath.Join(epDir, "Dark.Matter.S01E01.Pilot.mp4"), []byte("ep1-bytes"), 0o644)
	_ = os.WriteFile(filepath.Join(epDir, "Dark.Matter.S01E02.Second.mp4"), []byte("ep2-bytes"), 0o644)

	authSvc := auth.NewService(db, nil)
	libSvc := library.NewService(db, stubProber{}, nil)
	streamSvc, err := stream.NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("stream.NewService: %v", err)
	}
	t.Cleanup(streamSvc.Shutdown)

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		Library:   libSvc,
		Stream:    streamSvc,
		StartedAt: time.Now(),
	})

	// Setup initial admin.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username":     "dan",
		"display_name": "Dan",
		"password":     "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	adminCookie := rec.Result().Cookies()[0]

	// Create & scan both libraries.
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

	// Fetch movie item ID and episode 1 ID.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?library_id="+moviesLib.ID, nil, adminCookie, "")
	var mList struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &mList)
	if len(mList.Items) != 1 {
		t.Fatalf("expected 1 movie item, got %d", len(mList.Items))
	}
	movieItemID := mList.Items[0].ID

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?library_id="+showsLib.ID, nil, adminCookie, "")
	var sList struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sList)
	if len(sList.Items) != 2 {
		t.Fatalf("expected 2 episode items, got %d", len(sList.Items))
	}
	ep1ID := sList.Items[0].ID

	return h, adminCookie, moviesLib.ID, showsLib.ID, movieItemID, ep1ID
}

func TestUserSystemSelfServiceProfilePasswordAvatarAndSessions(t *testing.T) {
	h, adminCookie, _, _, _, _ := setupUserSystemFixture(t)

	// Create a second session (app) so we can verify password change & revoke-others.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "dan",
		"password":    "orbit-secret-99",
		"client_kind": "app",
		"client_name": "Lune iPad",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("second login status = %d", rec.Code)
	}
	var secondLogin struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &secondLogin)

	// 1. GET /api/v1/users/me shows defaults, active_sessions == 2, last_login_at set.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /users/me status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var prof UserProfileDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &prof)
	if prof.Username != "dan" || !prof.AllLibraries || !prof.Preferences.AutoplayNext || prof.ActiveSessions != 2 || prof.LastLoginAt == nil {
		t.Fatalf("unexpected profile: %+v", prof)
	}
	if p := prof.Preferences; p.AutoSkipIntro != "countdown" || p.AutoSkipRecap != "countdown" || p.AutoSkipCredits != "off" {
		t.Fatalf("unexpected default auto-skip preferences: %+v", p)
	}

	// 2. PATCH /api/v1/users/me updates display_name & preferences.
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"display_name": "Captain Dan",
		"preferences": map[string]any{
			"audio_language":    "eng",
			"subtitle_language": "en",
			"subtitles_enabled": true,
			"autoplay_next":     false,
			"max_bitrate":       8_000_000,
		},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH /users/me status = %d, body = %s", rec.Code, rec.Body.String())
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &prof)
	if prof.DisplayName != "Captain Dan" || prof.Preferences.AudioLanguage != "eng" || prof.Preferences.AutoplayNext || prof.Preferences.MaxBitrate != 8_000_000 {
		t.Fatalf("unexpected updated profile: %+v", prof)
	}

	// 2b. show_chapter_markers round-trips through PATCH and GET.
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"preferences": map[string]any{"autoplay_next": true, "subtitles_enabled": false, "show_chapter_markers": false},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH show_chapter_markers status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, adminCookie, "")
	prof = UserProfileDTO{}
	_ = json.Unmarshal(rec.Body.Bytes(), &prof)
	if prof.Preferences.ShowChapterMarkers {
		t.Fatalf("show_chapter_markers = true after disabling: %+v", prof.Preferences)
	}
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"preferences": map[string]any{"autoplay_next": true, "subtitles_enabled": false, "show_chapter_markers": true},
	}, adminCookie, "")
	prof = UserProfileDTO{}
	_ = json.Unmarshal(rec.Body.Bytes(), &prof)
	if !prof.Preferences.ShowChapterMarkers {
		t.Fatalf("show_chapter_markers = false after re-enabling: %+v", prof.Preferences)
	}

	// 2c. auto_skip_* round-trip; omitted modes fall back to defaults; unknown modes are rejected.
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"preferences": map[string]any{"autoplay_next": true, "subtitles_enabled": false, "show_chapter_markers": true,
			"auto_skip_intro": "instant", "auto_skip_credits": "countdown"},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH auto_skip status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, adminCookie, "")
	prof = UserProfileDTO{}
	_ = json.Unmarshal(rec.Body.Bytes(), &prof)
	if p := prof.Preferences; p.AutoSkipIntro != "instant" || p.AutoSkipRecap != "countdown" || p.AutoSkipCredits != "countdown" {
		t.Fatalf("auto-skip preferences after PATCH = %+v", p)
	}
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{
		"preferences": map[string]any{"autoplay_next": true, "subtitles_enabled": false, "auto_skip_intro": "sometimes"},
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH invalid auto_skip_intro status = %d, want 422", rec.Code)
	}

	// 3. Avatar upload (rejects invalid MIME, accepts PNG via multipart, serves image, deletes).
	badReq := httptest.NewRequest(http.MethodPut, "/api/v1/users/me/avatar", bytes.NewReader([]byte("not-an-image")))
	badReq.AddCookie(adminCookie)
	badRec := httptest.NewRecorder()
	h.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid avatar status = %d, want 422", badRec.Code)
	}

	pngBytes := makeTestPNG(t)
	var mpBody bytes.Buffer
	mw := multipart.NewWriter(&mpBody)
	fw, _ := mw.CreateFormFile("avatar", "me.png")
	_, _ = fw.Write(pngBytes)
	_ = mw.Close()

	upReq := httptest.NewRequest(http.MethodPut, "/api/v1/users/me/avatar", &mpBody)
	upReq.Header.Set("Content-Type", mw.FormDataContentType())
	upReq.AddCookie(adminCookie)
	upRec := httptest.NewRecorder()
	h.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("avatar upload status = %d, body = %s", upRec.Code, upRec.Body.String())
	}
	_ = json.Unmarshal(upRec.Body.Bytes(), &prof)
	if prof.AvatarURL == "" {
		t.Fatalf("expected avatar_url to be populated, got %+v", prof)
	}

	// Fetch avatar image while authenticated (and 401 while unauthenticated).
	unauthRec := httptest.NewRecorder()
	h.ServeHTTP(unauthRec, httptest.NewRequest(http.MethodGet, prof.AvatarURL, nil))
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated avatar fetch = %d, want 401", unauthRec.Code)
	}

	avReq := httptest.NewRequest(http.MethodGet, prof.AvatarURL, nil)
	avReq.AddCookie(adminCookie)
	avRec := httptest.NewRecorder()
	h.ServeHTTP(avRec, avReq)
	if avRec.Code != http.StatusOK || !bytes.Equal(avRec.Body.Bytes(), pngBytes) {
		t.Fatalf("avatar download status = %d, len = %d", avRec.Code, avRec.Body.Len())
	}

	// Delete avatar -> avatar_url cleared and GET returns 404.
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/users/me/avatar", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("delete avatar status = %d", rec.Code)
	}
	avRec2 := httptest.NewRecorder()
	avReq2 := httptest.NewRequest(http.MethodGet, prof.AvatarURL, nil)
	avReq2.AddCookie(adminCookie)
	h.ServeHTTP(avRec2, avReq2)
	if avRec2.Code != http.StatusNotFound {
		t.Fatalf("deleted avatar status = %d, want 404", avRec2.Code)
	}

	// 4. Change own password: wrong current password rejected (401), valid change revokes other sessions while keeping current alive.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/users/me/password", map[string]any{
		"current_password": "wrong-password",
		"new_password":     "new-orbit-secret-100",
	}, adminCookie, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong current password status = %d, want 401", rec.Code)
	}

	rec = doJSON(t, h, http.MethodPost, "/api/v1/users/me/password", map[string]any{
		"current_password": "orbit-secret-99",
		"new_password":     "new-orbit-secret-100",
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("change password status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Current cookie still works; secondLogin bearer token is revoked.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("current session should remain active after own password change, got %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, nil, secondLogin.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("other session should be revoked after own password change, got %d", rec.Code)
	}

	// 5. Revoke other sessions endpoint works explicitly too.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "dan",
		"password":    "new-orbit-secret-100",
		"client_kind": "app",
		"client_name": "Lune TV",
	}, nil, "")
	var tvLogin struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &tvLogin)

	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/sessions/revoke-others", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("revoke-others status = %d", rec.Code)
	}
	var revResp struct {
		RevokedCount int64 `json:"revoked_count"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &revResp)
	if revResp.RevokedCount != 1 {
		t.Fatalf("expected 1 revoked session, got %d", revResp.RevokedCount)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, nil, tvLogin.AccessToken)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("revoked TV session should return 401, got %d", rec.Code)
	}
}

func TestUserSystemLibraryACLsWatchlistAndFavorites(t *testing.T) {
	h, adminCookie, moviesLibID, showsLibID, movieItemID, ep1ID := setupUserSystemFixture(t)

	// Create a restricted member who ONLY has access to Movies (not Shows).
	allFalse := false
	rec := doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":      "maya",
		"display_name":  "Maya",
		"password":      "maya-secret-99",
		"role":          "member",
		"all_libraries": allFalse,
		"library_ids":   []string{moviesLibID},
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create restricted member status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var maya AdminUserDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &maya)
	if maya.AllLibraries || len(maya.LibraryIDs) != 1 || maya.LibraryIDs[0] != moviesLibID {
		t.Fatalf("unexpected restricted user: %+v", maya)
	}

	// Unknown library_id rejected with 422.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":      "badlib",
		"password":      "badlib-secret-99",
		"all_libraries": false,
		"library_ids":   []string{"nonexistent-lib"},
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown library_id status = %d, want 422", rec.Code)
	}

	// Login as Maya.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "maya",
		"password": "maya-secret-99",
	}, nil, "")
	mayaCookie := rec.Result().Cookies()[0]

	// 1. /api/v1/libraries only lists Movies.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/libraries", nil, mayaCookie, "")
	var libsResp struct {
		Libraries []LibraryDTO `json:"libraries"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &libsResp)
	if len(libsResp.Libraries) != 1 || libsResp.Libraries[0].ID != moviesLibID {
		t.Fatalf("expected only Movies library for maya, got %+v", libsResp.Libraries)
	}

	// 2. /api/v1/items returns only the Movie; explicitly filtering by Shows returns 403.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items", nil, mayaCookie, "")
	var itemsResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
		Total int                   `json:"total"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	if itemsResp.Total != 1 || itemsResp.Items[0].ID != movieItemID {
		t.Fatalf("expected only movie item for maya, got %+v", itemsResp)
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items?library_id="+showsLibID, nil, mayaCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("explicit forbidden library items status = %d, want 403", rec.Code)
	}

	// 3. Direct item detail, artwork, series, next-episode, playback, and progress on restricted episode all return 403 (or empty for /series).
	for _, tc := range []struct {
		method string
		path   string
		body   any
		want   int
	}{
		{http.MethodGet, "/api/v1/items/" + ep1ID, nil, http.StatusForbidden},
		{http.MethodGet, "/api/v1/items/" + ep1ID + "/artwork/poster", nil, http.StatusForbidden},
		{http.MethodGet, "/api/v1/items/" + ep1ID + "/next", nil, http.StatusForbidden},
		{http.MethodGet, "/api/v1/libraries/" + showsLibID + "/scan", nil, http.StatusForbidden},
		{http.MethodGet, "/api/v1/series?library_id=" + showsLibID, nil, http.StatusForbidden},
		{http.MethodGet, "/api/v1/series/detail?library_id=" + showsLibID + "&title=" + url.QueryEscape("Dark Matter"), nil, http.StatusForbidden},
		{http.MethodPost, "/api/v1/items/" + ep1ID + "/playback", map[string]any{}, http.StatusForbidden},
		{http.MethodPut, "/api/v1/items/" + ep1ID + "/progress", map[string]any{"position_ms": 1000}, http.StatusForbidden},
		{http.MethodGet, "/api/v1/items/" + ep1ID + "/progress", nil, http.StatusForbidden},
		{http.MethodPut, "/api/v1/users/me/watchlist/" + ep1ID, nil, http.StatusForbidden},
		{http.MethodPut, "/api/v1/users/me/favorites/" + ep1ID, nil, http.StatusForbidden},
	} {
		rec = doJSON(t, h, tc.method, tc.path, tc.body, mayaCookie, "")
		if rec.Code != tc.want {
			t.Fatalf("%s %s status = %d, want %d (body=%s)", tc.method, tc.path, rec.Code, tc.want, rec.Body.String())
		}
	}

	// Unfiltered /api/v1/series returns empty list for maya.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/series", nil, mayaCookie, "")
	var serResp struct {
		Series []SeriesSummaryDTO `json:"series"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &serResp)
	if len(serResp.Series) != 0 {
		t.Fatalf("expected 0 series for maya, got %d", len(serResp.Series))
	}

	// 4. Maya CAN watch, progress, watchlist, and favorite the allowed Movie.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+movieItemID+"/playback", map[string]any{}, mayaCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed playback status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/items/"+movieItemID+"/progress", map[string]any{"position_ms": 120000}, mayaCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("allowed progress status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/users/me/watchlist/"+movieItemID, nil, mayaCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add watchlist status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/users/me/favorites/"+movieItemID, nil, mayaCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("add favorite status = %d", rec.Code)
	}

	// Item detail reflects in_watchlist & is_favorite for Maya, but NOT for Admin.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+movieItemID, nil, mayaCookie, "")
	var detail MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if !detail.InWatchlist || !detail.IsFavorite {
		t.Fatalf("expected in_watchlist=true is_favorite=true for maya, got %+v", detail)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/items/"+movieItemID, nil, adminCookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &detail)
	if detail.InWatchlist || detail.IsFavorite {
		t.Fatalf("expected in_watchlist=false is_favorite=false for admin, got %+v", detail)
	}

	// Watchlist & favorites listing return the item.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me/watchlist", nil, mayaCookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	if itemsResp.Total != 1 || itemsResp.Items[0].ID != movieItemID {
		t.Fatalf("unexpected watchlist: %+v", itemsResp)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me/favorites", nil, mayaCookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	if itemsResp.Total != 1 || itemsResp.Items[0].ID != movieItemID {
		t.Fatalf("unexpected favorites: %+v", itemsResp)
	}

	// 5. Revoking Maya's access to Movies immediately hides it from Continue Watching & Watchlist!
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/admin/users/"+maya.ID, map[string]any{
		"all_libraries": false,
		"library_ids":   []string{showsLibID},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("update maya libraries status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/playback/continue", nil, mayaCookie, "")
	var cwResp struct {
		Items []ContinueWatchingItemDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &cwResp)
	if len(cwResp.Items) != 0 {
		t.Fatalf("expected continue watching to hide revoked library item, got %d", len(cwResp.Items))
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me/watchlist", nil, mayaCookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &itemsResp)
	if itemsResp.Total != 0 {
		t.Fatalf("expected watchlist to hide revoked library item, got %d", itemsResp.Total)
	}

	// And now Maya CAN see Dark Matter series!
	rec = doJSON(t, h, http.MethodGet, "/api/v1/series", nil, mayaCookie, "")
	_ = json.Unmarshal(rec.Body.Bytes(), &serResp)
	if len(serResp.Series) != 1 || serResp.Series[0].Title != "Dark Matter" {
		t.Fatalf("expected Dark Matter series for maya after grant, got %+v", serResp.Series)
	}

	// Remove from favorites (204) and delete watchlist entry (204).
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/users/me/favorites/"+movieItemID, nil, mayaCookie, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("remove favorite status = %d, want 204", rec.Code)
	}
}

func TestUserSystemInvitesAndAdminManagement(t *testing.T) {
	h, adminCookie, moviesLibID, _, _, _ := setupUserSystemFixture(t)

	// 1. Admin creates a single-use invite restricted to Movies.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/admin/invites", map[string]any{
		"label":            "For Sam",
		"role":             "member",
		"all_libraries":    false,
		"library_ids":      []string{moviesLibID},
		"max_uses":         1,
		"expires_in_hours": 48,
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create invite status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var invCreated struct {
		Invite     InviteDTO `json:"invite"`
		Token      string    `json:"token"`
		InvitePath string    `json:"invite_path"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &invCreated)
	if invCreated.Token == "" || invCreated.Invite.Status != "active" || invCreated.Invite.AllLibraries {
		t.Fatalf("unexpected created invite: %+v", invCreated)
	}

	// 2. Public inspect returns valid metadata.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/invites/"+invCreated.Token, nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("inspect invite status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var inspectResp struct {
		Valid         bool   `json:"valid"`
		Label         string `json:"label"`
		Role          string `json:"role"`
		RemainingUses int    `json:"remaining_uses"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &inspectResp)
	if !inspectResp.Valid || inspectResp.Label != "For Sam" || inspectResp.RemainingUses != 1 {
		t.Fatalf("unexpected inspect response: %+v", inspectResp)
	}

	// 3. Attempting to accept with an existing username ("dan") fails with 409 and does NOT consume the invite!
	rec = doJSON(t, h, http.MethodPost, "/api/v1/invites/"+invCreated.Token+"/accept", map[string]any{
		"username": "dan",
		"password": "sam-secret-99",
	}, nil, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username invite accept status = %d, want 409", rec.Code)
	}

	// 4. Accepting with a fresh username ("sam") succeeds, sets cookie, grants only Movies, and exhausts the invite.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/invites/"+invCreated.Token+"/accept", map[string]any{
		"username":     "sam",
		"display_name": "Sam",
		"password":     "sam-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("accept invite status = %d, body = %s", rec.Code, rec.Body.String())
	}
	samCookie := rec.Result().Cookies()[0]
	var samAuth struct {
		User UserDTO `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &samAuth)
	if samAuth.User.Username != "sam" || samAuth.User.Role != "member" || samAuth.User.AllLibraries {
		t.Fatalf("unexpected sam auth response: %+v", samAuth)
	}

	// Exhausted invite returns 410 Gone on inspect and accept.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/invites/"+invCreated.Token, nil, nil, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("exhausted invite inspect status = %d, want 410", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/invites/"+invCreated.Token+"/accept", map[string]any{
		"username": "latecomer",
		"password": "late-secret-99",
	}, nil, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("exhausted invite accept status = %d, want 410", rec.Code)
	}

	// 5. Admin creates a second invite and revokes it -> 410 Gone on inspect.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/invites", map[string]any{
		"label": "Temp Invite",
	}, adminCookie, "")
	var inv2 struct {
		Invite InviteDTO `json:"invite"`
		Token  string    `json:"token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &inv2)
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/admin/invites/"+inv2.Invite.ID, nil, adminCookie, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("revoke invite status = %d, want 204", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/invites/"+inv2.Token, nil, nil, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("revoked invite inspect status = %d, want 410", rec.Code)
	}

	// Admin lists invites -> sees exhausted and revoked statuses.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/invites", nil, adminCookie, "")
	var invList struct {
		Invites []InviteDTO `json:"invites"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &invList)
	if len(invList.Invites) != 2 {
		t.Fatalf("expected 2 invites in admin list, got %d", len(invList.Invites))
	}

	// 6. Admin promotes Sam to admin -> automatically gets all_libraries=true, can now view /admin/users!
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/admin/users/"+samAuth.User.ID, map[string]any{
		"display_name": "Admin Sam",
		"role":         "admin",
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("promote sam status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var samAdmin AdminUserDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &samAdmin)
	if samAdmin.Role != "admin" || !samAdmin.AllLibraries || samAdmin.DisplayName != "Admin Sam" {
		t.Fatalf("unexpected promoted user: %+v", samAdmin)
	}

	// Sam cannot self-demote (422).
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/admin/users/"+samAuth.User.ID, map[string]any{
		"role": "member",
	}, samCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("self-demote status = %d, want 422", rec.Code)
	}

	// 7. Admin inspects Sam's sessions and revokes all of them.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/users/"+samAuth.User.ID+"/sessions", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin list user sessions status = %d", rec.Code)
	}
	var sessResp struct {
		Sessions []SessionDTO `json:"sessions"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &sessResp)
	if len(sessResp.Sessions) != 1 {
		t.Fatalf("expected 1 active session for sam, got %d", len(sessResp.Sessions))
	}

	rec = doJSON(t, h, http.MethodDelete, "/api/v1/admin/users/"+samAuth.User.ID+"/sessions", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin revoke user sessions status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, samCookie, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sam session should be revoked, got %d", rec.Code)
	}

	// 8. Admin GET /api/v1/admin/users/{id} returns single user detail.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/users/"+samAuth.User.ID, nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin get user status = %d", rec.Code)
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &samAdmin)
	if samAdmin.ActiveSessions != 0 {
		t.Fatalf("expected 0 active sessions after admin revoke, got %d", samAdmin.ActiveSessions)
	}

	// 9. Admin can dismiss and list dismissed console system alerts.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/alerts/dismissed", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("list dismissed alerts status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/alerts/dismiss", map[string]any{
		"ids": []string{"audit-1", "scan-err-lib1"},
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dismiss alerts status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var disResp struct {
		DismissedIDs []string `json:"dismissed_ids"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &disResp)
	if len(disResp.DismissedIDs) != 2 {
		t.Fatalf("expected 2 dismissed alert ids, got %+v", disResp.DismissedIDs)
	}
}

func TestUserPreferencesSubtitleStyleSyncAndPreserve(t *testing.T) {
	h, adminCookie, _, _, _, _ := setupUserSystemFixture(t)

	patch := func(prefs map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		prefs["subtitles_enabled"] = false
		if _, ok := prefs["autoplay_next"]; !ok {
			prefs["autoplay_next"] = true
		}
		return doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{"preferences": prefs}, adminCookie, "")
	}
	readStyle := func() *store.SubtitleStyle {
		t.Helper()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, adminCookie, "")
		var prof UserProfileDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &prof); err != nil {
			t.Fatal(err)
		}
		return prof.Preferences.SubtitleStyle
	}

	if st := readStyle(); st != nil {
		t.Fatalf("expected no stored subtitle style by default, got %+v", st)
	}

	rec := patch(map[string]any{
		"autoplay_next": true,
		"subtitle_style": map[string]any{
			"size": "large", "color": "yellow", "background": false, "outline": true,
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	want := store.SubtitleStyle{Size: "large", Color: "yellow", Background: false, Outline: true}
	if st := readStyle(); st == nil || *st != want {
		t.Fatalf("subtitle style = %+v, want %+v", st, want)
	}

	// A client that doesn't know subtitle_style must not erase it.
	if rec := patch(map[string]any{"autoplay_next": false}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH (legacy client) status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if st := readStyle(); st == nil || *st != want {
		t.Fatalf("subtitle style after legacy update = %+v, want %+v", st, want)
	}

	// Unknown values are rejected.
	rec = patch(map[string]any{
		"subtitle_style": map[string]any{"size": "huge", "color": "white", "background": true, "outline": false},
	})
	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("expected 4xx for invalid size, got %d", rec.Code)
	}
}

func TestUserPreferencesAnimeAudioSyncAndPreserve(t *testing.T) {
	h, adminCookie, _, _, _, _ := setupUserSystemFixture(t)

	patch := func(prefs map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		prefs["subtitles_enabled"] = false
		prefs["autoplay_next"] = true
		return doJSON(t, h, http.MethodPatch, "/api/v1/users/me", map[string]any{"preferences": prefs}, adminCookie, "")
	}
	read := func() string {
		t.Helper()
		rec := doJSON(t, h, http.MethodGet, "/api/v1/users/me", nil, adminCookie, "")
		var prof UserProfileDTO
		if err := json.Unmarshal(rec.Body.Bytes(), &prof); err != nil {
			t.Fatal(err)
		}
		return prof.Preferences.AnimeAudio
	}

	if got := read(); got != "" {
		t.Fatalf("expected no anime_audio by default, got %q", got)
	}
	if rec := patch(map[string]any{"anime_audio": "subbed"}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := read(); got != "subbed" {
		t.Fatalf("anime_audio = %q, want subbed", got)
	}
	// Older clients that omit the field keep it.
	if rec := patch(map[string]any{}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH (legacy client) status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := read(); got != "subbed" {
		t.Fatalf("anime_audio after legacy update = %q, want subbed", got)
	}
	// "auto" clears it.
	if rec := patch(map[string]any{"anime_audio": "auto"}); rec.Code != http.StatusOK {
		t.Fatalf("PATCH auto status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := read(); got != "" {
		t.Fatalf("anime_audio after auto = %q, want empty", got)
	}
	if rec := patch(map[string]any{"anime_audio": "both"}); rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("expected 4xx for unknown anime_audio, got %d", rec.Code)
	}
}
