package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/maintenance"
	"github.com/LuneServer/lune/internal/store"
)

func TestAdminMaintenanceEndpoints(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	authSvc := auth.NewService(db, nil)
	h := NewHandler(Dependencies{
		Config:    config.Config{EdgeMode: config.EdgeModeDev, DataDir: dataDir},
		Store:     db,
		Auth:      authSvc,
		StartedAt: time.Now(),
	})

	// Setup admin
	rec := doJSON(t, h, http.MethodPost, "/api/v1/setup", map[string]any{
		"username": "admin",
		"password": "super-secret-password",
	}, nil, "")
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup status = %d", rec.Code)
	}
	adminCookie := rec.Result().Cookies()[0]

	// GET default status
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/maintenance", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET maintenance status = %d body=%s", rec.Code, rec.Body.String())
	}
	var st maintenance.Status
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatalf("unmarshal status: %v", err)
	}
	if !st.Config.Enabled || st.Config.IntervalHours != 24 || st.Config.AuditRetentionDays != 90 {
		t.Fatalf("unexpected default maintenance config: %+v", st.Config)
	}

	// PUT invalid config -> 400
	bad := st.Config
	bad.AuditRetentionDays = 2
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/maintenance/config", bad, adminCookie, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid retention days, got %d", rec.Code)
	}

	// PUT valid config -> 200
	updated := st.Config
	updated.IntervalHours = 48
	updated.AuditRetentionDays = 60
	rec = doJSON(t, h, http.MethodPut, "/api/v1/admin/maintenance/config", updated, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT maintenance config status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stAfterPut maintenance.Status
	_ = json.Unmarshal(rec.Body.Bytes(), &stAfterPut)
	if stAfterPut.Config.IntervalHours != 48 || stAfterPut.Config.AuditRetentionDays != 60 {
		t.Fatalf("unexpected saved config: %+v", stAfterPut.Config)
	}

	// POST run -> 200 with populated last_run
	rec = doJSON(t, h, http.MethodPost, "/api/v1/admin/maintenance/run", nil, adminCookie, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("POST maintenance run status = %d body=%s", rec.Code, rec.Body.String())
	}
	var stAfterRun maintenance.Status
	_ = json.Unmarshal(rec.Body.Bytes(), &stAfterRun)
	if stAfterRun.LastRun == nil || stAfterRun.LastRun.Trigger != "manual" || !stAfterRun.LastRun.DatabaseOptimized {
		t.Fatalf("unexpected run report: %+v", stAfterRun.LastRun)
	}

	// Unauthenticated -> 401
	rec = doJSON(t, h, http.MethodGet, "/api/v1/admin/maintenance", nil, nil, "")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 unauthenticated, got %d", rec.Code)
	}
}
