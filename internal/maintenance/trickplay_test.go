package maintenance

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

func setupTrickplayFixture(t *testing.T, seconds int) (context.Context, *store.Store, string) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not found on PATH")
	}
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mediaRoot := t.TempDir()
	videoPath := filepath.Join(mediaRoot, "clip.mkv")
	cmd := exec.CommandContext(ctx, ffmpegPath, "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc=duration="+itoa(seconds)+":size=640x360:rate=10",
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-pix_fmt", "yuv420p", "-y", videoPath)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg test video generation unavailable: %v (%s)", err, out)
	}
	info, _ := os.Stat(videoPath)
	if err := db.CreateLibrary(ctx, store.Library{ID: "lib-1", Name: "Movies", Kind: "movies", RootPath: mediaRoot}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertMediaItem(ctx, store.MediaItem{
		ID: "item-tp", LibraryID: "lib-1", Kind: "movie", Title: "Clip", SortTitle: "Clip",
		FilePath: videoPath, FileMtime: info.ModTime().UnixNano(), VideoCodec: "h264",
		DurationMS: int64(seconds) * 1000, VideoHDR: "sdr",
	}); err != nil {
		t.Fatal(err)
	}
	return ctx, db, dataDir
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestGenerateTrickplay_BuildsSheetsAndSkipsWhenCurrent(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 147)
	svc := NewService(ctx, db, Options{DataDir: dataDir})
	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true

	res, err := svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 1 || res.Total != 1 {
		t.Fatalf("first pass: %+v", res)
	}
	m, err := metadata.ReadTrickplayManifest(dataDir, "item-tp")
	if err != nil {
		t.Fatalf("manifest: %v", err)
	}
	// 147s at 10s intervals; FFmpeg's fps filter emits a thumbnail per full interval.
	if m.ThumbCount < 14 || m.ThumbCount > 15 || m.SheetCount != 1 || m.Columns != 10 || m.Rows != 10 {
		t.Fatalf("unexpected manifest: %+v", m)
	}
	if m.ThumbWidth != 320 || m.ThumbHeight != 180 || m.IntervalMS != 10000 || !m.KeyframesOnly {
		t.Fatalf("unexpected geometry: %+v", m)
	}
	if m.Format == "" {
		t.Fatalf("manifest missing format: %+v", m)
	}
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), "0."+m.Format)); err != nil {
		t.Fatalf("sheet missing: %v", err)
	}
	if _, err := os.Stat(metadata.TrickplayDir(dataDir, "item-tp") + ".building"); !os.IsNotExist(err) {
		t.Fatalf("build dir left behind: %v", err)
	}

	res, err = svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 0 || res.Existing != 1 {
		t.Fatalf("second pass should skip current item: %+v", res)
	}

	// Changing settings regenerates.
	cfg.TrickplayIntervalSeconds = 5
	res, err = svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 1 {
		t.Fatalf("settings change should regenerate: %+v", res)
	}
	m, _ = metadata.ReadTrickplayManifest(dataDir, "item-tp")
	if m.IntervalMS != 5000 || m.ThumbCount < 28 {
		t.Fatalf("unexpected regenerated manifest: %+v", m)
	}
}

func TestGenerateTrickplay_PausesWhileBusyAndGivesUp(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 300)
	svc := NewService(ctx, db, Options{
		DataDir:             dataDir,
		ActiveTranscodeDirs: func() map[string]struct{} { return map[string]struct{}{"/tmp/active": {}} },
	})
	oldPoll, oldPause := trickplayBusyPoll, trickplayMaxPausePerRun
	trickplayBusyPoll, trickplayMaxPausePerRun = 5*time.Millisecond, 150*time.Millisecond
	t.Cleanup(func() { trickplayBusyPoll, trickplayMaxPausePerRun = oldPoll, oldPause })

	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	var sawPause bool
	res, err := svc.generateTrickplay(ctx, cfg, func(done, total int, detail string) {
		if len(detail) > 6 && detail[:6] == "Paused" {
			sawPause = true
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !sawPause || !res.SkippedBusy || res.Generated != 0 {
		t.Fatalf("expected paused then skipped: sawPause=%v res=%+v", sawPause, res)
	}
	if _, err := metadata.ReadTrickplayManifest(dataDir, "item-tp"); err == nil {
		t.Fatal("no manifest should be written for an abandoned build")
	}
	// A busy skip must not be recorded as a permanent failure.
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), trickplayFailedMarker)); err == nil {
		t.Fatal("unexpected failure marker")
	}
}

