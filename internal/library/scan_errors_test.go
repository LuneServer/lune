package library

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

type failingProber struct{}

func (failingProber) Probe(_ context.Context, path string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{}, os.ErrInvalid
}

// ctxCheckingProber succeeds for every file but fails if the probe context is
// already done, so tests do not depend on a host ffprobe binary.
type ctxCheckingProber struct{}

func (ctxCheckingProber) Probe(ctx context.Context, path string) (metadata.ProbeResult, error) {
	if err := ctx.Err(); err != nil {
		return metadata.ProbeResult{}, err
	}
	return metadata.ProbeResult{}, nil
}

func TestScanRecordsErrorDetails(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	bad := filepath.Join(root, "[Group] Broken Show - 01.mkv")
	_ = os.WriteFile(bad, []byte("x"), 0o644)

	svc := NewService(db, failingProber{}, nil)
	lib, err := svc.CreateLibrary(ctx, "Anime", "anime", root)
	if err != nil {
		t.Fatal(err)
	}
	st, err := svc.ScanSync(ctx, lib.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Errors != 1 || len(st.ErrorSamples) != 1 {
		t.Fatalf("expected 1 recorded error sample, got %+v", st)
	}
	if st.ErrorSamples[0].Path != bad || st.ErrorSamples[0].Message == "" || st.LastError == "" {
		t.Fatalf("unexpected error detail: %+v", st.ErrorSamples)
	}
}

func TestScanSyncSurvivesCanceledRequestContext(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	for _, n := range []string{"[G] Show - 01.mkv", "[G] Show - 02.mkv", "[G] Show - 03.mkv"} {
		_ = os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644)
	}
	svc := NewService(db, ctxCheckingProber{}, nil)
	lib, err := svc.CreateLibrary(context.Background(), "Anime", "anime", root)
	if err != nil {
		t.Fatal(err)
	}

	// Simulate the browser disconnecting immediately after the request starts.
	reqCtx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	st, err := svc.ScanSync(reqCtx, lib.ID, true)
	if err != nil {
		t.Fatalf("scan should not fail when request ctx is canceled: %v", err)
	}
	if st.Errors != 0 || st.Added != 3 {
		t.Fatalf("expected 3 added with 0 errors, got %+v", st)
	}
}

type stubCreditsEnricher struct {
	movieCalls   int
	episodeCalls int
}

func (e *stubCreditsEnricher) EnrichMovie(_ context.Context, _, title string, _ *int) (metadata.EnrichedInfo, error) {
	e.movieCalls++
	return metadata.EnrichedInfo{
		Title:         title,
		Overview:      "A richly enriched movie.",
		ContentRating: "PG-13",
		ReleaseDate:   "2014-11-05",
		Directors:     []string{"Christopher Nolan"},
		Writers:       []string{"Jonathan Nolan"},
		Studios:       []string{"Syncopy"},
		Cast: []metadata.CreditPerson{
			{Name: "Matthew McConaughey", Role: "Cooper", Department: "cast", ProfilePath: "/mcc.jpg"},
		},
		TMDBID: "157336",
		IMDBID: "tt0816692",
	}, nil
}

func (e *stubCreditsEnricher) EnrichEpisode(_ context.Context, _, _ string, _, _ int, _ *int) (metadata.EnrichedInfo, error) {
	e.episodeCalls++
	return metadata.EnrichedInfo{
		Title:         "Pilot",
		Overview:      "An enriched episode.",
		ContentRating: "TV-14",
		ReleaseDate:   "2015-12-14",
		Directors:     []string{"Terry McDonough"},
		Writers:       []string{"Mark Fergus"},
		Studios:       []string{"Syfy"},
		Cast: []metadata.CreditPerson{
			{Name: "Steven Strait", Role: "Jim Holden", Department: "cast", ProfilePath: "/strait.jpg"},
		},
		TMDBID: "63639",
		IMDBID: "tt3230854",
	}, nil
}

