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
  <mpaa>TV-14</mpaa>
  <genre>Sci-Fi</genre>
  <studio>Syfy</studio>
  <uniqueid type="tmdb">63639</uniqueid>
  <uniqueid type="imdb">tt3230854</uniqueid>
</tvshow>`
	if err := os.WriteFile(filepath.Join(showDir, "tvshow.nfo"), []byte(tvshowNFO), 0o644); err != nil {
		t.Fatalf("write tvshow.nfo: %v", err)
	}

	// Episode with no own .nfo inherits series identity from parent tvshow.nfo without clobbering episode title.
	ep1 := filepath.Join(seasonDir, "The Expanse - S01E01.mkv")
	info1, ok := ParseLocalNFO(ep1)
	if !ok {
		t.Fatal("expected ParseLocalNFO to find parent tvshow.nfo")
	}
	if info1.Title != "" || info1.SeriesTitle != "The Expanse" || info1.TMDBID != "63639" || info1.IMDBID != "tt3230854" {
		t.Fatalf("unexpected parent tvshow.nfo result: %+v", info1)
	}

	// Episode with own .nfo keeps episode title/plot and inherits missing TMDB/IMDb IDs from parent tvshow.nfo.
	ep2 := filepath.Join(seasonDir, "The Expanse - S01E04.mkv")
	ep2NFO := `<?xml version="1.0" encoding="UTF-8"?>
<episodedetails>
  <title>CQB</title>
  <plot>The Donnager comes under attack.</plot>
</episodedetails>`
	if err := os.WriteFile(filepath.Join(seasonDir, "The Expanse - S01E04.nfo"), []byte(ep2NFO), 0o644); err != nil {
		t.Fatalf("write ep2 nfo: %v", err)
	}
	info2, ok := ParseLocalNFO(ep2)
	if !ok {
		t.Fatal("expected ParseLocalNFO to parse episode nfo")
	}
	if info2.Title != "CQB" || info2.SeriesTitle != "The Expanse" || info2.TMDBID != "63639" || info2.IMDBID != "tt3230854" {
		t.Fatalf("unexpected merged episode nfo: %+v", info2)
	}
}
