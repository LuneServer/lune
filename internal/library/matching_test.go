package library

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

type stubMatchingEnricher struct {
	mu               sync.Mutex
	searchMovieCalls int
	searchEpCalls    int
	byIDMovieCalls   []string
	byIDEpCalls      []string
}

func (e *stubMatchingEnricher) EnrichMovie(_ context.Context, _, title string, _ *int) (metadata.EnrichedInfo, error) {
	e.mu.Lock()
	e.searchMovieCalls++
	e.mu.Unlock()
	return metadata.EnrichedInfo{
		Title:    title,
		Overview: "matched by search",
		TMDBID:   "111",
	}, nil
}

func (e *stubMatchingEnricher) EnrichEpisode(_ context.Context, _, seriesTitle string, _, _ int, _ *int) (metadata.EnrichedInfo, error) {
	e.mu.Lock()
	e.searchEpCalls++
	e.mu.Unlock()
	return metadata.EnrichedInfo{
		SeriesTitle: seriesTitle,
		Overview:    "matched by search",
		TMDBID:      "222",
	}, nil
}

func (e *stubMatchingEnricher) SearchMetadata(_ context.Context, _, _ string, _ *int) ([]metadata.MetadataSearchResult, error) {
	return nil, nil
}

func (e *stubMatchingEnricher) EnrichMovieByTMDBID(_ context.Context, _, tmdbID string) (metadata.EnrichedInfo, error) {
	e.mu.Lock()
	e.byIDMovieCalls = append(e.byIDMovieCalls, tmdbID)
	e.mu.Unlock()
	return metadata.EnrichedInfo{
		Title:    "Pinned Movie " + tmdbID,
		Overview: "matched directly by id " + tmdbID,
		TMDBID:   tmdbID,
	}, nil
}

func (e *stubMatchingEnricher) EnrichEpisodeByTMDBID(_ context.Context, _, tmdbID string, _, _, _ int, _ bool) (metadata.EnrichedInfo, error) {
	e.mu.Lock()
	e.byIDEpCalls = append(e.byIDEpCalls, tmdbID)
	e.mu.Unlock()
	return metadata.EnrichedInfo{
		SeriesTitle: "Pinned Series " + tmdbID,
		Overview:    "matched directly by id " + tmdbID,
		TMDBID:      tmdbID,
	}, nil
}

type noopMatchProber struct{}

func (noopMatchProber) Probe(_ context.Context, _ string) (metadata.ProbeResult, error) {
	return metadata.ProbeResult{Container: "mkv", DurationMS: 60000}, nil
}

func TestScanner_DirectIDLookupAndPinnedTMDBSurvival(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer db.Close()

	// 1. Movie with {tmdb-438631} path tag + plain movie to be manually pinned later.
	moviesRoot := t.TempDir()
	taggedDir := filepath.Join(moviesRoot, "Dune (2021) {tmdb-438631}")
	_ = os.MkdirAll(taggedDir, 0o755)
	_ = os.WriteFile(filepath.Join(taggedDir, "Dune (2021).mkv"), []byte("video"), 0o644)

	plainPath := filepath.Join(moviesRoot, "Arrival (2016).mkv")
	_ = os.WriteFile(plainPath, []byte("video"), 0o644)

	// 2. Show with parent tvshow.nfo carrying TMDB ID 63639.
	showsRoot := t.TempDir()
	showDir := filepath.Join(showsRoot, "The Expanse")
	seasonDir := filepath.Join(showDir, "Season 01")
	_ = os.MkdirAll(seasonDir, 0o755)
	_ = os.WriteFile(filepath.Join(showDir, "tvshow.nfo"), []byte(`<tvshow><title>The Expanse</title><uniqueid type="tmdb">63639</uniqueid></tvshow>`), 0o644)
	_ = os.WriteFile(filepath.Join(seasonDir, "The Expanse - S01E01.mkv"), []byte("video"), 0o644)

	enricher := &stubMatchingEnricher{}
	svc := NewService(db, noopMatchProber{}, nil)
	svc.SetEnricher(enricher)

	moviesLib, err := svc.CreateLibrary(ctx, "Movies", "movies", moviesRoot)
	if err != nil {
		t.Fatalf("CreateLibrary movies: %v", err)
	}
	showsLib, err := svc.CreateLibrary(ctx, "Shows", "shows", showsRoot)
	if err != nil {
		t.Fatalf("CreateLibrary shows: %v", err)
	}

	if _, err := svc.ScanSync(ctx, moviesLib.ID, false); err != nil {
		t.Fatalf("ScanSync movies: %v", err)
	}
	if _, err := svc.ScanSync(ctx, showsLib.ID, false); err != nil {
		t.Fatalf("ScanSync shows: %v", err)
	}

	if len(enricher.byIDMovieCalls) != 1 || enricher.byIDMovieCalls[0] != "438631" {
		t.Fatalf("expected direct EnrichMovieByTMDBID(438631) for tagged path, got %+v", enricher.byIDMovieCalls)
	}
	if enricher.searchMovieCalls != 1 {
		t.Fatalf("expected 1 search call for untagged movie, got %d", enricher.searchMovieCalls)
	}
	if len(enricher.byIDEpCalls) != 1 || enricher.byIDEpCalls[0] != "63639" || enricher.searchEpCalls != 0 {
		t.Fatalf("expected direct EnrichEpisodeByTMDBID(63639) from parent tvshow.nfo, got byID=%+v search=%d", enricher.byIDEpCalls, enricher.searchEpCalls)
	}

	// Manually re-match "Arrival" to TMDB ID 329865, then force re-scan the library.
	items, _, err := svc.ListItems(ctx, store.MediaListFilter{LibraryID: moviesLib.ID})
	if err != nil {
		t.Fatalf("ListItems: %v", err)
	}
	var arrivalID string
	for _, it := range items {
		if it.FilePath == plainPath {
			arrivalID = it.ID
			break
		}
	}
	if arrivalID == "" {
		t.Fatal("could not find Arrival item")
	}

	if _, err := svc.RefreshItem(ctx, arrivalID, "", nil, "329865"); err != nil {
		t.Fatalf("RefreshItem with TMDB override: %v", err)
	}

	// Force re-scan must preserve the manually pinned TMDB ID 329865 instead of falling back to title search.
	enricher.mu.Lock()
	enricher.searchMovieCalls = 0
	enricher.byIDMovieCalls = nil
	enricher.mu.Unlock()

	if _, err := svc.ScanSync(ctx, moviesLib.ID, true); err != nil {
		t.Fatalf("force ScanSync movies: %v", err)
	}

	updated, err := svc.GetItem(ctx, arrivalID)
	if err != nil {
		t.Fatalf("GetItem: %v", err)
	}
	if updated.TMDBID != "329865" {
		t.Fatalf("expected pinned TMDB ID 329865 to survive force re-scan, got %q", updated.TMDBID)
	}
	if enricher.searchMovieCalls != 0 {
		t.Fatalf("expected 0 title-search calls on force re-scan of already-matched items, got %d", enricher.searchMovieCalls)
	}
}
