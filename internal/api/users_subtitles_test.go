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
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

func TestSubtitlesConversionRefreshAndMultiUserAdmin(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	movieDir := filepath.Join(mediaRoot, "Dune (2021)")
	_ = os.MkdirAll(movieDir, 0o755)
	_ = os.WriteFile(filepath.Join(movieDir, "Dune (2021).mp4"), []byte("mp4-data"), 0o644)
	srtContent := "1\r\n00:00:01,250 --> 00:00:04,500\r\nDreams are messages from the deep.\r\n"
	_ = os.WriteFile(filepath.Join(movieDir, "Dune (2021).en.srt"), []byte(srtContent), 0o644)

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

	// Setup admin
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	adminCookie := rec.Result().Cookies()[0]
	var setupResp struct {
		User UserDTO `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &setupResp)

	// 1. Scan movie + verify sidecar .srt -> WebVTT stream
	rec = doJSON(t, h, http.MethodPost, "/api/v1/libraries", map[string]any{
		"name":      "Movies",
		"kind":      "movies",
		"root_path": mediaRoot,
	}, adminCookie, "")
	var libResp LibraryDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &libResp)
	doJSON(t, h, http.MethodPost, "/api/v1/libraries/"+libResp.ID+"/scan?sync=true", nil, adminCookie, "")

	rec = doJSON(t, h, http.MethodGet, "/api/v1/items", nil, adminCookie, "")
	var listResp struct {
		Items []MediaItemSummaryDTO `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &listResp)
	itemID := listResp.Items[0].ID

	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/playback", map[string]any{}, adminCookie, "")
	var plan stream.PlaybackPlan
	_ = json.Unmarshal(rec.Body.Bytes(), &plan)
	if len(plan.Subtitles) != 1 || plan.Subtitles[0].Language != "en" || plan.Subtitles[0].Label != "English" {
		t.Fatalf("expected 1 English subtitle track, got %+v", plan.Subtitles)
	}

	subRec := httptest.NewRecorder()
	h.ServeHTTP(subRec, httptest.NewRequest(http.MethodGet, plan.Subtitles[0].URL, nil))
	if subRec.Code != http.StatusOK {
		t.Fatalf("subtitle fetch status = %d", subRec.Code)
	}
	body := subRec.Body.String()
	if !strings.HasPrefix(body, "WEBVTT\n\n") || !strings.Contains(body, "00:00:01.250 --> 00:00:04.500") {
		t.Fatalf("unexpected WebVTT conversion output: %q", body)
	}

	// 2. Manual single-item refresh / re-match
	rec = doJSON(t, h, http.MethodPost, "/api/v1/items/"+itemID+"/refresh", map[string]any{
		"title": "Dune: Part One",
		"year":  2021,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var refreshed MediaItemDetailDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &refreshed)
	if refreshed.Title != "Dune: Part One" {
		t.Fatalf("expected updated title, got %q", refreshed.Title)
	}

	// 3. Multi-user admin CRUD + role isolation + session revocation on disable
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username":     "alex",
		"display_name": "Alex",
		"password":     "member-secret-99",
		"role":         "member",
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var member AdminUserDTO
	_ = json.Unmarshal(rec.Body.Bytes(), &member)

	// Member logs in and can browse items, but is forbidden from /api/v1/admin/users.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "alex",
		"password": "member-secret-99",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member login status = %d", rec.Code)
	}
	memberCookie := rec.Result().Cookies()[0]

	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/users", nil, memberCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member admin/users status = %d, want 403", rec.Code)
	}

	// Admin disables member -> member session is immediately revoked (401).
	disabled := true
	rec = doJSON(t, h, http.MethodPatch, "/api/v1/admin/users/"+member.ID, map[string]any{
		"disabled": disabled,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("disable member status = %d", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, memberCookie, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("disabled member auth/me status = %d, want 401", rec.Code)
	}

	// Admin cannot delete themselves (422).
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/admin/users/"+setupResp.User.ID, nil, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("self-delete status = %d, want 422", rec.Code)
	}

	// Admin can delete member (204).
	rec = doJSON(t, h, http.MethodDelete, "/api/v1/admin/users/"+member.ID, nil, adminCookie, "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete member status = %d, want 204", rec.Code)
	}
}
