package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/store"
)

func TestSystemEndpointsAndOpenAPI(t *testing.T) {
	t.Setenv("LUNE_ENABLE_API_DOCS", "true")
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev},
		Store:     db,
		StartedAt: time.Now().Add(-5 * time.Second),
	})

	// 1. /healthz
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", rec.Code)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("expected security headers middleware to set X-Content-Type-Options")
	}
	var health map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &health); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	if health["status"] != "ok" || health["database_ok"] != true {
		t.Fatalf("unexpected /healthz body: %v", health)
	}

	// 2. /api/v1/system/info
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/api/v1/system/info status = %d, want 200", rec.Code)
	}
	var info map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &info); err != nil {
		t.Fatalf("decode /api/v1/system/info: %v", err)
	}
	if info["brand"] != "Lune" || info["setup_required"] != true {
		t.Fatalf("unexpected system info: %v", info)
	}

	// 3. /openapi.json
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/openapi.json", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/openapi.json status = %d, want 200", rec.Code)
	}
	var spec map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
		t.Fatalf("decode /openapi.json: %v", err)
	}
	if spec["openapi"] == nil {
		t.Fatalf("expected OpenAPI version field in spec")
	}
}
