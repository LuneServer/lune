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
