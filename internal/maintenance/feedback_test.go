package maintenance

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/LuneServer/lune/internal/store"
)

func openFeedbackStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dataDir := t.TempDir()
	db, err := store.Open(context.Background(), filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, dataDir
}

func TestRunTask_ReportsSkipReasonWhenScanRunning(t *testing.T) {
	ctx := context.Background()
	db, dataDir := openFeedbackStore(t)
	svc := NewService(ctx, db, Options{DataDir: dataDir, IsScanRunning: func() bool { return true }})

	st, err := svc.RunTask(ctx, TaskArtwork)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want %q", st.LastRun.Outcome, OutcomeSkipped)
	}
	if len(st.LastRun.Skipped) != 1 || st.LastRun.Skipped[0].Task != TaskArtwork || st.LastRun.Skipped[0].Reason != BusyReasonScan {
		t.Fatalf("skipped = %+v, want artwork/%q", st.LastRun.Skipped, BusyReasonScan)
	}

	// A task that does not yield still completes.
	st, err = svc.RunTask(ctx, TaskAudit)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun.Outcome != OutcomeCompleted || len(st.LastRun.Skipped) != 0 {
		t.Fatalf("audit run: outcome=%q skipped=%+v", st.LastRun.Outcome, st.LastRun.Skipped)
	}

	// A full pass during a scan is partial: the artwork sweep is skipped, the rest run.
	st, err = svc.RunNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.LastRun.Outcome != OutcomePartial {
		t.Fatalf("full pass outcome = %q (skipped=%+v), want partial", st.LastRun.Outcome, st.LastRun.Skipped)
	}
	svc.CancelRun() // drop any chapter resume armed by the pass
}

func TestRunTask_ChapterStillsYieldToDirectPlay(t *testing.T) {
	ctx := context.Background()
	db, dataDir := openFeedbackStore(t)
	svc := NewService(ctx, db, Options{DataDir: dataDir, IsPlaybackActive: func() bool { return true }})
	defer svc.CancelRun()

	if svc.busyReason() != BusyReasonPlayback {
		t.Fatalf("busyReason = %q, want %q", svc.busyReason(), BusyReasonPlayback)
	}

	st, err := svc.RunTask(ctx, TaskChapterThumbnails)
	if err != nil {
		t.Fatal(err)
	}
	want := BusyReasonPlayback
	if svc.ffmpegBin == "" {
		want = "FFmpeg not available"
	} else if !st.LastRun.SkippedChaptersBusy || !st.ChaptersResumePending {
		t.Fatalf("expected chapter pass to yield to direct play: %+v pending=%v", st.LastRun, st.ChaptersResumePending)
	}
	if st.LastRun.Outcome != OutcomeSkipped {
		t.Fatalf("outcome = %q, want skipped", st.LastRun.Outcome)
	}
	if len(st.LastRun.Skipped) != 1 || st.LastRun.Skipped[0].Task != TaskChapterThumbnails || st.LastRun.Skipped[0].Reason != want {
		t.Fatalf("skipped = %+v, want chapter_thumbnails/%q", st.LastRun.Skipped, want)
	}
}

func TestBusyReason_ScanTakesPrecedence(t *testing.T) {
	svc := &Service{
		isScanRunning:    func() bool { return true },
		isPlaybackActive: func() bool { return true },
	}
	if got := svc.busyReason(); got != BusyReasonScan {
		t.Fatalf("busyReason = %q, want %q", got, BusyReasonScan)
	}
	svc.isScanRunning = func() bool { return false }
	if got := svc.busyReason(); got != BusyReasonPlayback {
		t.Fatalf("busyReason = %q, want %q", got, BusyReasonPlayback)
	}
	svc.isPlaybackActive = func() bool { return false }
	if got := svc.busyReason(); got != "" {
		t.Fatalf("busyReason = %q, want idle", got)
	}
}
