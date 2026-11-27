package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestUserPreferencesCombineNextUp(t *testing.T) {
	if !DefaultUserPreferences().CombineNextUp {
		t.Fatal("expected DefaultUserPreferences().CombineNextUp == true")
	}
	if !ParseUserPreferences("{}").CombineNextUp {
		t.Fatal("expected ParseUserPreferences(\"{}\").CombineNextUp == true")
	}
	if !ParseUserPreferences("").CombineNextUp {
		t.Fatal("expected ParseUserPreferences(\"\").CombineNextUp == true")
	}
	if !ParseUserPreferences(`{"autoplay_next":false}`).CombineNextUp {
		t.Fatal("expected ParseUserPreferences without combine_next_up key to default to true")
	}
	if ParseUserPreferences(`{"combine_next_up":false}`).CombineNextUp {
		t.Fatal("expected ParseUserPreferences({\"combine_next_up\":false}).CombineNextUp == false")
	}
}

func TestListNextUp(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "lune.db")
	s, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	// Create user and two libraries.
	user := User{
		ID:           "user-1",
		Username:     "dan",
		DisplayName:  "Dan",
		PasswordHash: "hash",
		Role:         "member",
		AllLibraries: true,
	}
	if err := s.CreateUserWithLibraries(ctx, user, nil); err != nil {
		t.Fatalf("CreateUserWithLibraries: %v", err)
	}
	lib1 := Library{ID: "lib-shows", Name: "Shows", Kind: "shows", RootPath: "/media/shows"}
	lib2 := Library{ID: "lib-anime", Name: "Anime", Kind: "anime", RootPath: "/media/anime"}
	if err := s.CreateLibrary(ctx, lib1); err != nil {
		t.Fatalf("CreateLibrary lib1: %v", err)
	}
	if err := s.CreateLibrary(ctx, lib2); err != nil {
		t.Fatalf("CreateLibrary lib2: %v", err)
	}

	intPtr := func(v int) *int { return &v }
	boolPtr := func(v bool) *bool { return &v }

	items := []MediaItem{
		// Show A (lib1): S01E01 completed -> S01E02 should be Next Up.
		{ID: "a-s1e1", LibraryID: lib1.ID, Kind: "episode", Title: "Pilot", SortTitle: "pilot", SeriesTitle: "Show A", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(1), FilePath: "/media/shows/a1.mkv", DurationMS: 1000},
		{ID: "a-s1e2", LibraryID: lib1.ID, Kind: "episode", Title: "Second", SortTitle: "second", SeriesTitle: "Show A", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(2), FilePath: "/media/shows/a2.mkv", DurationMS: 1000},
		// Show B (lib1): Entire series completed -> omitted from Next Up.
		{ID: "b-s1e1", LibraryID: lib1.ID, Kind: "episode", Title: "Only Ep", SortTitle: "only ep", SeriesTitle: "Show B", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(1), FilePath: "/media/shows/b1.mkv", DurationMS: 1000},
		// Show C (lib1): S01E01 completed, S01E02 in-progress -> not duplicated in Next Up.
		{ID: "c-s1e1", LibraryID: lib1.ID, Kind: "episode", Title: "Ep 1", SortTitle: "ep 1", SeriesTitle: "Show C", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(1), FilePath: "/media/shows/c1.mkv", DurationMS: 1000},
		{ID: "c-s1e2", LibraryID: lib1.ID, Kind: "episode", Title: "Ep 2", SortTitle: "ep 2", SeriesTitle: "Show C", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(2), FilePath: "/media/shows/c2.mkv", DurationMS: 1000},
		{ID: "c-s1e3", LibraryID: lib1.ID, Kind: "episode", Title: "Ep 3", SortTitle: "ep 3", SeriesTitle: "Show C", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(3), FilePath: "/media/shows/c3.mkv", DurationMS: 1000},
		// Show D (lib2): S01E01 completed, bridges to S02E01 -> S02E01 should be Next Up (and tests library ACL).
		{ID: "d-s1e1", LibraryID: lib2.ID, Kind: "episode", Title: "S1 Finale", SortTitle: "s1 finale", SeriesTitle: "Show D", SeasonNumber: intPtr(1), EpisodeNumber: intPtr(1), FilePath: "/media/anime/d1.mkv", DurationMS: 1000},
		{ID: "d-s2e1", LibraryID: lib2.ID, Kind: "episode", Title: "S2 Premiere", SortTitle: "s2 premiere", SeriesTitle: "Show D", SeasonNumber: intPtr(2), EpisodeNumber: intPtr(1), FilePath: "/media/anime/d2.mkv", DurationMS: 1000},
	}
	if err := s.UpsertMediaItemsBatch(ctx, items); err != nil {
		t.Fatalf("UpsertMediaItemsBatch: %v", err)
	}

	// Mark Show A S01E01 completed first.
	progA, err := s.UpsertPlaybackProgress(ctx, user.ID, "a-s1e1", 1000, 1000, boolPtr(true))
	if err != nil {
		t.Fatalf("progress a-s1e1: %v", err)
	}
	// Mark Show B S01E01 completed.
	if _, err := s.UpsertPlaybackProgress(ctx, user.ID, "b-s1e1", 1000, 1000, boolPtr(true)); err != nil {
		t.Fatalf("progress b-s1e1: %v", err)
	}
	// Mark Show C S01E01 completed and S01E02 in-progress.
	if _, err := s.UpsertPlaybackProgress(ctx, user.ID, "c-s1e1", 1000, 1000, boolPtr(true)); err != nil {
		t.Fatalf("progress c-s1e1: %v", err)
	}
	if _, err := s.UpsertPlaybackProgress(ctx, user.ID, "c-s1e2", 400, 1000, boolPtr(false)); err != nil {
		t.Fatalf("progress c-s1e2: %v", err)
	}
	// Sleep briefly so Show D has a strictly newer last_played_at timestamp.
	time.Sleep(15 * time.Millisecond)
	progD, err := s.UpsertPlaybackProgress(ctx, user.ID, "d-s1e1", 1000, 1000, boolPtr(true))
	if err != nil {
		t.Fatalf("progress d-s1e1: %v", err)
	}

	// Unrestricted (allowedLibraryIDs == nil): Show D (S02E01) first, then Show A (S01E02).
	entries, err := s.ListNextUp(ctx, user.ID, nil, 20)
	if err != nil {
		t.Fatalf("ListNextUp unrestricted: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 next-up entries, got %d: %+v", len(entries), entries)
	}
	if entries[0].Item.ID != "d-s2e1" {
		t.Fatalf("expected first next-up to be d-s2e1, got %s", entries[0].Item.ID)
	}
	if !entries[0].LastPlayedAt.Equal(progD.LastPlayedAt) {
		t.Fatalf("expected d-s2e1 LastPlayedAt %v, got %v", progD.LastPlayedAt, entries[0].LastPlayedAt)
	}
	if entries[1].Item.ID != "a-s1e2" {
		t.Fatalf("expected second next-up to be a-s1e2, got %s", entries[1].Item.ID)
	}
	if !entries[1].LastPlayedAt.Equal(progA.LastPlayedAt) {
		t.Fatalf("expected a-s1e2 LastPlayedAt %v, got %v", progA.LastPlayedAt, entries[1].LastPlayedAt)
	}

	// Restricted to lib1 only: only Show A (S01E02) returned.
	restricted, err := s.ListNextUp(ctx, user.ID, []string{lib1.ID}, 20)
	if err != nil {
		t.Fatalf("ListNextUp restricted lib1: %v", err)
	}
	if len(restricted) != 1 || restricted[0].Item.ID != "a-s1e2" {
		t.Fatalf("expected only a-s1e2 for lib1 ACL, got %+v", restricted)
	}

	// Restricted to empty slice: empty result.
	empty, err := s.ListNextUp(ctx, user.ID, []string{}, 20)
	if err != nil {
		t.Fatalf("ListNextUp empty ACL: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("expected 0 entries for empty ACL, got %d", len(empty))
	}
}

func TestUserPreferencesShowChapterMarkers(t *testing.T) {
	if !DefaultUserPreferences().ShowChapterMarkers {
		t.Fatal("expected DefaultUserPreferences().ShowChapterMarkers == true")
	}
	for _, raw := range []string{"", "{}", `{"combine_next_up":false}`} {
		if !ParseUserPreferences(raw).ShowChapterMarkers {
			t.Fatalf("expected ParseUserPreferences(%q).ShowChapterMarkers == true", raw)
		}
	}
	if ParseUserPreferences(`{"show_chapter_markers":false}`).ShowChapterMarkers {
		t.Fatal("expected show_chapter_markers:false to decode as false")
	}
}
