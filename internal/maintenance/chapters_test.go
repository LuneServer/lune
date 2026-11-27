package maintenance

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

func TestChapterThumbnailSeekMS(t *testing.T) {
	tests := []struct {
		name string
		ch   metadata.ChapterInfo
		want int64
	}{
		{
			name: "long chapter caps offset at 5000ms",
			ch:   metadata.ChapterInfo{Index: 0, StartMS: 10000, EndMS: 100000},
			want: 15000,
		},
		{
			name: "short chapter uses quarter duration",
			ch:   metadata.ChapterInfo{Index: 1, StartMS: 2000, EndMS: 10000},
			want: 4000,
		},
		{
			name: "zero or inverted duration falls back to +3000ms",
			ch:   metadata.ChapterInfo{Index: 2, StartMS: 5000, EndMS: 5000},
			want: 8000,
		},
		{
			name: "negative start clamped to 0",
			ch:   metadata.ChapterInfo{Index: 3, StartMS: -500, EndMS: 0},
			want: 3000,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := chapterThumbnailSeekMS(tc.ch); got != tc.want {
				t.Fatalf("chapterThumbnailSeekMS(%+v) = %d, want %d", tc.ch, got, tc.want)
			}
		})
	}
}

func TestConfigUpgradeDefaultsGenerateChapterThumbnailsTrue(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// Persist an older maintenance_config JSON that omits generate_chapter_thumbnails.
	legacyJSON := `{"enabled":true,"interval_hours":24,"schedule_time":"03:00","cleanup_orphaned_artwork":true,"artwork_grace_hours":24,"cleanup_transcode_cache":true,"subtitle_cache_days":30,"prune_audit_logs":true,"audit_retention_days":90,"optimize_database":true}`
	if err := db.SetSetting(ctx, ConfigSettingKey, legacyJSON); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	svc := NewService(ctx, db, Options{DataDir: dataDir})
	if !svc.Status().Config.GenerateChapterThumbnails {
		t.Fatal("expected GenerateChapterThumbnails to default to true when omitted from persisted JSON")
	}

	// Explicit false must persist across reload.
	cfg := svc.Status().Config
	cfg.GenerateChapterThumbnails = false
	if _, err := svc.UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}
	reloaded := NewService(ctx, db, Options{DataDir: dataDir})
	if reloaded.Status().Config.GenerateChapterThumbnails {
		t.Fatal("expected explicit generate_chapter_thumbnails=false to persist across reload")
	}
}

func TestCleanOrphanedArtwork_ChapterStills(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	lib := store.Library{ID: "lib-ch", Name: "Anime", Kind: "anime", RootPath: t.TempDir()}
	if err := db.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID:        "live-item",
		LibraryID: lib.ID,
		Kind:      "episode",
		Title:     "Ep 1",
		SortTitle: "Ep 1",
		FilePath:  filepath.Join(lib.RootPath, "ep1.mkv"),
	}); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	chaptersRoot := filepath.Join(dataDir, "artwork", "chapters")
	liveStill := filepath.Join(chaptersRoot, "live-item", "0.jpg")
	orphanStill0 := filepath.Join(chaptersRoot, "deleted-item", "0.jpg")
	orphanStill1 := filepath.Join(chaptersRoot, "deleted-item", "1.jpg")
	orphanTmp := filepath.Join(chaptersRoot, "deleted-item-tmp", "0.tmp.jpg")

	for _, p := range []string{liveStill, orphanStill0, orphanStill1, orphanTmp} {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte("jpeg-bytes"), 0o644)
	}
	oldTime := time.Now().Add(-72 * time.Hour)
	for _, p := range []string{liveStill, orphanStill0, orphanStill1, orphanTmp} {
		_ = os.Chtimes(p, oldTime, oldTime)
	}

	svc := NewService(ctx, db, Options{DataDir: dataDir})
	cfg := DefaultConfig()
	cfg.GenerateChapterThumbnails = false
	if _, err := svc.UpdateConfig(ctx, cfg); err != nil {
		t.Fatalf("UpdateConfig: %v", err)
	}

	st, err := svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow: %v", err)
	}
	if st.LastRun == nil || st.LastRun.ArtworkFilesRemoved != 2 {
		t.Fatalf("expected 2 orphaned chapter stills removed, got %+v", st.LastRun)
	}
	if st.LastRun.ArtworkBytesFreed != int64(2*len("jpeg-bytes")) {
		t.Fatalf("expected %d bytes freed, got %d", 2*len("jpeg-bytes"), st.LastRun.ArtworkBytesFreed)
	}
	if _, err := os.Stat(liveStill); err != nil {
		t.Fatalf("expected live item chapter still preserved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(chaptersRoot, "deleted-item")); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned chapter directory removed, stat err=%v", err)
	}
	if _, err := os.Stat(orphanTmp); err != nil {
		t.Fatalf("expected in-flight .tmp.jpg file ignored by cleanup: %v", err)
	}
}