func TestIncrementalScanAndStartupBackfillUpgradeStaleMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	moviesRoot := t.TempDir()
	movieDir := filepath.Join(moviesRoot, "Interstellar (2014)")
	_ = os.MkdirAll(movieDir, 0o755)
	_ = os.WriteFile(filepath.Join(movieDir, "Interstellar (2014).mp4"), []byte("movie"), 0o644)

	showsRoot := t.TempDir()
	showDir := filepath.Join(showsRoot, "The Expanse (2015)", "Season 01")
	_ = os.MkdirAll(showDir, 0o755)
	_ = os.WriteFile(filepath.Join(showDir, "The.Expanse.S01E01.mp4"), []byte("ep1"), 0o644)

	// 1. Initial scan without enricher simulates items indexed before credits metadata existed
	//    (or before TMDB was configured) — metadata_version stays 0.
	svc := NewService(db, metadata.NewFFProbe(), nil)
	movieLib, err := svc.CreateLibrary(ctx, "Movies", "movies", moviesRoot)
	if err != nil {
		t.Fatal(err)
	}
	showLib, err := svc.CreateLibrary(ctx, "TV Shows", "shows", showsRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ScanSync(ctx, movieLib.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ScanSync(ctx, showLib.ID, false); err != nil {
		t.Fatal(err)
	}

	// Verify both libraries report needing backfill.
	staleLibs, err := db.ListLibrariesNeedingMetadataBackfill(ctx, CurrentMetadataVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(staleLibs) != 2 {
		t.Fatalf("expected 2 libraries needing metadata backfill, got %v", staleLibs)
	}

	// 2. Attach enricher and trigger BackfillStaleLibraries (same path Watcher runs at startup).
	enricher := &stubCreditsEnricher{}
	svc.SetEnricher(enricher)
	if err := svc.BackfillStaleLibraries(ctx); err != nil {
		t.Fatal(err)
	}
	if enricher.movieCalls != 1 || enricher.episodeCalls != 1 {
		t.Fatalf("expected 1 movie and 1 episode enrichment call, got movie=%d episode=%d", enricher.movieCalls, enricher.episodeCalls)
	}

	// Verify rich metadata (cast, directors, studios, certification, IMDb) is now populated.
	items, _, err := svc.ListItems(ctx, store.MediaListFilter{LibraryID: movieLib.ID})
	if err != nil || len(items) != 1 {
		t.Fatalf("list movies: %v len=%d", err, len(items))
	}
	if items[0].ContentRating != "PG-13" || items[0].CastJSON == "[]" || items[0].DirectorsJSON == "[]" || items[0].MetadataVersion != CurrentMetadataVersion {
		t.Fatalf("movie metadata not backfilled: %+v", items[0])
	}

	epItems, _, err := svc.ListItems(ctx, store.MediaListFilter{LibraryID: showLib.ID})
	if err != nil || len(epItems) != 1 {
		t.Fatalf("list shows: %v len=%d", err, len(epItems))
	}
	if epItems[0].ContentRating != "TV-14" || epItems[0].IMDBID != "tt3230854" || epItems[0].CastJSON == "[]" || epItems[0].MetadataVersion != CurrentMetadataVersion {
		t.Fatalf("episode metadata not backfilled: %+v", epItems[0])
	}

	// 3. Subsequent incremental scans and backfill checks skip already-upgraded items cleanly.
	staleLibs, err = db.ListLibrariesNeedingMetadataBackfill(ctx, CurrentMetadataVersion)
	if err != nil {
		t.Fatal(err)
	}
	if len(staleLibs) != 0 {
		t.Fatalf("expected 0 libraries needing backfill after upgrade, got %v", staleLibs)
	}
	st, err := svc.ScanSync(ctx, movieLib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Skipped != 1 || st.Updated != 0 || enricher.movieCalls != 1 {
		t.Fatalf("expected second scan to skip backfilled movie without re-querying enricher, got scan=%+v calls=%d", st, enricher.movieCalls)
	}
}

func TestScanProgressTotal(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	for _, n := range []string{"[G] Show - 01.mkv", "[G] Show - 02.mkv", "[G] Show - 03.mkv"} {
		_ = os.WriteFile(filepath.Join(root, n), []byte("x"), 0o644)
	}
	svc := NewService(db, ctxCheckingProber{}, nil)
	lib, err := svc.CreateLibrary(ctx, "Anime", "anime", root)
	if err != nil {
		t.Fatal(err)
	}

	// Fresh scan: every file is processed.
	st, err := svc.ScanSync(ctx, lib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Scanned != 3 || st.Running {
		t.Fatalf("first scan: want total=3 scanned=3, got %+v", st)
	}

	// Incremental rescan: unchanged files are skipped but still count toward progress.
	st, err = svc.ScanSync(ctx, lib.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Scanned != 3 || st.Skipped != 3 {
		t.Fatalf("incremental scan: want total=3 scanned=3 skipped=3, got %+v", st)
	}

	// Forced rescan reprocesses everything and still ends at 100%.
	st, err = svc.ScanSync(ctx, lib.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if st.Total != 3 || st.Scanned != 3 || st.Skipped != 0 {
		t.Fatalf("forced scan: want total=3 scanned=3 skipped=0, got %+v", st)
	}
}

func TestScanCallsMediaChangedHookOnlyForFileChanges(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	root := t.TempDir()
	ep := filepath.Join(root, "[G] Show - 01.mkv")
	_ = os.WriteFile(ep, []byte("x"), 0o644)
	svc := NewService(db, ctxCheckingProber{}, nil)
	var calls int
	svc.SetMediaChangedHook(func() {
		if svc.HasRunningScan() {
			t.Error("hook ran before the scan was marked finished")
		}
		calls++
	})
	lib, err := svc.CreateLibrary(ctx, "Anime", "anime", root)
	if err != nil {
		t.Fatal(err)
	}

	scan := func(force bool) {
		t.Helper()
		if _, err := svc.ScanSync(ctx, lib.ID, force); err != nil {
			t.Fatal(err)
		}
	}
	scan(false)
	if calls != 1 {
		t.Fatalf("new file: hook calls = %d, want 1", calls)
	}
	scan(false)
	scan(true) // re-indexes unchanged files: metadata only
	if calls != 1 {
		t.Fatalf("unchanged files: hook calls = %d, want 1", calls)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(ep, later, later)
	scan(false)
	if calls != 2 {
		t.Fatalf("modified file: hook calls = %d, want 2", calls)
	}
}
