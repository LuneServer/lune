package metadata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
)

func TestTMDBClient_MovieAndEpisodeCaching(t *testing.T) {
	var tvSearchHits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/movie":
			_, _ = w.Write([]byte(`{"results":[{"id":157336}]}`))
		case "/movie/157336":
			_, _ = w.Write([]byte(`{
				"id": 157336,
				"imdb_id": "tt0816692",
				"title": "Interstellar",
				"tagline": "Mankind was born on Earth.",
				"overview": "Explorers travel through a wormhole.",
				"release_date": "2014-11-05",
				"runtime": 169,
				"vote_average": 8.4,
				"poster_path": "/interstellar.jpg",
				"backdrop_path": "/interstellar-bg.jpg",
				"genres": [{"name": "Sci-Fi"}, {"name": "Drama"}],
				"production_companies": [{"name": "Legendary Pictures"}, {"name": "Syncopy"}],
				"release_dates": {"results": [{"iso_3166_1": "US", "release_dates": [{"certification": "PG-13"}]}]},
				"credits": {
					"cast": [
						{"name": "Matthew McConaughey", "character": "Cooper", "profile_path": "/mcconaughey.jpg"},
						{"name": "Anne Hathaway", "character": "Brand", "profile_path": "/hathaway.jpg"}
					],
					"crew": [
						{"name": "Christopher Nolan", "job": "Director", "department": "Directing"},
						{"name": "Jonathan Nolan", "job": "Writer", "department": "Writing"},
						{"name": "Christopher Nolan", "job": "Writer", "department": "Writing"}
					]
				}
			}`))
		case "/search/tv":
			tvSearchHits.Add(1)
			_, _ = w.Write([]byte(`{"results":[{"id":63639}]}`))
		case "/tv/63639":
			_, _ = w.Write([]byte(`{
				"id": 63639,
				"overview": "A thriller set in the solar system.",
				"first_air_date": "2015-12-14",
				"vote_average": 8.1,
				"poster_path": "/expanse.jpg",
				"backdrop_path": "/expanse-bg.jpg",
				"genres": [{"name": "Sci-Fi & Fantasy"}],
				"networks": [{"name": "Syfy"}],
				"created_by": [{"name": "Mark Fergus"}],
				"content_ratings": {"results": [{"iso_3166_1": "US", "rating": "TV-14"}]},
				"external_ids": {"imdb_id": "tt3230854"},
				"credits": {"cast": [{"name": "Steven Strait", "character": "Jim Holden", "profile_path": "/strait.jpg"}]}
			}`))
		case "/tv/63639/season/1/episode/1":
			_, _ = w.Write([]byte(`{
				"name": "Dulcinea",
				"overview": "Ice trawler Canterbury responds to a distress call.",
				"air_date": "2015-12-14",
				"vote_average": 7.9,
				"still_path": "/ep1-still.jpg",
				"crew": [{"name": "Terry McDonough", "job": "Director"}],
				"guest_stars": [{"name": "Jonathan Banks", "character": "Executive Officer", "profile_path": "/banks.jpg"}]
			}`))
		case "/tv/63639/season/1/episode/2":
			_, _ = w.Write([]byte(`{
				"name": "The Big Empty",
				"overview": "Holden and crew struggle to survive in a damaged shuttle.",
				"vote_average": 8.0,
				"still_path": "/ep2-still.jpg"
			}`))
		default:
			// Serve dummy artwork bytes for /w780/... and /w1280/...
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("jpeg-bytes"))
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	client := NewTMDBClient("test-key", srv.URL, dataDir)
	client.imageBase = srv.URL

	year := 2014
	movie, err := client.EnrichMovie(context.Background(), "item-1", "Interstellar", &year)
	if err != nil {
		t.Fatalf("EnrichMovie: %v", err)
	}
	if movie.Title != "Interstellar" || movie.IMDBID != "tt0816692" || movie.Tagline == "" || len(movie.Genres) != 2 {
		t.Fatalf("unexpected movie enrichment: %+v", movie)
	}
	if movie.ContentRating != "PG-13" || movie.ReleaseDate != "2014-11-05" {
		t.Fatalf("unexpected movie rating/date: %q / %q", movie.ContentRating, movie.ReleaseDate)
	}
	if len(movie.Directors) != 1 || movie.Directors[0] != "Christopher Nolan" {
		t.Fatalf("expected director Christopher Nolan, got %+v", movie.Directors)
	}
	if len(movie.Writers) != 2 || len(movie.Studios) != 2 || len(movie.Cast) != 2 {
		t.Fatalf("expected writers/studios/cast populated: %+v", movie)
	}
	if _, err := os.Stat(movie.PosterPath); err != nil {
		t.Fatalf("expected cached poster on disk: %v", err)
	}
	photoPath, err := client.FetchPersonPhoto(context.Background(), "/mcconaughey.jpg")
	if err != nil {
		t.Fatalf("FetchPersonPhoto: %v", err)
	}
	if _, err := os.Stat(photoPath); err != nil {
		t.Fatalf("expected cached person photo on disk: %v", err)
	}
	if _, err := client.FetchPersonPhoto(context.Background(), "/../etc/passwd"); err == nil {
		t.Fatal("expected traversal path to be rejected")
	}

	// Enrich two episodes of the same show -> /search/tv must only be called once.
	ep1, err := client.EnrichEpisode(context.Background(), "ep-1", "The Expanse", 1, 1, nil)
	if err != nil {
		t.Fatalf("EnrichEpisode 1: %v", err)
	}
	if ep1.ContentRating != "TV-14" || len(ep1.Directors) != 1 || ep1.Directors[0] != "Terry McDonough" || len(ep1.Cast) != 2 {
		t.Fatalf("unexpected episode credits: %+v", ep1)
	}
	ep2, err := client.EnrichEpisode(context.Background(), "ep-2", "The Expanse", 1, 2, nil)
	if err != nil {
		t.Fatalf("EnrichEpisode 2: %v", err)
	}
	if ep1.Title != "Dulcinea" || ep2.Title != "The Big Empty" {
		t.Fatalf("unexpected episode titles: %q, %q", ep1.Title, ep2.Title)
	}
	if tvSearchHits.Load() != 1 {
		t.Fatalf("expected 1 cached /search/tv hit, got %d", tvSearchHits.Load())
	}
	if ep1.PosterPath == "" || ep1.PosterPath != ep2.PosterPath {
		t.Fatalf("expected shared series poster cache, got %q vs %q", ep1.PosterPath, ep2.PosterPath)
	}
}