func TestGenerateChapterThumbnails_BusyGuardAndFFmpegExtraction(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	mediaRoot := t.TempDir()
	videoPath := filepath.Join(mediaRoot, "sample.mp4")

	lib := store.Library{ID: "lib-1", Name: "Movies", Kind: "movies", RootPath: mediaRoot}
	if err := db.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	chapters := []metadata.ChapterInfo{
		{Index: 0, Title: "Opening", StartMS: 0, EndMS: 4000, Kind: "intro"},
		{Index: 1, Title: "Part A", StartMS: 4000, EndMS: 8000, Kind: "chapter"},
	}
	chaptersJSON, _ := json.Marshal(chapters)

	ffmpegPath, lookErr := exec.LookPath("ffmpeg")
	hasFFmpeg := lookErr == nil && ffmpegPath != ""
	if hasFFmpeg {
		// Create a tiny 9-second synthetic MP4 using FFmpeg's built-in testsrc.
		cmd := exec.CommandContext(ctx, ffmpegPath,
			"-nostdin", "-loglevel", "error",
			"-f", "lavfi", "-i", "testsrc=duration=9:size=320x180:rate=10",
			"-c:v", "libx264", "-pix_fmt", "yuv420p",
			"-y", videoPath,
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("ffmpeg test video generation unavailable: %v (%s)", err, string(out))
		}
	} else {
		_ = os.WriteFile(videoPath, []byte("placeholder"), 0o644)
	}
	info, err := os.Stat(videoPath)
	if err != nil {
		t.Fatalf("stat video: %v", err)
	}

	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID:           "item-ch-1",
		LibraryID:    lib.ID,
		Kind:         "movie",
		Title:        "Sample",
		SortTitle:    "Sample",
		FilePath:     videoPath,
		FileMtime:    info.ModTime().UnixNano(),
		VideoHDR:     "sdr",
		ChaptersJSON: string(chaptersJSON),
	}); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	// Pre-populate a stale chapter still index (99.jpg) to verify pruning.
	itemChapterDir := filepath.Join(dataDir, "artwork", "chapters", "item-ch-1")
	_ = os.MkdirAll(itemChapterDir, 0o755)
	staleStill := filepath.Join(itemChapterDir, "99.jpg")
	_ = os.WriteFile(staleStill, []byte("stale"), 0o644)

	scanRunning := true
	activeTranscodes := map[string]struct{}{}
	svc := NewService(ctx, db, Options{
		DataDir:             dataDir,
		IsScanRunning:       func() bool { return scanRunning },
		ActiveTranscodeDirs: func() map[string]struct{} { return activeTranscodes },
	})

	// 1. Must skip while a library scan is running.
	st, err := svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (scan busy): %v", err)
	}
	if st.LastRun.ChapterThumbnailsGenerated != 0 {
		t.Fatalf("expected 0 thumbnails generated while scan running, got %d", st.LastRun.ChapterThumbnailsGenerated)
	}

	// 2. Must skip while a transcode session is active.
	scanRunning = false
	activeTranscodes["/tmp/active"] = struct{}{}
	st, err = svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (transcode busy): %v", err)
	}
	if st.LastRun.ChapterThumbnailsGenerated != 0 {
		t.Fatalf("expected 0 thumbnails generated while transcode active, got %d", st.LastRun.ChapterThumbnailsGenerated)
	}

	// 3. When idle and ffmpeg is available, extracts both chapter stills and prunes stale index 99.jpg.
	delete(activeTranscodes, "/tmp/active")
	if !hasFFmpeg {
		t.Skip("ffmpeg not found on PATH; skipping real extraction check")
	}

	st, err = svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (idle): %v", err)
	}
	if st.LastRun.ChapterThumbnailsGenerated != 2 {
		t.Fatalf("expected 2 chapter thumbnails generated, got %d (errors=%v)", st.LastRun.ChapterThumbnailsGenerated, st.LastRun.Errors)
	}

	for _, idx := range []string{"0.jpg", "1.jpg"} {
		raw, err := os.ReadFile(filepath.Join(itemChapterDir, idx))
		if err != nil {
			t.Fatalf("read %s: %v", idx, err)
		}
		if len(raw) < 4 || !bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}) {
			t.Fatalf("expected %s to be a valid JPEG, got %d bytes", idx, len(raw))
		}
	}
	if _, err := os.Stat(staleStill); !os.IsNotExist(err) {
		t.Fatalf("expected stale 99.jpg to be pruned, stat err=%v", err)
	}

	// 4. Subsequent run skips already up-to-date thumbnails.
	st, err = svc.RunNow(ctx)
	if err != nil {
		t.Fatalf("RunNow (second pass): %v", err)
	}
	if st.LastRun.ChapterThumbnailsGenerated != 0 {
		t.Fatalf("expected 0 chapter thumbnails generated on second pass, got %d", st.LastRun.ChapterThumbnailsGenerated)
	}
}