func TestGenerateTrickplay_ResumesAfterPlaybackEnds(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 300)
	busy := make(chan struct{})
	svc := NewService(ctx, db, Options{
		DataDir: dataDir,
		ActiveTranscodeDirs: func() map[string]struct{} {
			select {
			case <-busy:
				return map[string]struct{}{}
			default:
				return map[string]struct{}{"/tmp/active": {}}
			}
		},
	})
	oldPoll := trickplayBusyPoll
	trickplayBusyPoll = 5 * time.Millisecond
	t.Cleanup(func() { trickplayBusyPoll = oldPoll })
	time.AfterFunc(200*time.Millisecond, func() { close(busy) })

	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	res, err := svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 1 || res.SkippedBusy {
		t.Fatalf("expected generation to resume and finish: %+v", res)
	}
}

func TestConfigTrickplayDefaultsAndValidation(t *testing.T) {
	c := Config{IntervalHours: 24, ArtworkGraceHours: 24, SubtitleCacheDays: 30, AuditRetentionDays: 90}.normalize()
	if c.TrickplayIntervalSeconds != 10 || c.TrickplayWidth != 320 || c.GenerateTrickplay {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if !DefaultConfig().GenerateTrickplay {
		t.Fatal("trickplay should be on by default for fresh installs")
	}
	for _, bad := range []Config{{TrickplayIntervalSeconds: 1, TrickplayWidth: 320}, {TrickplayIntervalSeconds: 10, TrickplayWidth: 321}, {TrickplayIntervalSeconds: 10, TrickplayWidth: 1000}} {
		b := c
		b.TrickplayIntervalSeconds, b.TrickplayWidth = bad.TrickplayIntervalSeconds, bad.TrickplayWidth
		if err := b.Validate(); err == nil {
			t.Fatalf("expected invalid: %+v", bad)
		}
	}
}

// makeClip encodes a clip from a lavfi filtergraph (a single video output).
func makeClip(t *testing.T, path, graph string) {
	t.Helper()
	cmd := exec.Command("ffmpeg", "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", graph,
		"-c:v", "libx264", "-preset", "ultrafast", "-g", "20", "-pix_fmt", "yuv420p", "-y", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("ffmpeg clip generation unavailable: %v (%s)", err, out)
	}
}

func TestGenerateTrickplay_DropsDuplicateTilesWithTimestamps(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 10)
	// 60s moving, 120s of a still black frame, 60s moving: the still section should collapse.
	item, err := db.GetMediaItem(ctx, "item-tp")
	if err != nil {
		t.Fatal(err)
	}
	makeClip(t, item.FilePath, "testsrc2=size=640x360:rate=10:duration=60[a];color=c=black:size=640x360:rate=10:duration=120[b];testsrc2=size=640x360:rate=10:duration=60[c];[a][b][c]concat=n=3:v=1:a=0")
	info, _ := os.Stat(item.FilePath)
	item.FileMtime = info.ModTime().UnixNano()
	item.DurationMS = 240000
	if err := db.UpsertMediaItem(ctx, item); err != nil {
		t.Fatal(err)
	}

	svc := NewService(ctx, db, Options{DataDir: dataDir})
	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	if res, err := svc.generateTrickplay(ctx, cfg, nil); err != nil || res.Generated != 1 {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}
	m, err := metadata.ReadTrickplayManifest(dataDir, "item-tp")
	if err != nil {
		t.Fatal(err)
	}
	// 24 uniform slots; the 12 black slots collapse to ~1.
	if m.ThumbCount > 15 || m.ThumbCount < 12 || len(m.TimestampsMS) != m.ThumbCount {
		t.Fatalf("expected deduplicated tiles with timestamps, got count=%d stamps=%v", m.ThumbCount, m.TimestampsMS)
	}
	for i := 1; i < len(m.TimestampsMS); i++ {
		if m.TimestampsMS[i] <= m.TimestampsMS[i-1] {
			t.Fatalf("timestamps not increasing: %v", m.TimestampsMS)
		}
	}
	// The moving section after the still part must still be represented at its real time.
	last := m.TimestampsMS[len(m.TimestampsMS)-1]
	if last < 220000 {
		t.Fatalf("last tile at %dms, want >= 220000 (%v)", last, m.TimestampsMS)
	}
	info2 := metadata.LoadTrickplay(dataDir, "item-tp")
	if info2 == nil || len(info2.TimestampsMS) != m.ThumbCount {
		t.Fatalf("client info should carry timestamps: %+v", info2)
	}
}

