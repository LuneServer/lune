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
	"github.com/LuneServer/lune/internal/store"
)

func TestAdminNetworkConfigLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewService(db, nil)
	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeLocal},
		Store:     db,
		Auth:      authSvc,
		StartedAt: time.Now(),
	})

	// Bootstrap admin account.
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "admin",
		"password": "admin-password-123",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d, body = %s", rec.Code, rec.Body.String())
	}
	adminCookie := rec.Result().Cookies()[0]

	// Create a non-admin member via admin API.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/users", map[string]any{
		"username": "member",
		"password": "member-password-123",
		"role":     "member",
	}, adminCookie, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("create member status = %d, body = %s", rec.Code, rec.Body.String())
	}

	// Member login.
	rec = doJSON(t, h, http.MethodPost, "/api/v1/auth/login", map[string]any{
		"username": "member",
		"password": "member-password-123",
	}, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("member login status = %d, body = %s", rec.Code, rec.Body.String())
	}
	memberCookie := rec.Result().Cookies()[0]

	// 1. Unauthenticated -> 401; Member -> 403 on GET and PUT.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/network/config", nil, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth GET status = %d, want 401", rec.Code)
	}
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/network/config", nil, memberCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member GET status = %d, want 403", rec.Code)
	}
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode": "proxy",
	}, memberCookie, "")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("member PUT status = %d, want 403", rec.Code)
	}

	// 2. Initial GET as admin reflects local mode.
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/network/config", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("admin GET status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var netCfg config.NetworkConfig
	if err := json.Unmarshal(rec.Body.Bytes(), &netCfg); err != nil {
		t.Fatalf("decode network config: %v", err)
	}
	if netCfg.EdgeMode != config.EdgeModeLocal || netCfg.ActiveEdgeMode != config.EdgeModeLocal || netCfg.RestartRequired {
		t.Fatalf("unexpected initial network config: %+v", netCfg)
	}

	// 3. Before switching to proxy, X-Forwarded-Proto: https from a Docker bridge IP is ignored (cookie not Secure).
	loginOverProxy := func() *http.Cookie {
		body, _ := json.Marshal(map[string]any{
			"username": "admin",
			"password": "admin-password-123",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "172.18.0.5:43210"
		req.Header.Set("X-Forwarded-Proto", "https")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("loginOverProxy status = %d, body = %s", w.Code, w.Body.String())
		}
		cookies := w.Result().Cookies()
		if len(cookies) == 0 {
			t.Fatal("expected session cookie")
		}
		return cookies[0]
	}

	if c := loginOverProxy(); c.Secure {
		t.Fatal("expected Secure=false in local mode when X-Forwarded-Proto is spoofed")
	}

	// 4. Live switch local -> proxy (with blank public_url first to verify X-Forwarded-Proto hot-reload).
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode": "proxy",
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT proxy status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &netCfg); err != nil {
		t.Fatalf("decode proxy response: %v", err)
	}
	if netCfg.EdgeMode != config.EdgeModeProxy || netCfg.RestartRequired {
		t.Fatalf("expected live switch to proxy with restart_required=false, got %+v", netCfg)
	}

	// Immediately after live switch, X-Forwarded-Proto: https from 172.18.0.5 yields Secure=true.
	if c := loginOverProxy(); !c.Secure {
		t.Fatal("expected Secure=true immediately after switching to proxy mode")
	}

	// Update public_url in proxy mode and verify /api/v1/system/info reflects live edge_mode + public_url.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":  "proxy",
		"public_url": "https://media.example.com/",
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT proxy public_url status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = doJSON(t, h, http.MethodGet, "/api/v1/system/info", nil, nil, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("system/info status = %d", rec.Code)
	}
	var sysInfo struct {
		EdgeMode  string `json:"edge_mode"`
		PublicURL string `json:"public_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sysInfo); err != nil {
		t.Fatalf("decode system/info: %v", err)
	}
	if sysInfo.EdgeMode != "proxy" || sysInfo.PublicURL != "https://media.example.com" {
		t.Fatalf("unexpected live system/info: %+v", sysInfo)
	}

	// 5. Validation errors for ACME / Cloudflare / public_url / CIDRs.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode": "acme",
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("acme without domain status = %d, want 422", rec.Code)
	}

	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":         "acme",
		"domain":            "lune.example.com",
		"acme_dns_provider": "cloudflare",
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cloudflare without token status = %d, want 422", rec.Code)
	}

	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":  "proxy",
		"public_url": "ftp://invalid.example.com",
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid public_url status = %d, want 422", rec.Code)
	}

	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":       "proxy",
		"trusted_proxies": "not-a-cidr",
	}, adminCookie, "")
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid trusted_proxies status = %d, want 422", rec.Code)
	}

	// 6. Switching to ACME with Cloudflare token succeeds, masks token, and signals restart_required=true.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":         "acme",
		"domain":            "lune.example.com",
		"acme_email":        "ops@example.com",
		"acme_staging":      true,
		"acme_dns_provider": "cloudflare",
		"cf_api_token":      "cf-secret-token-xyz",
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT acme status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &netCfg); err != nil {
		t.Fatalf("decode acme response: %v", err)
	}
	if netCfg.EdgeMode != config.EdgeModeACME || netCfg.ActiveEdgeMode != config.EdgeModeLocal || !netCfg.RestartRequired {
		t.Fatalf("expected acme with active=local and restart_required=true, got %+v", netCfg)
	}
	if netCfg.CFAPIToken != "" || !netCfg.HasCFAPIToken {
		t.Fatalf("expected masked cf_api_token with has_cf_api_token=true, got %+v", netCfg)
	}

	// 7. Subsequent PUT with masked placeholder preserves the saved Cloudflare token.
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/network/config", map[string]any{
		"edge_mode":         "acme",
		"domain":            "lune.example.com",
		"acme_email":        "ops2@example.com",
		"acme_staging":      false,
		"acme_dns_provider": "cloudflare",
		"cf_api_token":      maskedCFTokenValue,
	}, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT acme masked token status = %d, body = %s", rec.Code, rec.Body.String())
	}
	rawSetting, err := db.GetSetting(ctx, config.NetworkConfigSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	var persisted config.NetworkConfig
	if err := json.Unmarshal([]byte(rawSetting), &persisted); err != nil {
		t.Fatalf("unmarshal stored setting: %v", err)
	}
	if persisted.CFAPIToken != "cf-secret-token-xyz" {
		t.Fatalf("expected preserved cf_api_token, got %q", persisted.CFAPIToken)
	}

	// 8. Verify audit event was recorded.
	events, err := db.ListAuditEvents(ctx, 20)
	if err != nil {
		t.Fatalf("ListAuditEvents: %v", err)
	}
	foundAudit := false
	for _, ev := range events {
		if ev.EventType == "admin.network_config_updated" {
			foundAudit = true
			break
		}
	}
	if !foundAudit {
		t.Fatalf("expected admin.network_config_updated audit event, got %+v", events)
	}
}
