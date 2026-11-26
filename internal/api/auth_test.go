package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
)

func TestAuthFlowsEndToEnd(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	limiter := security.NewAuthRateLimiterWithClock(time.Now, 10*time.Minute, 3, 30*time.Second, 2*time.Minute)
	authSvc := auth.NewService(db, limiter)

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev},
		Store:     db,
		Auth:      authSvc,
		StartedAt: time.Now(),
	})

	// 1. First-run setup creates admin and sets HttpOnly cookie.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username":     "dan",
		"display_name": "Dan",
		"password":     "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 || cookies[0].Name != sessionCookieName || !cookies[0].HttpOnly {
		t.Fatalf("expected HttpOnly %s cookie, got %+v", sessionCookieName, cookies)
	}
	webCookie := cookies[0]

	// 2. Second setup attempt is rejected with 409 Conflict.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "attacker",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("second setup status = %d, want 409", rec.Code)
	}

	// 3. /api/v1/auth/me authenticates via web cookie.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, webCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me (cookie) status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// 4. App login returns access + refresh tokens, and refresh rotates them.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username":    "dan",
		"password":    "orbit-secret-99",
		"client_kind": "app",
		"client_name": "Lune iOS",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("app login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var appLogin struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &appLogin); err != nil {
		t.Fatalf("decode app login: %v", err)
	}
	if appLogin.AccessToken == "" || appLogin.RefreshToken == "" {
		t.Fatalf("expected both access and refresh tokens, got %+v", appLogin)
	}

	// Bearer auth works on /api/v1/auth/me.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/auth/me", nil, nil, appLogin.AccessToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("auth/me (bearer) status = %d", rec.Code)
	}

	// Refresh rotates tokens and invalidates old refresh token.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh", map[string]any{
		"refresh_token": appLogin.RefreshToken,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/refresh", map[string]any{
		"refresh_token": appLogin.RefreshToken,
	}, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("reused refresh token status = %d, want 401", rec.Code)
	}

	// 5. RFC 8628 Device Flow (TV app -> browser approval -> token exchange).
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device", map[string]any{
		"client_name": "Apple TV — Lounge",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device start status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var devStart struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &devStart)

	// Polling before approval returns 428 authorization_pending.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/token", map[string]any{
		"device_code": devStart.DeviceCode,
	}, nil, "")
	if rec.Code != http.StatusPreconditionRequired {
		t.Fatalf("pending poll status = %d, want 428", rec.Code)
	}

	// Browser user approves user_code.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/approve", map[string]any{
		"user_code": devStart.UserCode,
	}, webCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("device approve status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// TV polls and receives access + refresh tokens.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/token", map[string]any{
		"device_code": devStart.DeviceCode,
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("approved poll status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Re-polling consumed device_code fails with 410 Gone.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/device/token", map[string]any{
		"device_code": devStart.DeviceCode,
	}, nil, "")
	if rec.Code != http.StatusGone {
		t.Fatalf("consumed poll status = %d, want 410", rec.Code)
	}

	// 6. Rate limiting locks out repeated failed logins with 429 + Retry-After.
	for i := 0; i < 3; i++ {
		rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
			"username": "dan",
			"password": "wrong-password",
		}, nil, "")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("failed login %d status = %d, want 401", i+1, rec.Code)
		}
	}
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "dan",
		"password": "orbit-secret-99",
	}, nil, "")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("rate-limited login status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("expected Retry-After header on 429 response")
	}

	// 7. Admin audit log lists recorded events.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/audit", nil, webCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin audit status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var auditResp struct {
		Events []AuditEventDTO `json:"events"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &auditResp)
	if len(auditResp.Events) < 4 {
		t.Fatalf("expected audit trail entries, got %d", len(auditResp.Events))
	}
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any, cookie *http.Cookie, bearer string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode json: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.RemoteAddr = "198.51.100.77:55555"
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
