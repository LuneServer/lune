package maintenance

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/store"
)

func TestService_RunNowGuardrailsAndCleanup(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Seed a library and one media item referencing a poster and one cast portrait.
	lib := store.Library{ID: "lib-1", Name: "Movies", Kind: "movies", RootPath: t.TempDir()}
	if err := db.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	artDir := filepath.Join(dataDir, "artwork")
	refPoster := filepath.Join(artDir, "item-1", "poster.jpg")
	orphanOldPoster := filepath.Join(artDir, "deleted-item", "poster.jpg")
	orphanRecentPoster := filepath.Join(artDir, "brand-new", "poster.jpg")
	refPerson := filepath.Join(artDir, "people", "actor1.jpg")
	orphanOldPerson := filepath.Join(artDir, "people", "old-actor.jpg")

	for _, p := range []string{refPoster, orphanOldPoster, orphanRecentPoster, refPerson, orphanOldPerson} {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("jpeg-data"), 0o644)
	}
	oldTime := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{refPoster, orphanOldPoster, refPerson, orphanOldPerson} {
		_ = os.Chtimes(p, oldTime, oldTime)
	}

	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID:         "item-1",
		LibraryID:  lib.ID,
		Kind:       "movie",
		Title:      "Dune",
		SortTitle:  "Dune",
		FilePath:   filepath.Join(lib.RootPath, "Dune.mkv"),
		PosterPath: refPoster,
		CastJSON:   `[{"name":"Actor One","profile_path":"/actor1.jpg"}]`,
	}); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	// Seed transcode dirs: one active, one old abandoned, plus one old and one recent extracted .vtt.
	transDir := filepath.Join(dataDir, "transcode")
	activeSessionDir := filepath.Join(transDir, "active-sess")
	staleSessionDir := filepath.Join(transDir, "stale-sess")
	subsDir := filepath.Join(transDir, "subs")
	oldSub := filepath.Join(subsDir, "old.vtt")
	freshSub := filepath.Join(subsDir, "fresh.vtt")

	for _, d := range []string{activeSessionDir, staleSessionDir, subsDir} {
		_ = os.MkdirAll(d, 0o755)
	}
	_ = os.WriteFile(filepath.Join(activeSessionDir, "index.m3u8"), []byte("#EXTM3U"), 0o644)
	_ = os.WriteFile(filepath.Join(staleSessionDir, "index.m3u8"), []byte("#EXTM3U"), 0o644)
	_ = os.WriteFile(oldSub, []byte("WEBVTT"), 0o644)
	_ = os.WriteFile(freshSub, []byte("WEBVTT"), 0o644)

	veryOld := time.Now().Add(-45 * 24 * time.Hour)
	_ = os.Chtimes(activeSessionDir, oldTime, oldTime)
	_ = os.Chtimes(staleSessionDir, oldTime, oldTime)
	_ = os.Chtimes(oldSub, veryOld, veryOld)

	// Seed one old audit event and one recent audit event.
	_ = db.InsertAuditEvent(ctx, store.AuditEvent{EventType: "auth.login", ClientIP: "127.0.0.1", Detail: "recent"})
	_ = db.InsertAuditEvent(ctx, store.AuditEvent{EventType: "auth.login", ClientIP: "127.0.0.1", Detail: "old"})
	// Age the "old" event directly via PruneAuditEventsBefore boundary test: wait, use a separate check or update via setting?
	// We can test retention with a custom service config and verify recent event survives while old pruned works via SQLite.
	scanBusy := true
	svc := NewService(ctx, db, Options{
		DataDir:       dataDir,
		IsScanRunning: func() bool { return scanBusy },
		ActiveTranscodeDirs: func() map[string]struct{} {
			return map[string]struct{}{filepath.Clean(activeSessionDir): {}}
		},
	})

	// 1. When a scan is running, artwork cleanup must be skipped while transcode & DB still run.
	st, err := svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (scan busy): %v", err)
	}
	if st.LastRun == nil || !st.LastRun.SkippedArtworkScanBusy {
		t.Fatalf("expected SkippedArtworkScanBusy=true, got %+v", st.LastRun)
	}
	if _, err := os.Stat(orphanOldPoster); err != nil {
		t.Fatalf("expected orphan poster untouched while scan busy: %v", err)
	}
	if st.LastRun.TranscodeItemsRemoved != 2 {
		t.Fatalf("expected 2 stale transcode items removed (stale session + old sub), got %d", st.LastRun.TranscodeItemsRemoved)
	}
	if !st.LastRun.DatabaseOptimized {
		t.Fatalf("expected DatabaseOptimized=true")
	}
	if _, err := os.Stat(activeSessionDir); err != nil {
		t.Fatalf("expected active transcode dir preserved: %v", err)
	}
	if _, err := os.Stat(staleSessionDir); !os.IsNotExist(err) {
		t.Fatalf("expected stale transcode dir removed")
	}
	if _, err := os.Stat(oldSub); !os.IsNotExist(err) {
		t.Fatalf("expected old cached .vtt removed")
	}
	if _, err := os.Stat(freshSub); err != nil {
		t.Fatalf("expected fresh cached .vtt preserved: %v", err)
	}

	// 2. Once scan finishes, artwork cleanup removes old orphans while preserving referenced & recent files.
	scanBusy = false
	st, err = svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (scan idle): %v", err)
	}
	if st.LastRun == nil || st.LastRun.SkippedArtworkScanBusy {
		t.Fatalf("expected artwork sweep to run when scan idle: %+v", st.LastRun)
	}
	if st.LastRun.ArtworkFilesRemoved != 2 {
		t.Fatalf("expected 2 orphaned artwork files removed, got %d", st.LastRun.ArtworkFilesRemoved)
	}
	if _, err := os.Stat(refPoster); err != nil {
		t.Fatalf("expected referenced poster preserved: %v", err)
	}
	if _, err := os.Stat(refPerson); err != nil {
		t.Fatalf("expected referenced person photo preserved: %v", err)
	}
	if _, err := os.Stat(orphanRecentPoster); err != nil {
		t.Fatalf("expected recent orphan within grace window preserved: %v", err)
	}
	if _, err := os.Stat(orphanOldPoster); !os.IsNotExist(err) {
		t.Fatalf("expected old orphan poster removed")
	}
	if _, err := os.Stat(orphanOldPerson); !os.IsNotExist(err) {
		t.Fatalf("expected old orphan person photo removed")
	}

	// 3. Config validation & persistence across restart.
	bad := DefaultConfig()
	bad.IntervalHours = 0
	if _, err := svc.UpdateConfig(ctx, bad); err == nil {
		t.Fatal("expected invalid interval_hours to fail validation")
	}

	custom := DefaultConfig()
	custom.Enabled = false
	custom.IntervalHours = 72
	custom.AuditRetentionDays = 30
	if _, err := svc.UpdateConfig(ctx, custom); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	reloaded := NewService(ctx, db, Options{DataDir: dataDir})
	got := reloaded.Status()
	if got.Config.Enabled || got.Config.IntervalHours != 72 || got.Config.AuditRetentionDays != 30 {
		t.Fatalf("expected persisted maintenance config after reload, got %+v", got.Config)
	}
	if got.LastRun == nil || got.LastRun.ArtworkFilesRemoved != 2 {
		t.Fatalf("expected persisted last_run after reload, got %+v", got.LastRun)
	}
}