func TestGenerateTrickplay_FallsBackToJPEG(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 30)
	old := forceTrickplayFormat
	forceTrickplayFormat = "jpg"
	t.Cleanup(func() { forceTrickplayFormat = old })

	svc := NewService(ctx, db, Options{DataDir: dataDir})
	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	if res, err := svc.generateTrickplay(ctx, cfg, nil); err != nil || res.Generated != 1 || res.Bytes <= 0 {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}
	m, _ := metadata.ReadTrickplayManifest(dataDir, "item-tp")
	if m.Format != "jpg" {
		t.Fatalf("format = %q, want jpg", m.Format)
	}
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), "0.jpg")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), "0.png")); !os.IsNotExist(err) {
		t.Fatal("intermediate PNG sheet left behind")
	}
}

func TestUniformStamps(t *testing.T) {
	if !uniformStamps([]int64{0, 10000, 20000}, 10000) {
		t.Fatal("expected uniform")
	}
	if uniformStamps([]int64{0, 10000, 40000}, 10000) {
		t.Fatal("expected non-uniform after a dropped run")
	}
}

// trickplayFor replaces the fixture clip with graph and returns the built manifest.
func trickplayFor(t *testing.T, graph string, prep func(svc *Service, dataDir string, item store.MediaItem)) metadata.TrickplayManifest {
	t.Helper()
	ctx, db, dataDir := setupTrickplayFixture(t, 10)
	item, err := db.GetMediaItem(ctx, "item-tp")
	if err != nil {
		t.Fatal(err)
	}
	makeClip(t, item.FilePath, graph)
	info, _ := os.Stat(item.FilePath)
	item.FileMtime = info.ModTime().UnixNano()
	item.DurationMS = 60000
	if err := db.UpsertMediaItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	svc := NewService(ctx, db, Options{DataDir: dataDir})
	if prep != nil {
		prep(svc, dataDir, item)
	}
	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	if res, err := svc.generateTrickplay(ctx, cfg, nil); err != nil || res.Generated != 1 {
		t.Fatalf("generate: res=%+v err=%v", res, err)
	}
	m, err := metadata.ReadTrickplayManifest(dataDir, "item-tp")
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTrickplay_CropsBlackBarsAndFits16x9(t *testing.T) {
	cases := []struct {
		name, graph, crop string
		w, h              int
	}{
		{"letterbox", "testsrc2=size=1920x800:rate=24:duration=60,pad=1920:1080:0:140:black", "crop=1920:800:0:140", 320, 134},
		{"pillarbox", "testsrc2=size=1440x1080:rate=24:duration=60,pad=1920:1080:240:0:black", "crop=1440:1080:240:0", 240, 180},
		{"full frame", "testsrc2=size=1920x1080:rate=24:duration=60", "", 320, 180},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := trickplayFor(t, tc.graph, nil)
			if m.Crop != tc.crop || m.ThumbWidth != tc.w || m.ThumbHeight != tc.h {
				t.Fatalf("got crop=%q tile=%dx%d, want crop=%q tile=%dx%d", m.Crop, m.ThumbWidth, m.ThumbHeight, tc.crop, tc.w, tc.h)
			}
		})
	}
}

func TestTrickplay_ReusesChapterStillsCrop(t *testing.T) {
	// A full-frame clip, but chapter stills already recorded a crop for this exact file
	// version: trickplay must reuse it instead of detecting again.
	m := trickplayFor(t, "testsrc2=size=1920x1080:rate=24:duration=60", func(svc *Service, dataDir string, item store.MediaItem) {
		dir := filepath.Join(dataDir, "artwork", "chapters", item.ID)
		_ = os.MkdirAll(dir, 0o755)
		b, _ := json.Marshal(chapterStillsMeta{Version: chapterStillsVersion, SourceMtime: item.FileMtime, Crop: "crop=1920:800:0:140"})
		_ = os.WriteFile(filepath.Join(dir, chapterStillsMetaName), b, 0o644)
	})
	if m.Crop != "crop=1920:800:0:140" || m.ThumbHeight != 134 {
		t.Fatalf("expected reused chapter crop, got crop=%q tile=%dx%d", m.Crop, m.ThumbWidth, m.ThumbHeight)
	}
}
