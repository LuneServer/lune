package maintenance

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/store"
)

func newMediaTestService(t *testing.T, busy *atomic.Bool) *Service {
	t.Helper()
	return newMediaTestServiceWithPlayback(t, busy, nil)
}

func newMediaTestServiceWithPlayback(t *testing.T, busy, playing *atomic.Bool) *Service {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := NewService(ctx, db, Options{
		DataDir:       dataDir,
		IsScanRunning: busy.Load,
	})
	if playing != nil {
		svc.isPlaybackActive = playing.Load
	}
	if svc.ffmpegBin == "" {
		t.Skip("ffmpeg not found on PATH")
	}
	svc.newMediaPoll = 5 * time.Millisecond
	svc.newMediaQuiet = 20 * time.Millisecond
	t.Cleanup(func() { svc.CancelRun() })
	return svc
}

func waitForLastRun(t *testing.T, svc *Service, trigger string) RunReport {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		st := svc.Status()
		if st.LastRun != nil && st.LastRun.Trigger == trigger && !st.Running {
			return *st.LastRun
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("no %q pass finished; status=%+v", trigger, svc.Status())
	return RunReport{}
}

func TestNotifyMediaAdded_WaitsForScanThenRunsPerItemTasks(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true) // the scan that found the media is still finishing
	svc := newMediaTestService(t, &busy)

	svc.NotifyMediaAdded([]string{"item-1"})
	svc.NotifyMediaAdded([]string{"item-1"}) // coalesces
	if !svc.Status().NewMediaPending {
		t.Fatal("expected a queued new-media pass")
	}
	time.Sleep(60 * time.Millisecond)
	if st := svc.Status(); st.LastRun != nil || !st.NewMediaPending {
		t.Fatalf("ran while a scan was running: %+v", st)
	}

	busy.Store(false)
	rep := waitForLastRun(t, svc, TriggerNewMedia)
	if rep.Task != "" || rep.Outcome != OutcomeCompleted {
		t.Fatalf("unexpected new-media report: %+v", rep)
	}
	if svc.Status().NewMediaPending {
		t.Fatal("pending flag should clear once the pass starts")
	}
}

func TestNotifyMediaAdded_DuringRunningPassQueuesFollowUp(t *testing.T) {
	var busy atomic.Bool
	svc := newMediaTestService(t, &busy)

	// Simulate an in-flight pass (e.g. the scheduled one) that listed its items already.
	svc.mu.Lock()
	svc.running = true
	svc.mu.Unlock()

	svc.NotifyMediaAdded([]string{"item-1"})
	time.Sleep(60 * time.Millisecond)
	if st := svc.Status(); st.LastRun != nil || !st.NewMediaPending {
		t.Fatalf("follow-up must wait for the running pass: %+v", st)
	}

	svc.mu.Lock()
	svc.running = false
	svc.mu.Unlock()
	waitForLastRun(t, svc, TriggerNewMedia)
}

func TestNotifyMediaAdded_RespectsConfig(t *testing.T) {
	var busy atomic.Bool
	svc := newMediaTestService(t, &busy)

	for name, mutate := range map[string]func(*Config){
		"housekeeping disabled": func(c *Config) { c.Enabled = false },
		"both tasks disabled": func(c *Config) {
			c.GenerateChapterThumbnails = false
			c.GenerateTrickplay = false
		},
	} {
		cfg := DefaultConfig()
		mutate(&cfg)
		if _, err := svc.UpdateConfig(context.Background(), cfg); err != nil {
			t.Fatal(err)
		}
		svc.NotifyMediaAdded([]string{"item-1"})
		if svc.Status().NewMediaPending {
			t.Fatalf("%s: queued a new-media pass", name)
		}
	}
}

func TestCancelRun_DropsPendingNewMediaPass(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	svc := newMediaTestService(t, &busy)
	svc.NotifyMediaAdded([]string{"item-1"})
	if !svc.CancelRun() {
		t.Fatal("expected CancelRun to report the dropped pass")
	}
	busy.Store(false)
	time.Sleep(80 * time.Millisecond)
	if st := svc.Status(); st.NewMediaPending || st.LastRun != nil {
		t.Fatalf("canceled pass still ran or is pending: %+v", st)
	}
}

func TestNotifyMediaAdded_ConcurrentNotifiesRunOnce(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	svc := newMediaTestService(t, &busy)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() { defer wg.Done(); svc.NotifyMediaAdded([]string{"item-1"}) }()
	}
	wg.Wait()
	svc.mu.Lock()
	gen := svc.newMediaGen
	svc.mu.Unlock()
	if gen != 1 {
		t.Fatalf("expected one queued pass, got %d", gen)
	}
}

