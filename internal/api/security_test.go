package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
)

func TestOpenAPIDisabledByDefault(t *testing.T) {
	t.Setenv("LUNE_ENABLE_API_DOCS", "")
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeLocal},
		Store:     db,
		StartedAt: time.Now(),
	})

	for _, p := range []string{"/openapi.json", "/docs"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want 404 when LUNE_ENABLE_API_DOCS is not true", p, rec.Code)
		}
	}
}

func TestCSRFMiddleware(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	h := NewHandler(Dependencies{
		Config: config.Config{
			EdgeMode:  config.EdgeModeProxy,
			PublicURL: "https://media.example.com",
		},
		Store:     db,
		StartedAt: time.Now(),
	})

	// Setup admin to obtain a session cookie and an app bearer token.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	webCookie := rec.Result().Cookies()[0]

	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "dan",
		"password":    "orbit-secret-99",
		"client_kind": "app",
	}, nil, "")
	var appLogin struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &appLogin)

	newPatch := func(origin, secFetchSite string, withCookie bool, bearer string) *httptest.ResponseRecorder {
		body := bytes.NewBufferString(`{"display_name":"Dan Updated"}`)
		req := httptest.NewRequest(http.MethodPatch, "http://localhost:5863/api/v1/users/me", body)
		req.Header.Set("Content-Type", "application/json")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if secFetchSite != "" {
			req.Header.Set("Sec-Fetch-Site", secFetchSite)
		}
		if withCookie {
			req.AddCookie(webCookie)
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	// Cross-site Sec-Fetch-Site rejected for cookie auth.
	if w := newPatch("", "cross-site", true, ""); w.Code != http.StatusForbidden {
		t.Fatalf("cross-site Sec-Fetch-Site status = %d, want 403", w.Code)
	}
	if w := newPatch("", "same-site", true, ""); w.Code != http.StatusForbidden {
		t.Fatalf("same-site Sec-Fetch-Site status = %d, want 403", w.Code)
	}

	// Mismatched Origin rejected for cookie auth.
	if w := newPatch("https://evil.example.net", "same-origin", true, ""); w.Code != http.StatusForbidden {
		t.Fatalf("evil Origin status = %d, want 403", w.Code)
	}

	// Matching Host Origin allowed.
	if w := newPatch("http://localhost:5863", "same-origin", true, ""); w.Code != http.StatusOK {
		t.Fatalf("same-origin Host status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Matching PublicURL Origin allowed.
	if w := newPatch("https://media.example.com", "same-origin", true, ""); w.Code != http.StatusOK {
		t.Fatalf("matching PublicURL Origin status = %d, want 200: %s", w.Code, w.Body.String())
	}

	// Bearer token bypasses cookie CSRF checks even with cross-site Origin.
	if w := newPatch("https://evil.example.net", "cross-site", true, appLogin.AccessToken); w.Code != http.StatusOK {
		t.Fatalf("bearer request status = %d, want 200: %s", w.Code, w.Body.String())
	}
}

func TestSecurityHardening_PasswordCapDeviceIntervalAvatarCSPAndPrune(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	limiter := security.NewAuthRateLimiterWithClock(time.Now, 10*time.Minute, 3, 30*time.Second, 2*time.Minute)
	authSvc := auth.NewService(db, limiter)
	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeLocal, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		StartedAt: time.Now(),
	})

	// Setup admin.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	var setupBody struct {
		User UserDTO `json:"user"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &setupBody)
	cookie := rec.Result().Cookies()[0]

	// H1: Overlong password (>256 bytes) rejected with 422 before Argon2id.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "dan",
		"password": strings.Repeat("x", 300),
	}, nil, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("overlong login password status = %d, want 422", rec.Code)
	}

	// H5: RFC 8628 minimum 5s poll interval on pending device code.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device", map[string]any{
		"client_name": "Living Room TV",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device start status = %d", rec.Code)
	}
	var devStart struct {
		DeviceCode string `json:"device_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &devStart)

	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/token", map[string]any{
		"device_code": devStart.DeviceCode,
	}, nil, "")
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("first pending poll status = %d, want 428", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/token", map[string]any{
		"device_code": devStart.DeviceCode,
	}, nil, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate second pending poll status = %d, want 429", rec.Code)
	}

	// L4: Avatar download serves Content-Security-Policy: sandbox.
	pngBytes := makeTestPNG(t)
	upReq := httptest.NewRequest(http.MethodPut, "/api/v1/users/me/avatar", bytes.NewReader(pngBytes))
	upReq.Header.Set("Content-Type", "image/png")
	upReq.AddCookie(cookie)
	upRec := httptest.NewRecorder()
	h.ServeHTTP(upRec, upReq)
	if upRec.Code != http.StatusOK {
		t.Fatalf("avatar upload status = %d, body = %s", upRec.Code, upRec.Body.String())
	}

	getReq := httptest.NewRequest(http.MethodGet, "/api/v1/users/"+setupBody.User.ID+"/avatar", nil)
	getReq.AddCookie(cookie)
	getRec := httptest.NewRecorder()
	h.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("avatar get status = %d", getRec.Code)
	}
	if csp := getRec.Header().Get("Content-Security-Policy"); csp != "sandbox" {
		t.Fatalf("avatar CSP = %q, want sandbox", csp)
	}

	// H5: PruneExpiredAuthState deletes expired device codes without error.
	if err := db.CreateDeviceCode(ctx, store.DeviceCode{
		DeviceCodeHash: "expired-hash-test",
		UserCode:       "AAAA-BBBB",
		ClientName:     "Old TV",
		ClientIP:       "127.0.0.1",
		ExpiresAt:      time.Now().UTC().Add(-time.Hour),
	}); err != nil {
		t.Fatalf("CreateDeviceCode: %v", err)
	}
	if err := db.PruneExpiredAuthState(ctx); err != nil {
		t.Fatalf("PruneExpiredAuthState: %v", err)
	}
	if _, err := db.GetDeviceCodeByHash(ctx, "expired-hash-test"); err != store.ErrNotFound {
		t.Fatalf("expected expired device code pruned, got err=%v", err)
	}
}
