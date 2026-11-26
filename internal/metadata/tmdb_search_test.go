package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTMDBSearchAndExplicitIDReMatch(t *testing.T) {
	imgBytesV1 := []byte("poster-v1")
	imgBytesV2 := []byte("poster-v2")
	currentImg := imgBytesV1

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/search/movie":
			_, _ = w.Write([]byte(`{
				"results": [
					{
						"id": 101,
						"title": "Dune",
						"original_title": "Dune",
						"release_date": "1984-12-14",
						"overview": "1984 David Lynch adaptation.",
						"poster_path": "/dune84.jpg",
						"vote_average": 6.2
					},
					{
						"id": 202,
						"title": "Dune",
						"original_title": "Dune",
						"release_date": "2021-09-15",
						"overview": "2021 Denis Villeneuve adaptation.",
						"poster_path": "/dune21.jpg",
						"vote_average": 7.8
					}
				]
			}`))
		case r.URL.Path == "/movie/202":
			_, _ = w.Write([]byte(`{
				"id": 202,
				"imdb_id": "tt1160419",
				"title": "Dune",
				"original_title": "Dune",
				"overview": "2021 Denis Villeneuve adaptation.",
				"release_date": "2021-09-15",
				"runtime": 155,
				"vote_average": 7.8,
				"poster_path": "/dune21.jpg",
				"backdrop_path": "/dune21-bg.jpg",
				"genres": [{"name": "Science Fiction"}]
			}`))
		case r.URL.Path == "/search/tv":
			_, _ = w.Write([]byte(`{
				"results": [
					{
						"id": 909,
						"name": "Fullmetal Alchemist: Brotherhood",
						"original_name": "鋼の錬金術師 FULLMETAL ALCHEMIST",
						"first_air_date": "2009-04-05",
						"overview": "Two brothers search for the Philosopher's Stone.",
						"poster_path": "/fmab.jpg",
						"vote_average": 8.7
					}
				]
			}`))
		case r.URL.Path == "/tv/909":
			_, _ = w.Write([]byte(`{
				"id": 909,
				"name": "Fullmetal Alchemist: Brotherhood",
				"original_name": "鋼の錬金術師 FULLMETAL ALCHEMIST",
				"overview": "Two brothers search for the Philosopher's Stone.",
				"first_air_date": "2009-04-05",
				"vote_average": 8.7,
				"poster_path": "/fmab.jpg",
				"backdrop_path": "/fmab-bg.jpg",
				"seasons": [{"season_number": 1, "episode_count": 64}],
				"genres": [{"name": "Animation"}]
			}`))
		case r.URL.Path == "/tv/909/season/1/episode/1":
			_, _ = w.Write([]byte(`{
				"name": "Fullmetal Alchemist",
				"overview": "Edward and Alphonse confront the Freezing Alchemist.",
				"air_date": "2009-04-05",
				"runtime": 24,
				"vote_average": 8.1,
				"still_path": "/fmab-ep1.jpg"
			}`))
		case strings.HasPrefix(r.URL.Path, "/img/"):
			_, _ = w.Write(currentImg)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	client := NewTMDBClient("test-key", srv.URL, dataDir)
	client.imageBase = srv.URL + "/img"

	ctx := context.Background()

	// 1. Visual movie search returns both 1984 and 2021 candidates with posters & years
	movies, err := client.SearchMetadata(ctx, "movie", "Dune", nil)
	if err != nil {
		t.Fatalf("SearchMetadata movie: %v", err)
	}
	if len(movies) != 2 || movies[1].TMDBID != "202" || movies[1].Year == nil || *movies[1].Year != 2021 {
		t.Fatalf("unexpected movie search results: %+v", movies)
	}
	if !strings.HasSuffix(movies[1].PosterURL, "/w342/dune21.jpg") {
		t.Fatalf("expected w342 poster url, got %q", movies[1].PosterURL)
	}

	// Pre-seed stale artwork to verify explicit TMDB ID re-match overwrites it
	staleDir := filepath.Join(dataDir, "artwork", "item-1")
	_ = os.MkdirAll(staleDir, 0o750)
	_ = os.WriteFile(filepath.Join(staleDir, "poster.jpg"), imgBytesV1, 0o644)

	currentImg = imgBytesV2
	info, err := client.EnrichMovieByTMDBID(ctx, "item-1", "202")
	if err != nil {
		t.Fatalf("EnrichMovieByTMDBID: %v", err)
	}
	if info.TMDBID != "202" || info.IMDBID != "tt1160419" {
		t.Fatalf("unexpected enriched movie: %+v", info)
	}
	gotPoster, err := os.ReadFile(info.PosterPath)
	if err != nil || string(gotPoster) != string(imgBytesV2) {
		t.Fatalf("expected stale poster to be overwritten, got %q (err=%v)", string(gotPoster), err)
	}

	// 2. TV search & explicit TMDB TV ID binding
	shows, err := client.SearchMetadata(ctx, "tv", "Fullmetal Alchemist", nil)
	if err != nil {
		t.Fatalf("SearchMetadata tv: %v", err)
	}
	if len(shows) != 1 || shows[0].TMDBID != "909" || shows[0].OriginalTitle == "" {
		t.Fatalf("unexpected tv search results: %+v", shows)
	}

	epInfo, err := client.EnrichEpisodeByTMDBID(ctx, "ep-1", "909", 1, 1, 1, true)
	if err != nil {
		t.Fatalf("EnrichEpisodeByTMDBID: %v", err)
	}
	if epInfo.TMDBID != "909" || epInfo.SeriesTitle != "Fullmetal Alchemist: Brotherhood" || epInfo.Title != "Fullmetal Alchemist" {
		t.Fatalf("unexpected enriched episode: %+v", epInfo)
	}
}
