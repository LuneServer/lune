package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestWidescreenResolutionFiltersAndFacets(t *testing.T) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer s.Close()

	if err := s.CreateLibrary(ctx, Library{
		ID:       "lib-movies",
		Name:     "Movies",
		Kind:     "movies",
		RootPath: "/media/movies",
	}); err != nil {
		t.Fatalf("create movies library: %v", err)
	}
	if err := s.CreateLibrary(ctx, Library{
		ID:       "lib-shows",
		Name:     "Shows",
		Kind:     "shows",
		RootPath: "/media/shows",
	}); err != nil {
		t.Fatalf("create shows library: %v", err)
	}

	items := []MediaItem{
		{
			ID:          "m-scope-1080p-a",
			LibraryID:   "lib-movies",
			Kind:        "movie",
			Title:       "Scope Feature A",
			FilePath:    "/media/movies/Scope Feature A (2013).mkv",
			VideoWidth:  1920,
			VideoHeight: 800,
		},
		{
			ID:          "m-scope-1080p-b",
			LibraryID:   "lib-movies",
			Kind:        "movie",
			Title:       "Scope Feature B",
			FilePath:    "/media/movies/Scope Feature B (2009).mkv",
			VideoWidth:  1920,
			VideoHeight: 818,
		},
		{
			ID:          "m-scope-4k",
			LibraryID:   "lib-movies",
			Kind:        "movie",
			Title:       "Scope 4K Film",
			FilePath:    "/media/movies/Scope 4K Film.mkv",
			VideoWidth:  3840,
			VideoHeight: 1600,
		},
		{
			ID:          "m-scope-720p",
			LibraryID:   "lib-movies",
			Kind:        "movie",
			Title:       "Scope 720p Film",
			FilePath:    "/media/movies/Scope 720p Film.mkv",
			VideoWidth:  1280,
			VideoHeight: 536,
		},
		{
			ID:          "m-academy-1080p",
			LibraryID:   "lib-movies",
			Kind:        "movie",
			Title:       "Academy 1080p Film",
			FilePath:    "/media/movies/Academy 1080p Film.mkv",
			VideoWidth:  1440,
			VideoHeight: 1080,
		},
		{
			ID:          "ep-1",
			LibraryID:   "lib-shows",
			Kind:        "episode",
			Title:       "Pilot",
			SeriesTitle: "Scope Show",
			FilePath:    "/media/shows/Scope Show/S01E01.mkv",
			VideoWidth:  1920,
			VideoHeight: 800,
		},
	}
	for _, it := range items {
		if err := s.UpsertMediaItem(ctx, it); err != nil {
			t.Fatalf("upsert %s: %v", it.ID, err)
		}
	}

	got4K, _, err := s.ListMediaItems(ctx, MediaListFilter{LibraryID: "lib-movies", Resolution: "4k"})
	if err != nil {
		t.Fatalf("list 4k: %v", err)
	}
	if len(got4K) != 1 || got4K[0].ID != "m-scope-4k" {
		t.Fatalf("expected only m-scope-4k in 4k filter, got %+v", got4K)
	}

	got1080, _, err := s.ListMediaItems(ctx, MediaListFilter{LibraryID: "lib-movies", Resolution: "1080p"})
	if err != nil {
		t.Fatalf("list 1080p: %v", err)
	}
	if len(got1080) != 3 {
		t.Fatalf("expected 3 items in 1080p filter (1920x800, 1920x818, 1440x1080), got %d", len(got1080))
	}

	got720, _, err := s.ListMediaItems(ctx, MediaListFilter{LibraryID: "lib-movies", Resolution: "720p"})
	if err != nil {
		t.Fatalf("list 720p: %v", err)
	}
	if len(got720) != 1 || got720[0].ID != "m-scope-720p" {
		t.Fatalf("expected only m-scope-720p in 720p filter, got %+v", got720)
	}

	facets, err := s.GetLibraryFacets(ctx, "lib-movies")
	if err != nil {
		t.Fatalf("get facets: %v", err)
	}
	if !facets.Has4K {
		t.Fatalf("expected Has4K=true for 3840x1600 widescreen 4K item")
	}

	series1080, err := s.ListSeriesFiltered(ctx, SeriesListFilter{LibraryID: "lib-shows", Resolution: "1080p"})
	if err != nil {
		t.Fatalf("list series 1080p: %v", err)
	}
	if len(series1080) != 1 || series1080[0].VideoWidth != 1920 || series1080[0].VideoHeight != 800 {
		t.Fatalf("expected Scope Show (1920x800) in 1080p series filter, got %+v", series1080)
	}
}

func TestUserPreferencesPosterBadgesDefaultNone(t *testing.T) {
	prefs := DefaultUserPreferences()
	if len(prefs.PosterBadges) != 0 {
		t.Fatalf("expected default poster_badges to be empty, got %v", prefs.PosterBadges)
	}
	parsed := ParseUserPreferences(`{"autoplay_next":true}`)
	if len(parsed.PosterBadges) != 0 {
		t.Fatalf("expected parsed legacy prefs poster_badges to be empty, got %v", parsed.PosterBadges)
	}
	parsedWithBadges := ParseUserPreferences(`{"poster_badges":["resolution","hdr"]}`)
	if len(parsedWithBadges.PosterBadges) != 2 || parsedWithBadges.PosterBadges[0] != "resolution" || parsedWithBadges.PosterBadges[1] != "hdr" {
		t.Fatalf("expected parsed poster_badges [resolution hdr], got %v", parsedWithBadges.PosterBadges)
	}
}
