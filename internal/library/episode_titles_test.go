package library

import (
	"path/filepath"
	"testing"
)

func TestCleanEpisodeTitle(t *testing.T) {
	one := 1
	twelve := 12
	cases := []struct {
		raw  string
		ep   *int
		want string
	}{
		{"001 - My Fateful Encounter", &one, "My Fateful Encounter"},
		{"- 001 - My Fateful Encounter +SubsPlus-", &one, "My Fateful Encounter"},
		{"My Fateful Encounter +SubsPlus- [1080p]", &one, "My Fateful Encounter"},
		{"My.Fateful.Encounter.CR.WEB-DL.MULTi.1080p", &one, "My Fateful Encounter"},
		{"E01 - Please Adopt Me", &one, "Please Adopt Me"},
		{"#12 Delicious Gyokuro Tea", &twelve, "Delicious Gyokuro Tea"},
		{"12 Delicious Gyokuro Tea", &twelve, "Delicious Gyokuro Tea"},
		{"001", &one, ""},
		{"+SubsPlus-", &one, ""},
		{"1000 Years Later", &one, "1000 Years Later"},
		{"86 Days", &twelve, "86 Days"},
		{"The Journey's End (BD 1080p HEVC)", &one, "The Journey's End"},
	}
	for _, tc := range cases {
		if got := cleanEpisodeTitle(tc.raw, tc.ep); got != tc.want {
			t.Errorf("cleanEpisodeTitle(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestParseMediaPath_EpisodeTitleCleanup(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	cases := []struct {
		kind, rel, wantSeries, wantTitle string
	}{
		{"anime", "Zombie/[Group] I'm Looking for a Zombie - S01E01 - 001 - My Fateful Encounter +SubsPlus- [1080p].mkv", "I'm Looking for a Zombie", "My Fateful Encounter"},
		{"anime", "Zombie/[Group] I'm Looking for a Zombie - 01 - 001 [1080p].mkv", "I'm Looking for a Zombie", "Episode 01"},
		{"anime", "Zombie/I'm Looking for a Zombie - 02 - Please Adopt Me +SubsPlus-.mkv", "I'm Looking for a Zombie", "Please Adopt Me"},
		{"shows", "Show/Season 01/Show - S01E03 - 003 - Delicious Gyokuro Tea.mkv", "Show", "Delicious Gyokuro Tea"},
	}
	for _, tc := range cases {
		got := ParseMediaPath(root, filepath.Join(root, filepath.FromSlash(tc.rel)), tc.kind)
		if got.SeriesTitle != tc.wantSeries || got.Title != tc.wantTitle {
			t.Errorf("%s: series=%q title=%q, want series=%q title=%q", tc.rel, got.SeriesTitle, got.Title, tc.wantSeries, tc.wantTitle)
		}
	}
}

func TestPreferRemoteEpisodeTitle(t *testing.T) {
	cases := []struct {
		local, remote string
		nfo           bool
		want          bool
	}{
		{"Episode 01", "My Fateful Encounter", false, true},
		{"S01E01", "My Fateful Encounter", false, true},
		{"001", "My Fateful Encounter", false, true},
		{"Fansub Translation", "My Fateful Encounter", false, true},
		{"Curated NFO Title", "My Fateful Encounter", true, false},
		{"Episode 01", "My Fateful Encounter", true, true},
		{"Fansub Translation", "Episode 1", false, false},
		{"Secrets", "", false, false},
	}
	for _, tc := range cases {
		if got := preferRemoteEpisodeTitle(tc.local, tc.remote, tc.nfo); got != tc.want {
			t.Errorf("preferRemoteEpisodeTitle(%q, %q, nfo=%v) = %v, want %v", tc.local, tc.remote, tc.nfo, got, tc.want)
		}
	}
	if isGenericEpisodeTitle("Secrets of the Deep") || isGenericEpisodeTitle("Sky High") {
		t.Error("titles starting with S must not be treated as generic")
	}
}
