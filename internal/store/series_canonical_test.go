package store

import (
	"context"
	"testing"
)

func TestNormalizeSeriesKey(t *testing.T) {
	cases := map[string]string{
		"I'm Looking for a Zombie": "im looking for zombie",
		"I’m Looking for Zombie":   "im looking for zombie",
		"Re:Zero":                  "re zero",
		"Re Zero":                  "re zero",
		"The Office":               "office",
		"A":                        "a",
	}
	for in, want := range cases {
		if got := NormalizeSeriesKey(in); got != want {
			t.Errorf("NormalizeSeriesKey(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPlanSeriesTitleMerges(t *testing.T) {
	rows := []SeriesTitleRow{
		// One TMDB show split across two spellings: majority (4 vs 3) wins.
		{ID: "z1", Title: "#I'm Looking for Zombie", TMDBID: "100", Season: 1, Episode: 1},
		{ID: "z2", Title: "#I'm Looking for Zombie", TMDBID: "100", Season: 1, Episode: 2},
		{ID: "z3", Title: "#I'm Looking for Zombie", TMDBID: "100", Season: 1, Episode: 3},
		{ID: "z4", Title: "#I'm Looking for a Zombie", TMDBID: "100", Season: 1, Episode: 4},
		{ID: "z5", Title: "#I'm Looking for a Zombie", TMDBID: "100", Season: 1, Episode: 5},
		{ID: "z6", Title: "#I'm Looking for a Zombie", TMDBID: "100", Season: 1, Episode: 6},
		{ID: "z7", Title: "#I'm Looking for a Zombie", TMDBID: "100", Season: 1, Episode: 7},
		// Unmatched episode using one of that show's spellings joins it.
		{ID: "z8", Title: "I'm Looking for Zombie", Season: 1, Episode: 8},
		// Tie: the earliest regular episode's title wins (specials sort last).
		{ID: "t0", Title: "Tie Show OVA", TMDBID: "200", Season: 0, Episode: 1},
		{ID: "t1", Title: "Tie Show", TMDBID: "200", Season: 1, Episode: 1},
		// Different TMDB shows with the same title stay as they are.
		{ID: "o1", Title: "Hunter x Hunter", TMDBID: "300", Season: 1, Episode: 1},
		{ID: "o2", Title: "Hunter x Hunter", TMDBID: "301", Season: 1, Episode: 1},
		// Unbound episodes merge by normalised title.
		{ID: "r1", Title: "Re:Zero", Season: 1, Episode: 1},
		{ID: "r2", Title: "Re Zero", Season: 1, Episode: 2},
		{ID: "r3", Title: "Re:Zero", Season: 1, Episode: 3},
		// Unrelated unbound show is untouched.
		{ID: "u1", Title: "Frieren", Season: 1, Episode: 1},
	}
	want := map[string]string{
		"z1": "#I'm Looking for a Zombie",
		"z2": "#I'm Looking for a Zombie",
		"z3": "#I'm Looking for a Zombie",
		"z8": "#I'm Looking for a Zombie",
		"t0": "Tie Show",
		"r2": "Re:Zero",
	}
	got := PlanSeriesTitleMerges(rows)
	if len(got) != len(want) {
		t.Fatalf("plan = %v, want %v", got, want)
	}
	for id, title := range want {
		if got[id] != title {
			t.Errorf("plan[%s] = %q, want %q (full plan %v)", id, got[id], title, got)
		}
	}
}

func TestCanonicalizeSeriesTitlesGroupsOneShow(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)
	lib := Library{ID: "lib-anime", Name: "Anime", Kind: "anime", RootPath: "/media/anime"}
	if err := s.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}
	intPtr := func(v int) *int { return &v }
	var items []MediaItem
	for i := 1; i <= 7; i++ {
		title := "I'm Looking for Zombie"
		if i >= 4 {
			title = "I'm Looking for a Zombie"
		}
		items = append(items, MediaItem{
			ID: "ep-" + string(rune('0'+i)), LibraryID: lib.ID, Kind: "episode",
			Title: "Episode", SeriesTitle: title, SeasonNumber: intPtr(1), EpisodeNumber: intPtr(i),
			FilePath: "/media/anime/zombie/" + string(rune('0'+i)) + ".mkv", TMDBID: "39283",
			StreamsJSON: "[]", SubtitlesJSON: "[]", ChaptersJSON: "[]", GenresJSON: "[]",
			DirectorsJSON: "[]", WritersJSON: "[]", StudiosJSON: "[]", CastJSON: "[]",
		})
	}
	if err := s.UpsertMediaItemsBatch(ctx, items); err != nil {
		t.Fatalf("UpsertMediaItemsBatch: %v", err)
	}

	before, err := s.ListSeries(ctx, lib.ID, "", "", nil, false)
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("expected the split to reproduce as 2 series before merging, got %d", len(before))
	}

	n, err := s.CanonicalizeSeriesTitles(ctx, lib.ID)
	if err != nil {
		t.Fatalf("CanonicalizeSeriesTitles: %v", err)
	}
	if n != 3 {
		t.Fatalf("renamed %d episodes, want 3", n)
	}
	after, err := s.ListSeries(ctx, lib.ID, "", "", nil, false)
	if err != nil {
		t.Fatalf("ListSeries: %v", err)
	}
	if len(after) != 1 || after[0].Title != "I'm Looking for a Zombie" || after[0].EpisodeCount != 7 {
		t.Fatalf("expected one 7-episode series, got %+v", after)
	}
	if n, err := s.CanonicalizeSeriesTitles(ctx, lib.ID); err != nil || n != 0 {
		t.Fatalf("second pass renamed %d (err %v), want 0", n, err)
	}
}