func TestChapterStillUpToDate_UsesNanosecondMtime(t *testing.T) {
	dir := t.TempDir()
	still := filepath.Join(dir, "0.jpg")
	if err := os.WriteFile(still, []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	stillTime := time.Now().Add(-time.Hour)
	if err := os.Chtimes(still, stillTime, stillTime); err != nil {
		t.Fatal(err)
	}
	// media_items.file_mtime is Unix nanoseconds; a still written after the file is current.
	if !chapterStillUpToDate(still, stillTime.Add(-time.Minute).UnixNano()) {
		t.Fatal("expected still newer than media file to be up to date")
	}
	if chapterStillUpToDate(still, stillTime.Add(time.Minute).UnixNano()) {
		t.Fatal("expected still older than media file to be stale")
	}
	if chapterStillUpToDate(filepath.Join(dir, "missing.jpg"), 0) {
		t.Fatal("expected missing still to be stale")
	}
}

func TestPruneStaleChapterStills_RemovesStaleFailedMarkers(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"0.jpg", "0.failed", "5.failed", "5.jpg", "1.tmp.jpg"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	pruneStaleChapterStills(dir, map[int]struct{}{0: {}})
	for name, want := range map[string]bool{"0.jpg": true, "0.failed": true, "5.failed": false, "5.jpg": false, "1.tmp.jpg": true} {
		_, err := os.Stat(filepath.Join(dir, name))
		if got := err == nil; got != want {
			t.Fatalf("%s exists=%v, want %v", name, got, want)
		}
	}
}

func TestGenerateChapterThumbnails_FailedChapterNotRetried(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
	mediaRoot := t.TempDir()
	// Not a decodable video: every extraction fails.
	videoPath := filepath.Join(mediaRoot, "broken.mkv")
	if err := os.WriteFile(videoPath, []byte("not a video"), 0o644); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(videoPath)
	if err := db.CreateLibrary(ctx, store.Library{ID: "lib-1", Name: "Movies", Kind: "movies", RootPath: mediaRoot}); err != nil {
		t.Fatal(err)
	}
	chaptersJSON, _ := json.Marshal([]metadata.ChapterInfo{{Index: 0, StartMS: 0, EndMS: 4000}})
	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID: "item-bad", LibraryID: "lib-1", Kind: "movie", Title: "Broken", SortTitle: "Broken",
		FilePath: videoPath, FileMtime: info.ModTime().UnixNano(), ChaptersJSON: string(chaptersJSON),
	}); err != nil {
		t.Fatal(err)
	}

	svc := NewService(ctx, db, Options{DataDir: dataDir})
	first, err := svc.generateChapterThumbnails(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if first.Unextractable != 1 || first.Generated != 0 {
		t.Fatalf("first pass: got %+v, want 1 unextractable", first)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "artwork", "chapters", "item-bad", "0"+chapterFailedMarkerExt)); err != nil {
		t.Fatalf("expected failure marker: %v", err)
	}
	attempts := 0
	second, err := svc.generateChapterThumbnails(ctx, func(done, total int, detail string) { attempts++ })
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 0 || second.Unextractable != 1 {
		t.Fatalf("second pass should not retry the failed chapter: attempts=%d res=%+v", attempts, second)
	}
}

