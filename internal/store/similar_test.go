package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestFindSimilarMediaMovies(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.CreateLibrary(ctx, Library{
		ID:       "lib-movies",
		Name:     "Movies",
		Kind:     "movies",
		RootPath: "/media/movies",
	}); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	seed := MediaItem{
		ID:            "m-seed",
		LibraryID:     "lib-movies",
		Kind:          "movie",
		Title:         "Inception",
		SortTitle:     "inception",
		FilePath:      "/media/movies/inception.mkv",
		Rating:        8.8,
		GenresJSON:    `["Sci-Fi","Thriller"]`,
		DirectorsJSON: `["Christopher Nolan"]`,
		WritersJSON:   `["Christopher Nolan"]`,
		StudiosJSON:   `["Syncopy"]`,
		CastJSON:      `[{"name":"Leonardo DiCaprio"},{"name":"Joseph Gordon-Levitt"},{"name":"Elliot Page"},{"name":"Tom Hardy"}]`,
	}
	// Completely unrelated movie in the same library — MUST NOT be returned.
	unrelated := MediaItem{
		ID:            "m-unrelated",
		LibraryID:     "lib-movies",
		Kind:          "movie",
		Title:         "Paddington",
		SortTitle:     "paddington",
		FilePath:      "/media/movies/paddington.mkv",
		Rating:        9.5,
		GenresJSON:    `["Family","Comedy"]`,
		DirectorsJSON: `["Paul King"]`,
		WritersJSON:   `["Paul King"]`,
		StudiosJSON:   `["StudioCanal"]`,
		CastJSON:      `[{"name":"Ben Whishaw"}]`,
	}
	// Shares director + writer + 1 genre + studio -> highest score.
	highMatch := MediaItem{
		ID:            "m-high",
		LibraryID:     "lib-movies",
		Kind:          "movie",
		Title:         "Interstellar",
		SortTitle:     "interstellar",
		FilePath:      "/media/movies/interstellar.mkv",
		Rating:        8.7,
		GenresJSON:    `["Sci-Fi","Drama"]`,
		DirectorsJSON: `["Christopher Nolan"]`,
		WritersJSON:   `["Christopher Nolan"]`,
		StudiosJSON:   `["Syncopy"]`,
		CastJSON:      `[{"name":"Matthew McConaughey"}]`,
	}
	// Shares only cast (2 actors -> 4 pts + 1 same-lib = 5).
	castMatch := MediaItem{
		ID:            "m-cast",
		LibraryID:     "lib-movies",
		Kind:          "movie",
		Title:         "The Revenant",
		SortTitle:     "revenant",
		FilePath:      "/media/movies/revenant.mkv",
		Rating:        8.0,
		GenresJSON:    `["Adventure","Western"]`,
		DirectorsJSON: `["Alejandro G. Inarritu"]`,
		CastJSON:      `[{"name":"Leonardo DiCaprio"},{"name":"Tom Hardy"}]`,
	}
	// Shares only 1 genre (3 pts + 1 same-lib = 4).
	genreMatch := MediaItem{
		ID:         "m-genre",
		LibraryID:  "lib-movies",
		Kind:       "movie",
		Title:      "Arrival",
		SortTitle:  "arrival",
		FilePath:   "/media/movies/arrival.mkv",
		Rating:     7.9,
		GenresJSON: `["Sci-Fi","Drama"]`,
	}

	for _, it := range []MediaItem{seed, unrelated, highMatch, castMatch, genreMatch} {
		if err := st.UpsertMediaItem(ctx, it); err != nil {
			t.Fatalf("UpsertMediaItem %s: %v", it.ID, err)
		}
	}

	items, series, err := st.FindSimilarMedia(ctx, seed, nil, false, 12)
	if err != nil {
		t.Fatalf("FindSimilarMedia: %v", err)
	}
	if len(series) != 0 {
		t.Fatalf("expected 0 series for movie seed, got %d", len(series))
	}
	if len(items) != 3 {
		t.Fatalf("expected 3 similar movies (excluding unrelated), got %d: %+v", len(items), items)
	}
	if items[0].ID != "m-high" || items[1].ID != "m-cast" || items[2].ID != "m-genre" {
		t.Fatalf("unexpected ranking order: got [%s, %s, %s], want [m-high, m-cast, m-genre]",
			items[0].ID, items[1].ID, items[2].ID)
	}

	// Unrelated movie as seed should return empty slice instead of random same-library movies.
	emptyItems, _, err := st.FindSimilarMedia(ctx, unrelated, nil, false, 12)
	if err != nil {
		t.Fatalf("FindSimilarMedia(unrelated): %v", err)
	}
	if len(emptyItems) != 0 {
		t.Fatalf("expected 0 similar items for unrelated movie, got %d", len(emptyItems))
	}
}

