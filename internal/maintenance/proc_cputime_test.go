package maintenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
)

func TestParseProcStatCPU(t *testing.T) {
	// Real layout; the command name contains spaces and a ')' to check field counting.
	line := []byte("4242 (ffmpeg (x) y) S 1 4242 4242 0 -1 4194560 1234 0 0 0 1500 250 0 0 35 15 3 0 12345 1000000 2000 18446744073709551615\n")
	got, ok := parseProcStatCPU(line)
	if !ok || got != 17500*time.Millisecond {
		t.Fatalf("got %v, %v; want 17.5s", got, ok)
	}
	for _, bad := range []string{"", "4242 ffmpeg S 1", "4242 (ffmpeg) S 1 2 3", "4242 (ffmpeg) S 1 4242 4242 0 -1 4194560 1234 0 0 0 x 250 0"} {
		if _, ok := parseProcStatCPU([]byte(bad)); ok {
			t.Errorf("parseProcStatCPU(%q) should fail", bad)
		}
	}
}

func stubTrickplayBudget(t *testing.T, cpu time.Duration, wall time.Duration) {
	t.Helper()
	oldCPU, oldPoll, oldWall := cpuTimeOf, trickplayBusyPoll, trickplayMaxWallPerItem
	cpuTimeOf = func(*exec.Cmd) (time.Duration, bool) { return cpu, true }
	trickplayBusyPoll, trickplayMaxWallPerItem = 5*time.Millisecond, wall
	t.Cleanup(func() { cpuTimeOf, trickplayBusyPoll, trickplayMaxWallPerItem = oldCPU, oldPoll, oldWall })
}

// A starved job (little CPU used over a long unpaused stretch) is retried next run, not
// marked failed.
func TestGenerateTrickplay_StarvedJobIsRetriedNotFailed(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 300)
	svc := NewService(ctx, db, Options{DataDir: dataDir})
	stubTrickplayBudget(t, time.Second, time.Millisecond)

	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	res, err := svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 0 || res.Unextractable != 0 || res.SkippedBusy {
		t.Fatalf("expected a quiet skip: %+v", res)
	}
	if _, err := metadata.ReadTrickplayManifest(dataDir, "item-tp"); err == nil {
		t.Fatal("no manifest should be written for an abandoned build")
	}
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), trickplayFailedMarker)); err == nil {
		t.Fatal("a starved job must not be marked failed")
	}
}

// Genuinely exceeding the CPU budget still marks the item failed.
func TestGenerateTrickplay_CPUBudgetOverrunMarksFailed(t *testing.T) {
	ctx, db, dataDir := setupTrickplayFixture(t, 300)
	svc := NewService(ctx, db, Options{DataDir: dataDir})
	stubTrickplayBudget(t, trickplayMaxCPUPerItem+time.Minute, time.Hour)

	cfg := DefaultConfig()
	cfg.GenerateTrickplay = true
	res, err := svc.generateTrickplay(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Generated != 0 || res.Unextractable != 1 {
		t.Fatalf("expected one failure: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(metadata.TrickplayDir(dataDir, "item-tp"), trickplayFailedMarker)); err != nil {
		t.Fatalf("expected failure marker: %v", err)
	}
}