func TestTMDBClient_AnimeRankingAndAbsoluteEpisodeMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/search/tv":
			// First result is a live-action remake; second is the Japanese anime (genre 16 + ja/JP).
			_, _ = w.Write([]byte(`{"results":[
				{"id":11111,"genre_ids":[18,10759],"origin_country":["US"],"original_language":"en"},
				{"id":95479,"genre_ids":[16,10759],"origin_country":["JP"],"original_language":"ja"}
			]}`))
		case "/tv/95479":
			_, _ = w.Write([]byte(`{
				"id": 95479,
				"name": "Jujutsu Kaisen",
				"original_name": "呪術廻戦",
				"overview": "Sorcerers battle curses.",
				"vote_average": 8.6,
				"poster_path": "/jjk.jpg",
				"backdrop_path": "/jjk-bg.jpg",
				"seasons": [
					{"season_number": 0, "episode_count": 3},
					{"season_number": 1, "episode_count": 24},
					{"season_number": 2, "episode_count": 23}
				],
				"genres": [{"name": "Animation"}, {"name": "Action & Adventure"}]
			}`))
		case "/tv/95479/season/2/episode/4":
			_, _ = w.Write([]byte(`{
				"name": "Hidden Inventory 4",
				"overview": "Gojo faces Toji.",
				"vote_average": 9.1,
				"still_path": "/jjk-s2e4.jpg"
			}`))
		default:
			if r.URL.Path == "/w780/jjk.jpg" || r.URL.Path == "/w1280/jjk-bg.jpg" || r.URL.Path == "/w1280/jjk-s2e4.jpg" {
				w.Header().Set("Content-Type", "image/jpeg")
				_, _ = w.Write([]byte("jpeg"))
				return
			}
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client := NewTMDBClient("test-key", srv.URL, t.TempDir())
	client.imageBase = srv.URL

	// File uses absolute episode 28 (season=1, episode=28, abs=28) -> maps to S02E04!
	info, err := client.EnrichAnimeEpisode(context.Background(), "ep-28", "Jujutsu Kaisen", 1, 28, 28, nil)
	if err != nil {
		t.Fatalf("EnrichAnimeEpisode: %v", err)
	}
	if info.TMDBID != "95479" {
		t.Fatalf("expected anime TMDB ID 95479 over live-action 11111, got %q", info.TMDBID)
	}
	if info.OriginalTitle != "呪術廻戦" {
		t.Fatalf("expected original title 呪術廻戦, got %q", info.OriginalTitle)
	}
	if info.Title != "Hidden Inventory 4" {
		t.Fatalf("expected mapped episode title Hidden Inventory 4, got %q", info.Title)
	}
	if info.MappedSeason == nil || *info.MappedSeason != 2 || info.MappedEpisode == nil || *info.MappedEpisode != 4 {
		t.Fatalf("expected mapped S02E04, got S%vE%v", info.MappedSeason, info.MappedEpisode)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTMDBClient_FetchPersonPhotoResilience(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var hits atomic.Int32

	client := NewTMDBClient("test-key", "https://tmdb.example", t.TempDir())
	client.imageBase = "https://img.example"
	client.httpClient = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			switch r.URL.Path {
			case "/w185/slow.jpg":
				if hits.Add(1) == 1 {
					close(started)
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"image/jpeg"}},
					Body:       io.NopCloser(bytes.NewReader([]byte("portrait-bytes"))),
				}, nil
			case "/w185/missing.jpg":
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			case "/w185/flaky.jpg":
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			default:
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(bytes.NewReader(nil)),
				}, nil
			}
		}),
	}

	// Caller 1 starts fetching and cancels mid-flight (simulating browser navigation away).
	ctx1, cancel1 := context.WithCancel(context.Background())
	err1Ch := make(chan error, 1)
	go func() {
		_, err := client.FetchPersonPhoto(ctx1, "/slow.jpg")
		err1Ch <- err
	}()

	select {
	case <-started:
	case err := <-err1Ch:
		t.Fatalf("caller 1 failed before upstream started: %v", err)
	}

	// Caller 2 joins the same singleflight download.
	res2Ch := make(chan string, 1)
	err2Ch := make(chan error, 1)
	go func() {
		p, err := client.FetchPersonPhoto(context.Background(), "/slow.jpg")
		res2Ch <- p
		err2Ch <- err
	}()

	// Cancel caller 1 while the upstream request is still blocked.
	cancel1()
	if err := <-err1Ch; !errors.Is(err, context.Canceled) {
		t.Fatalf("expected caller 1 context.Canceled, got %v", err)
	}

	// Unblock upstream; caller 2 must succeed and cache the file on disk.
	close(release)
	if err := <-err2Ch; err != nil {
		t.Fatalf("expected caller 2 to succeed after caller 1 cancelled, got %v", err)
	}
	photoPath := <-res2Ch
	if _, err := os.Stat(photoPath); err != nil {
		t.Fatalf("expected portrait cached on disk: %v", err)
	}

	// Subsequent call hits disk cache with no additional upstream request.
	if _, err := client.FetchPersonPhoto(context.Background(), "/slow.jpg"); err != nil {
		t.Fatalf("cached FetchPersonPhoto failed: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 upstream fetch, got %d", hits.Load())
	}

	// 404 wraps ErrArtworkNotFound; 503 does not.
	if _, err := client.FetchPersonPhoto(context.Background(), "/missing.jpg"); !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("expected ErrArtworkNotFound for 404, got %v", err)
	}
	if _, err := client.FetchPersonPhoto(context.Background(), "/flaky.jpg"); err == nil || errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("expected non-ErrArtworkNotFound error for 503, got %v", err)
	}
	if _, err := client.FetchPersonPhoto(context.Background(), "/../bad.jpg"); !errors.Is(err, ErrArtworkNotFound) {
		t.Fatalf("expected ErrArtworkNotFound for invalid path, got %v", err)
	}
}