func TestFindSimilarMediaSeriesAndAnimationFilter(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "lune.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()

	if err := st.CreateLibrary(ctx, Library{ID: "lib-anime", Name: "Anime", Kind: "anime", RootPath: "/media/anime"}); err != nil {
		t.Fatalf("CreateLibrary anime: %v", err)
	}
	if err := st.CreateLibrary(ctx, Library{ID: "lib-shows", Name: "TV", Kind: "shows", RootPath: "/media/shows"}); err != nil {
		t.Fatalf("CreateLibrary shows: %v", err)
	}

	s1, e1 := 1, 1
	seed := MediaItem{
		ID:            "ep-frieren-1",
		LibraryID:     "lib-anime",
		Kind:          "episode",
		Title:         "The Journey's End",
		SortTitle:     "frieren 01",
		SeriesTitle:   "Frieren",
		SeasonNumber:  &s1,
		EpisodeNumber: &e1,
		FilePath:      "/media/anime/frieren/s01e01.mkv",
		GenresJSON:    `["Animation","Fantasy","Adventure"]`,
		DirectorsJSON: `["Keiichiro Saito"]`,
		StudiosJSON:   `["Madhouse"]`,
		CastJSON:      `[{"name":"Atsumi Tanezaki"}]`,
	}
	// Kids cartoon in a different library kind ("shows") that ONLY overlaps on "Animation" -> MUST NOT match.
	kidsCartoon := MediaItem{
		ID:            "ep-bluey-1",
		LibraryID:     "lib-shows",
		Kind:          "episode",
		Title:         "Magic Xylophone",
		SortTitle:     "bluey 01",
		SeriesTitle:   "Bluey",
		SeasonNumber:  &s1,
		EpisodeNumber: &e1,
		FilePath:      "/media/shows/bluey/s01e01.mkv",
		GenresJSON:    `["Animation","Family"]`,
		StudiosJSON:   `["Ludo Studio"]`,
	}
	// Unrelated live-action show in shows -> MUST NOT match.
	unrelatedShow := MediaItem{
		ID:            "ep-office-1",
		LibraryID:     "lib-shows",
		Kind:          "episode",
		Title:         "Pilot",
		SortTitle:     "office 01",
		SeriesTitle:   "The Office",
		SeasonNumber:  &s1,
		EpisodeNumber: &e1,
		FilePath:      "/media/shows/office/s01e01.mkv",
		GenresJSON:    `["Comedy"]`,
	}
	// Another anime sharing director + cast (even with no shared specific genre) -> MUST match via director & cast!
	sameDirectorAnime := MediaItem{
		ID:            "ep-bocchi-1",
		LibraryID:     "lib-anime",
		Kind:          "episode",
		Title:         "Rolling Bocchi",
		SortTitle:     "bocchi 01",
		SeriesTitle:   "Bocchi the Rock!",
		SeasonNumber:  &s1,
		EpisodeNumber: &e1,
		FilePath:      "/media/anime/bocchi/s01e01.mkv",
		GenresJSON:    `["Animation","Comedy"]`,
		DirectorsJSON: `["Keiichiro Saito"]`,
		CastJSON:      `[{"name":"Atsumi Tanezaki"}]`,
	}
	// Another anime sharing studio only -> matches below sameDirectorAnime.
	sameStudioAnime := MediaItem{
		ID:            "ep-deathnote-1",
		LibraryID:     "lib-anime",
		Kind:          "episode",
		Title:         "Rebirth",
		SortTitle:     "death note 01",
		SeriesTitle:   "Death Note",
		SeasonNumber:  &s1,
		EpisodeNumber: &e1,
		FilePath:      "/media/anime/deathnote/s01e01.mkv",
		GenresJSON:    `["Animation","Mystery"]`,
		StudiosJSON:   `["Madhouse"]`,
	}

	for _, it := range []MediaItem{seed, kidsCartoon, unrelatedShow, sameDirectorAnime, sameStudioAnime} {
		if err := st.UpsertMediaItem(ctx, it); err != nil {
			t.Fatalf("UpsertMediaItem %s: %v", it.ID, err)
		}
	}

	items, series, err := st.FindSimilarMedia(ctx, seed, nil, false, 12)
	if err != nil {
		t.Fatalf("FindSimilarMedia(series): %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected 0 movie items for episode seed, got %d", len(items))
	}
	if len(series) != 2 {
		t.Fatalf("expected 2 similar series (excluding Bluey & The Office), got %d: %+v", len(series), series)
	}
	if series[0].Title != "Bocchi the Rock!" || series[1].Title != "Death Note" {
		t.Fatalf("unexpected series ranking: got [%s, %s], want [Bocchi the Rock!, Death Note]",
			series[0].Title, series[1].Title)
	}
}