func TestChapterThumbnails_AutoResumeAfterPlaybackEnds(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	var mu sync.Mutex
	busy := true
	svc := NewService(ctx, db, Options{
		DataDir: dataDir,
		ActiveTranscodeDirs: func() map[string]struct{} {
			mu.Lock()
			defer mu.Unlock()
			if busy {
				return map[string]struct{}{"/tmp/active": {}}
			}
			return map[string]struct{}{}
		},
	})
	if svc.ffmpegBin == "" {
		t.Skip("ffmpeg not found on PATH")
	}
	svc.resumePoll = 10 * time.Millisecond
	svc.resumeQuiet = 30 * time.Millisecond

	st, err := svc.RunTask(ctx, TaskChapterThumbnails)
	if err != nil {
		t.Fatal(err)
	}
	if !st.LastRun.SkippedChaptersBusy || !st.ChaptersResumePending {
		t.Fatalf("expected paused pass with pending resume, got last_run=%+v pending=%v", st.LastRun, st.ChaptersResumePending)
	}

	// Still busy: must not resume.
	time.Sleep(100 * time.Millisecond)
	if st := svc.Status(); st.LastRun.Trigger != "manual" || !st.ChaptersResumePending {
		t.Fatalf("resumed while still busy: trigger=%q pending=%v", st.LastRun.Trigger, st.ChaptersResumePending)
	}

	mu.Lock()
	busy = false
	mu.Unlock()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := svc.Status()
		if st.LastRun != nil && st.LastRun.Trigger == "resume" && !st.Running {
			if st.ChaptersResumePending || st.LastRun.SkippedChaptersBusy {
				t.Fatalf("resumed pass should complete: %+v pending=%v", st.LastRun, st.ChaptersResumePending)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("chapter generation did not resume after playback ended")
}

func TestCancelRun_DropsPendingChapterResume(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()
	svc := NewService(ctx, db, Options{
		DataDir:             dataDir,
		ActiveTranscodeDirs: func() map[string]struct{} { return map[string]struct{}{"/tmp/active": {}} },
	})
	if svc.ffmpegBin == "" {
		t.Skip("ffmpeg not found on PATH")
	}
	if _, err := svc.RunTask(ctx, TaskChapterThumbnails); err != nil {
		t.Fatal(err)
	}
	if !svc.Status().ChaptersResumePending {
		t.Fatal("expected pending resume")
	}
	if !svc.CancelRun() {
		t.Fatal("expected CancelRun to report the dropped resume")
	}
	if svc.Status().ChaptersResumePending {
		t.Fatal("expected pending resume cleared")
	}
}
