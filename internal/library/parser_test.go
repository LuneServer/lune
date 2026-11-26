package library

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseMediaPath_MovieAndEpisode(t *testing.T) {
	root := t.TempDir()

	// Movie with sidecar poster
	movieDir := filepath.Join(root, "movies", "The Martian (2015)")
	_ = os.MkdirAll(movieDir, 0o755)
	posterPath := filepath.Join(movieDir, "poster.jpg")
	_ = os.WriteFile(posterPath, []byte("jpg"), 0o644)
	movieFile := filepath.Join(movieDir, "The.Martian.2015.2160p.UHD.BluRay.Remux.mkv")

	parsed := ParseMediaPath(filepath.Join(root, "movies"), movieFile, "movies")
	if parsed.Kind != "movie" || parsed.Title != "The Martian" || parsed.SortTitle != "Martian" {
		t.Fatalf("unexpected movie parse: %+v", parsed)
	}
	if parsed.Year == nil || *parsed.Year != 2015 {
		t.Fatalf("expected year 2015, got %v", parsed.Year)
	}
	if parsed.PosterPath != posterPath {
		t.Fatalf("expected poster %s, got %s", posterPath, parsed.PosterPath)
	}

	// TV Episode inheriting series poster from parent folder
	showRoot := filepath.Join(root, "shows")
	seriesDir := filepath.Join(showRoot, "The Expanse (2015)")
	seasonDir := filepath.Join(seriesDir, "Season 01")
	_ = os.MkdirAll(seasonDir, 0o755)
	seriesPoster := filepath.Join(seriesDir, "poster.png")
	_ = os.WriteFile(seriesPoster, []byte("png"), 0o644)
	epFile := filepath.Join(seasonDir, "The.Expanse.S01E04.CQB.2160p.WEB-DL.mkv")

	ep := ParseMediaPath(showRoot, epFile, "shows")
	if ep.Kind != "episode" || ep.SeriesTitle != "The Expanse" || ep.Title != "CQB" {
		t.Fatalf("unexpected episode parse: %+v", ep)
	}
	if ep.SeasonNumber == nil || *ep.SeasonNumber != 1 || ep.EpisodeNumber == nil || *ep.EpisodeNumber != 4 {
		t.Fatalf("expected S01E04, got S%vE%v", ep.SeasonNumber, ep.EpisodeNumber)
	}
	if ep.PosterPath != seriesPoster {
		t.Fatalf("expected inherited series poster %s, got %s", seriesPoster, ep.PosterPath)
	}
}

func TestParseMediaPath_MoviesWithYearsInTitle(t *testing.T) {
	root := filepath.Join(t.TempDir(), "movies")

	cases := []struct {
		relPath   string
		wantTitle string
		wantYear  int
	}{
		{
			relPath:   "Blade Runner 2049 (2017)/Blade Runner 2049 (2017).mp4",
			wantTitle: "Blade Runner 2049",
			wantYear:  2017,
		},
		{
			relPath:   "2001 A Space Odyssey (1968)/2001 A Space Odyssey (1968).mp4",
			wantTitle: "2001 A Space Odyssey",
			wantYear:  1968,
		},
		{
			relPath:   "1917 (2019)/1917 (2019).mkv",
			wantTitle: "1917",
			wantYear:  2019,
		},
		{
			relPath:   "Blade.Runner.2049.2017.2160p.BluRay.mkv",
			wantTitle: "Blade Runner 2049",
			wantYear:  2017,
		},
		{
			relPath:   "1984 (1984)/1984.mkv",
			wantTitle: "1984",
			wantYear:  1984,
		},
	}

	for _, tc := range cases {
		full := filepath.Join(root, filepath.FromSlash(tc.relPath))
		got := ParseMediaPath(root, full, "movies")
		if got.Kind != "movie" || got.Title != tc.wantTitle {
			t.Fatalf("%s: got title=%q kind=%q, want %q", tc.relPath, got.Title, got.Kind, tc.wantTitle)
		}
		if got.Year == nil || *got.Year != tc.wantYear {
			t.Fatalf("%s: got year=%v, want %d", tc.relPath, got.Year, tc.wantYear)
		}
	}
}