func TestComputeNextRun_BackgroundRunsDoNotMoveRollingSchedule(t *testing.T) {
	loc := time.UTC
	cfg := DefaultConfig()
	cfg.ScheduleTime = "" // rolling
	cfg.IntervalHours = 24
	scheduledAt := time.Date(2026, 11, 26, 3, 0, 0, 0, loc)
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, loc)
	for _, trigger := range []string{TriggerNewMedia, TriggerResume} {
		last := &RunReport{Trigger: trigger, FinishedAt: time.Date(2026, 11, 26, 11, 0, 0, 0, loc)}
		got := computeNextRun(now, cfg, last, scheduledAt, loc)
		if want := scheduledAt.Add(24 * time.Hour); !got.Equal(want) {
			t.Fatalf("%s: got %v, want %v", trigger, got, want)
		}
	}
	manual := &RunReport{Trigger: TriggerManual, FinishedAt: time.Date(2026, 11, 26, 11, 0, 0, 0, loc)}
	if got, want := computeNextRun(now, cfg, manual, scheduledAt, loc), manual.FinishedAt.Add(24*time.Hour); !got.Equal(want) {
		t.Fatalf("manual: got %v, want %v", got, want)
	}
}

func TestBusyReasonFor_NewMediaPassIgnoresDirectPlayOnly(t *testing.T) {
	var scanning, playing atomic.Bool
	transcodes := map[string]struct{}{}
	svc := &Service{
		isScanRunning:       scanning.Load,
		isPlaybackActive:    playing.Load,
		activeTranscodeDirs: func() map[string]struct{} { return transcodes },
	}
	backlog := context.Background()
	newMedia := withNewMediaScope(backlog, map[string]struct{}{"a": {}})

	playing.Store(true) // direct play: no FFmpeg session
	if got := svc.busyReasonFor(backlog); got != BusyReasonPlayback {
		t.Fatalf("backlog pass during direct play: %q, want %q", got, BusyReasonPlayback)
	}
	if got := svc.busyReasonFor(newMedia); got != "" {
		t.Fatalf("new-media pass during direct play: %q, want idle", got)
	}

	transcodes["session"] = struct{}{}
	if got := svc.busyReasonFor(newMedia); got != BusyReasonPlayback {
		t.Fatalf("new-media pass during a transcode: %q, want %q", got, BusyReasonPlayback)
	}
	delete(transcodes, "session")

	scanning.Store(true)
	if got := svc.busyReasonFor(newMedia); got != BusyReasonScan {
		t.Fatalf("new-media pass during a scan: %q, want %q", got, BusyReasonScan)
	}
}

func TestNewMediaScope_IncludesOnlyScannedItems(t *testing.T) {
	var none *newMediaScope
	if !none.includes("anything") {
		t.Fatal("backlog passes must include every item")
	}
	sc := newMediaScopeFrom(withNewMediaScope(context.Background(), map[string]struct{}{"a": {}}))
	if !sc.includes("a") || sc.includes("b") {
		t.Fatal("new-media scope must include only the scanned items")
	}
}

func TestNotifyMediaAdded_RunsDuringDirectPlay(t *testing.T) {
	var scanning, playing atomic.Bool
	playing.Store(true)
	svc := newMediaTestServiceWithPlayback(t, &scanning, &playing)

	svc.NotifyMediaAdded([]string{"item-1"})
	rep := waitForLastRun(t, svc, TriggerNewMedia)
	if rep.Outcome != OutcomeCompleted {
		t.Fatalf("new-media pass during direct play: %+v", rep)
	}
}

func TestNotifyMediaAdded_CoalescesItemIDs(t *testing.T) {
	var busy atomic.Bool
	busy.Store(true)
	svc := newMediaTestService(t, &busy)

	svc.NotifyMediaAdded(nil) // nothing changed: no pass
	if svc.Status().NewMediaPending {
		t.Fatal("queued a pass without items")
	}
	svc.NotifyMediaAdded([]string{"a", "b"})
	svc.NotifyMediaAdded([]string{"b", "c"})
	svc.mu.Lock()
	n, gen := len(svc.newMediaIDs), svc.newMediaGen
	svc.mu.Unlock()
	if n != 3 || gen != 1 {
		t.Fatalf("queued items = %d, passes = %d; want 3 items in 1 pass", n, gen)
	}
	svc.CancelRun()
	svc.mu.Lock()
	n = len(svc.newMediaIDs)
	svc.mu.Unlock()
	if n != 0 {
		t.Fatalf("CancelRun left %d queued items", n)
	}
}