func TestScheduleTimeValidationAndNextRun(t *testing.T) {
	for _, valid := range []string{"", "00:00", "03:00", "23:59"} {
		cfg := DefaultConfig()
		cfg.ScheduleTime = valid
		if err := cfg.Validate(); err != nil {
			t.Fatalf("expected schedule_time %q to be valid, got %v", valid, err)
		}
	}
	for _, invalid := range []string{"3:00", "24:00", "12:60", "ab:cd", "03:00:00"} {
		cfg := DefaultConfig()
		cfg.ScheduleTime = invalid
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected schedule_time %q to fail validation", invalid)
		}
	}

	loc := time.UTC
	cfg := DefaultConfig()
	cfg.IntervalHours = 24
	cfg.ScheduleTime = "03:00"

	// Before 03:00 -> runs today at 03:00.
	nowBefore := time.Date(2026, 11, 26, 1, 30, 0, 0, loc)
	next := computeNextRun(nowBefore, cfg, nil, loc)
	want := time.Date(2026, 11, 26, 3, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("before 03:00: got %v, want %v", next, want)
	}

	// After 03:00 -> runs tomorrow at 03:00.
	nowAfter := time.Date(2026, 11, 26, 10, 0, 0, 0, loc)
	next = computeNextRun(nowAfter, cfg, nil, loc)
	want = time.Date(2026, 11, 27, 3, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("after 03:00: got %v, want %v", next, want)
	}

	// Multi-day interval (72h = 3 days) after a scheduled run -> 3 days later at 03:00.
	cfg3d := DefaultConfig()
	cfg3d.IntervalHours = 72
	cfg3d.ScheduleTime = "03:00"
	lastScheduled := &RunReport{
		Trigger:    "scheduled",
		FinishedAt: time.Date(2026, 11, 26, 3, 0, 5, 0, loc),
	}
	next = computeNextRun(time.Date(2026, 11, 26, 3, 0, 10, 0, loc), cfg3d, lastScheduled, loc)
	want = time.Date(2026, 11, 29, 3, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("72h scheduled: got %v, want %v", next, want)
	}

	// Sub-day interval (12h) anchored at 03:00 -> 15:00 when now is 10:00.
	cfg12h := DefaultConfig()
	cfg12h.IntervalHours = 12
	cfg12h.ScheduleTime = "03:00"
	next = computeNextRun(nowAfter, cfg12h, nil, loc)
	want = time.Date(2026, 11, 26, 15, 0, 0, 0, loc)
	if !next.Equal(want) {
		t.Fatalf("12h anchored at 03:00: got %v, want %v", next, want)
	}
}