func TestParseMediaPath_AnimeConventions(t *testing.T) {
	root := filepath.Join(t.TempDir(), "anime")

	cases := []struct {
		name       string
		relPath    string
		wantKind   string
		wantSeries string
		wantTitle  string
		wantSeason int
		wantEp     int
		wantAbs    int
		wantYear   int
		wantGroup  string
	}{
		{
			name:       "SubsPlease fansub absolute",
			relPath:    "Sousou no Frieren/[SubsPlease] Sousou no Frieren - 01 (1080p) [A1B2C3D4].mkv",
			wantKind:   "episode",
			wantSeries: "Sousou no Frieren",
			wantTitle:  "Episode 01",
			wantSeason: 1,
			wantEp:     1,
			wantAbs:    1,
			wantGroup:  "SubsPlease",
		},
		{
			name:       "Erai-raws ordinal 2nd Season",
			relPath:    "Jujutsu Kaisen/[Erai-raws] Jujutsu Kaisen 2nd Season - 14 [1080p][Multiple Subtitle].mkv",
			wantKind:   "episode",
			wantSeries: "Jujutsu Kaisen",
			wantTitle:  "Episode 14",
			wantSeason: 2,
			wantEp:     14,
			wantGroup:  "Erai-raws",
		},
		{
			name:       "EMBER SxxExx with v2 and episode title",
			relPath:    "Attack on Titan/[EMBER] Attack on Titan (2013) - S04E28v2 - The Dawn of Humanity [1080p][Dual Audio].mkv",
			wantKind:   "episode",
			wantSeries: "Attack on Titan",
			wantTitle:  "The Dawn of Humanity",
			wantSeason: 4,
			wantEp:     28,
			wantYear:   2013,
			wantGroup:  "EMBER",
		},
		{
			name:       "Long-running 4-digit absolute episode",
			relPath:    "One Piece/One Piece - 1071 [1080p].mkv",
			wantKind:   "episode",
			wantSeries: "One Piece",
			wantTitle:  "Episode 1071",
			wantSeason: 1,
			wantEp:     1071,
			wantAbs:    1071,
		},
		{
			name:       "Season folder with bare episode number",
			relPath:    "Frieren (2023)/Season 01/01 - The Journey's End.mkv",
			wantKind:   "episode",
			wantSeries: "Frieren",
			wantTitle:  "The Journey's End",
			wantSeason: 1,
			wantEp:     1,
			wantYear:   2023,
		},
		{
			name:       "OVA mapped to Season 0",
			relPath:    "Vinland Saga/[Judas] Vinland Saga - OVA 01 [1080p].mkv",
			wantKind:   "episode",
			wantSeries: "Vinland Saga",
			wantTitle:  "OVA 01",
			wantSeason: 0,
			wantEp:     1,
			wantGroup:  "Judas",
		},
		{
			name:      "Standalone anime movie in anime library",
			relPath:   "Spirited Away (2001)/[EMBER] Spirited Away (2001) [1080p] [Dual Audio].mkv",
			wantKind:  "movie",
			wantTitle: "Spirited Away",
			wantYear:  2001,
			wantGroup: "EMBER",
		},
		{
			name:      "Anime movie inside Movies subfolder",
			relPath:   "Movies/Your Name (2016) [BDRip 1080p FLAC].mkv",
			wantKind:  "movie",
			wantTitle: "Your Name",
			wantYear:  2016,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			full := filepath.Join(root, filepath.FromSlash(tc.relPath))
			got := ParseMediaPath(root, full, "anime")
			if got.Kind != tc.wantKind {
				t.Fatalf("kind = %q, want %q (parsed: %+v)", got.Kind, tc.wantKind, got)
			}
			if got.SeriesTitle != tc.wantSeries {
				t.Fatalf("series = %q, want %q", got.SeriesTitle, tc.wantSeries)
			}
			if got.Title != tc.wantTitle {
				t.Fatalf("title = %q, want %q", got.Title, tc.wantTitle)
			}
			if tc.wantKind == "episode" {
				if got.SeasonNumber == nil || *got.SeasonNumber != tc.wantSeason {
					t.Fatalf("season = %v, want %d", got.SeasonNumber, tc.wantSeason)
				}
				if got.EpisodeNumber == nil || *got.EpisodeNumber != tc.wantEp {
					t.Fatalf("episode = %v, want %d", got.EpisodeNumber, tc.wantEp)
				}
			}
			if tc.wantAbs > 0 {
				if got.AbsoluteEpisodeNumber == nil || *got.AbsoluteEpisodeNumber != tc.wantAbs {
					t.Fatalf("abs episode = %v, want %d", got.AbsoluteEpisodeNumber, tc.wantAbs)
				}
			}
			if tc.wantYear > 0 {
				if got.Year == nil || *got.Year != tc.wantYear {
					t.Fatalf("year = %v, want %d", got.Year, tc.wantYear)
				}
			}
			if got.ReleaseGroup != tc.wantGroup {
				t.Fatalf("group = %q, want %q", got.ReleaseGroup, tc.wantGroup)
			}
		})
	}
}

