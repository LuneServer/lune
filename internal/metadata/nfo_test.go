package metadata

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseLocalNFO(t *testing.T) {
	dir := t.TempDir()
	mediaFile := filepath.Join(dir, "Interstellar (2014).mkv")
	nfoFile := filepath.Join(dir, "Interstellar (2014).nfo")

	xmlDoc := `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<movie>
  <title>Interstellar</title>
  <tagline>Mankind was born on Earth. It was never meant to die here.</tagline>
  <plot>A team of explorers travel through a wormhole in space in an attempt to ensure humanity's survival.</plot>
  <year>2014</year>
  <rating>8.7</rating>
  <mpaa>PG-13</mpaa>
  <premiered>2014-11-05</premiered>
  <genre>Science Fiction</genre>
  <genre>Drama</genre>
  <director>Christopher Nolan</director>
  <credits>Jonathan Nolan</credits>
  <studio>Syncopy</studio>
  <actor>
    <name>Matthew McConaughey</name>
    <role>Cooper</role>
  </actor>
  <uniqueid type="tmdb">157336</uniqueid>
  <uniqueid type="imdb">tt0816692</uniqueid>
</movie>`
	if err := os.WriteFile(nfoFile, []byte(xmlDoc), 0o644); err != nil {
		t.Fatalf("write nfo: %v", err)
	}

	info, ok := ParseLocalNFO(mediaFile)
	if !ok {
		t.Fatal("expected ParseLocalNFO to find and parse sidecar .nfo")
	}
	if info.Title != "Interstellar" || info.TMDBID != "157336" || info.IMDBID != "tt0816692" {
		t.Fatalf("unexpected NFO identity: %+v", info)
	}
	if info.Year == nil || *info.Year != 2014 || info.Rating != 8.7 || len(info.Genres) != 2 {
		t.Fatalf("unexpected NFO metadata: %+v", info)
	}
	if info.ContentRating != "PG-13" || info.ReleaseDate != "2014-11-05" || len(info.Directors) != 1 || len(info.Writers) != 1 || len(info.Studios) != 1 || len(info.Cast) != 1 {
		t.Fatalf("unexpected NFO credits: %+v", info)
	}
	if info.Overview == "" || info.Tagline == "" {
		t.Fatalf("expected overview and tagline to be populated")
	}
}

func TestParseLocalNFO_ParentTVShowAndEpisodeMerge(t *testing.T) {
	showDir := t.TempDir()
	seasonDir := filepath.Join(showDir, "Season 01")
	if err := os.MkdirAll(seasonDir, 0o755); err != nil {
		t.Fatalf("mkdir season: %v", err)
	}

	tvshowNFO := `<?xml version="1.0" encoding="UTF-8"?>
<tvshow>
  <title>The Expanse</title>
  <plot>Series-level synopsis that must not clobber episode overview.</plot>
  <rating>8.5</rating>
  <premiered>2015-12-14</premiered>
  <mpaa>TV-14</mpaa>
  <genre>Sci-Fi</genre>
  <studio>Syfy</studio>
  <uniqueid type="tmdb">63639</uniqueid>
  <uniqueid type="imdb">tt3230854</uniqueid>
</tvshow>`
	if err := os.WriteFile(filepath.Join(showDir, "tvshow.nfo"), []byte(tvshowNFO), 0o644); err != nil {
		t.Fatalf("write tvshow.nfo: %v", err)
	}

	// Episode with no own .nfo inherits series identity & shared attributes only (leaving episode plot/rating/date empty).
	ep1 := filepath.Join(seasonDir, "The Expanse - S01E01.mkv")
	info1, ok := ParseLocalNFOForKind(showDir, ep1, "episode")
	if !ok {
		t.Fatal("expected ParseLocalNFOForKind to find parent tvshow.nfo")
	}
	if info1.Title != "" || info1.SeriesTitle != "The Expanse" || info1.TMDBID != "63639" || info1.IMDBID != "tt3230854" {
		t.Fatalf("unexpected parent tvshow.nfo result: %+v", info1)
	}
	if info1.Overview != "" || info1.Rating != 0 || info1.ReleaseDate != "" {
		t.Fatalf("expected series-only plot/rating/premiere to stay empty on episode fallback, got %+v", info1)
	}

	// Episode with own <episodedetails> carrying an episode-level TMDB ID must NOT override series TMDB ID from tvshow.nfo.
	ep2 := filepath.Join(seasonDir, "The Expanse - S01E04.mkv")
	ep2NFO := `<?xml version="1.0" encoding="UTF-8"?>
<episodedetails>
  <title>CQB</title>
  <plot>The Donnager comes under attack.</plot>
  <uniqueid type="tmdb">9999999</uniqueid>
</episodedetails>`
	if err := os.WriteFile(filepath.Join(seasonDir, "The Expanse - S01E04.nfo"), []byte(ep2NFO), 0o644); err != nil {
		t.Fatalf("write ep2 nfo: %v", err)
	}
	info2, ok := ParseLocalNFOForKind(showDir, ep2, "episode")
	if !ok {
		t.Fatal("expected ParseLocalNFOForKind to parse episode nfo")
	}
	if info2.Title != "CQB" || info2.SeriesTitle != "The Expanse" || info2.TMDBID != "63639" || info2.IMDBID != "tt3230854" {
		t.Fatalf("expected series TMDB ID 63639 (not episode-level 9999999), got %+v", info2)
	}

	// Movie inside a series subfolder (e.g. Show/Movies/Film.mkv) must NEVER inherit parent tvshow.nfo.
	moviesSub := filepath.Join(showDir, "Movies")
	_ = os.MkdirAll(moviesSub, 0o755)
	movieFile := filepath.Join(moviesSub, "Film (2020).mkv")
	if infoM, ok := ParseLocalNFOForKind(showDir, movieFile, "movie"); ok {
		t.Fatalf("movie must not inherit parent tvshow.nfo, got %+v", infoM)
	}

	// Episode sitting directly at library root must NOT read a tvshow.nfo above library root.
	outsideRoot := t.TempDir()
	libRoot := filepath.Join(outsideRoot, "shows")
	_ = os.MkdirAll(libRoot, 0o755)
	_ = os.WriteFile(filepath.Join(outsideRoot, "tvshow.nfo"), []byte(tvshowNFO), 0o644)
	rootEp := filepath.Join(libRoot, "S01E01.mkv")
	if infoOut, ok := ParseLocalNFOForKind(libRoot, rootEp, "episode"); ok {
		t.Fatalf("must not traverse above library root to read tvshow.nfo, got %+v", infoOut)
	}
}