func TestDiscoverSubtitles_ASSAndSigns(t *testing.T) {
	dir := t.TempDir()
	media := filepath.Join(dir, "[SubsPlease] Frieren - 01 (1080p).mkv")
	_ = os.WriteFile(media, []byte("mkv"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "[SubsPlease] Frieren - 01 (1080p).en.ass"), []byte("[Events]"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "[SubsPlease] Frieren - 01 (1080p).en.signs.ass"), []byte("[Events]"), 0o644)

	tracks := DiscoverSubtitles(media)
	if len(tracks) != 2 {
		t.Fatalf("expected 2 .ass sidecar tracks, got %d: %+v", len(tracks), tracks)
	}
	foundSigns := false
	for _, tr := range tracks {
		if tr.Format != "ass" {
			t.Fatalf("expected format ass, got %q", tr.Format)
		}
		if tr.Forced && tr.Label == "English (Signs & Songs)" {
			foundSigns = true
		}
	}
	if !foundSigns {
		t.Fatalf("expected Signs & Songs forced track, got %+v", tracks)
	}
}

func TestParseMediaPath_ExternalIDTags(t *testing.T) {
	movieRoot := filepath.Join(t.TempDir(), "movies")
	m1 := ParseMediaPath(movieRoot, filepath.Join(movieRoot, "Dune (2021) {tmdb-438631}", "Dune (2021) {tmdb-438631}.mkv"), "movies")
	if m1.Title != "Dune" || m1.Year == nil || *m1.Year != 2021 || m1.TMDBID != "438631" {
		t.Fatalf("unexpected curly tmdb parse: %+v", m1)
	}

	m2 := ParseMediaPath(movieRoot, filepath.Join(movieRoot, "[tmdbid-129] Spirited Away (2001) {imdb-tt0245429}.mkv"), "movies")
	if m2.Title != "Spirited Away" || m2.TMDBID != "129" || m2.IMDBID != "tt0245429" || m2.ReleaseGroup != "" {
		t.Fatalf("unexpected bracket tmdbid + imdb parse: %+v", m2)
	}

	showRoot := filepath.Join(t.TempDir(), "shows")
	ep := ParseMediaPath(showRoot, filepath.Join(showRoot, "The Expanse (2015) [tmdbid-63639]", "Season 01", "S01E04 - CQB.mkv"), "shows")
	if ep.SeriesTitle != "The Expanse" || ep.TMDBID != "63639" || ep.SeasonNumber == nil || *ep.SeasonNumber != 1 || ep.EpisodeNumber == nil || *ep.EpisodeNumber != 4 {
		t.Fatalf("unexpected series folder tmdbid inheritance: %+v", ep)
	}
}
