package metadata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestPickBestTMDBCandidate_Scoring(t *testing.T) {
	intPtr := func(v int) *int { return &v }

	// 1. Exact year beats older remake even when older remake appears first.
	duneResults := []tmdbSearchCandidate{
		{ID: 841, Title: "Dune", ReleaseDate: "1984-12-14", VoteCount: 3000, Popularity: 40, PosterPath: "/old.jpg", Overview: "1984"},
		{ID: 438631, Title: "Dune", ReleaseDate: "2021-09-15", VoteCount: 11000, Popularity: 80, PosterPath: "/new.jpg", Overview: "2021"},
	}
	if got := pickBestTMDBCandidate(duneResults, "Dune", intPtr(2021), false); got != 438631 {
		t.Fatalf("expected Dune (2021) ID 438631, got %d", got)
	}
	if got := pickBestTMDBCandidate(duneResults, "Dune", intPtr(1984), false); got != 841 {
		t.Fatalf("expected Dune (1984) ID 841, got %d", got)
	}

	// 2. Without year, exact title + real vote count beats 0-vote obscure homonym and featurette.
	batmanResults := []tmdbSearchCandidate{
		{ID: 999999, Title: "The Batman: Behind the Scenes", ReleaseDate: "2022-03-01", VoteCount: 3, Popularity: 1},
		{ID: 888888, Title: "The Batman", ReleaseDate: "", VoteCount: 0, Popularity: 0.5},
		{ID: 414906, Title: "The Batman", ReleaseDate: "2022-03-01", VoteCount: 9500, Popularity: 90, PosterPath: "/bat.jpg", Overview: "Batman"},
	}
	if got := pickBestTMDBCandidate(batmanResults, "The Batman", nil, false); got != 414906 {
		t.Fatalf("expected The Batman ID 414906, got %d", got)
	}

	// 3. Normalized title (& vs and, punctuation) + +/-1 year festival offset beats mismatched sequel.
	normResults := []tmdbSearchCandidate{
		{ID: 1, Title: "Fast & Furious Presents: Hobbs & Shaw 2", ReleaseDate: "2020-01-01", VoteCount: 50},
		{ID: 2, Title: "Fast & Furious", ReleaseDate: "2009-04-02", VoteCount: 7000, Popularity: 50, PosterPath: "/ff.jpg", Overview: "Cars"},
	}
	if got := pickBestTMDBCandidate(normResults, "Fast and Furious", intPtr(2010), false); got != 2 {
		t.Fatalf("expected Fast & Furious ID 2, got %d", got)
	}

	// 4. Anime library still prefers Japanese Animation (16) over live-action with the same title & year.
	animeResults := []tmdbSearchCandidate{
		{ID: 100, Name: "One Piece", FirstAirDate: "2023-08-31", VoteCount: 4000, Popularity: 90, GenreIDs: []int{10759, 10765}, OriginCountry: []string{"US"}, OriginalLanguage: "en"},
		{ID: 37854, Name: "One Piece", FirstAirDate: "1999-10-20", VoteCount: 4500, Popularity: 120, GenreIDs: []int{16, 10759}, OriginCountry: []string{"JP"}, OriginalLanguage: "ja"},
	}
	if got := pickBestTMDBCandidate(animeResults, "One Piece", nil, true); got != 37854 {
		t.Fatalf("expected anime One Piece ID 37854, got %d", got)
	}
}

func TestTMDBClient_ShardedDeduplicationAndLazyBackdrops(t *testing.T) {
	var posterHits atomic.Int32
	var backdropHits atomic.Int32
	var stillHits atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/movie/157336":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": 157336,
				"title": "Interstellar",
				"poster_path": "/InterstellarPoster.jpg",
				"backdrop_path": "/InterstellarBG.jpg"
			}`))
		case "/tv/63639":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id": 63639,
				"name": "The Expanse",
				"poster_path": "/ExpansePoster.jpg",
				"backdrop_path": "/ExpanseBG.jpg"
			}`))
		case "/tv/63639/season/1/episode/1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"name": "Dulcinea",
				"still_path": "/Ep1Still.jpg"
			}`))
		case "/w780/InterstellarPoster.jpg":
			posterHits.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("poster-bytes"))
		case "/w780/ExpansePoster.jpg":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("series-poster-bytes"))
		case "/w1280/InterstellarBG.jpg":
			backdropHits.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("backdrop-bytes"))
		case "/w1280/Ep1Still.jpg":
			stillHits.Add(1)
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write([]byte("still-bytes"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	dataDir := t.TempDir()
	client := NewTMDBClient("test-key", srv.URL, dataDir)
	client.imageBase = srv.URL

	ctx := context.Background()

	// 1. Two distinct movie items (e.g. 4K + 1080p versions) share one sharded poster file.
	m1, err := client.enrichMovieByID(ctx, "movie-4k", 157336, false)
	if err != nil {
		t.Fatalf("enrichMovieByID m1: %v", err)
	}
	m2, err := client.enrichMovieByID(ctx, "movie-1080p", 157336, false)
	if err != nil {
		t.Fatalf("enrichMovieByID m2: %v", err)
	}
	wantPoster := filepath.Join(dataDir, "artwork", "tmdb", "w780", "in", "InterstellarPoster.jpg")
	if m1.PosterPath != wantPoster || m2.PosterPath != wantPoster {
		t.Fatalf("expected deduplicated sharded poster %q, got %q and %q", wantPoster, m1.PosterPath, m2.PosterPath)
	}
	if posterHits.Load() != 1 {
		t.Fatalf("expected 1 poster download across two movie versions, got %d", posterHits.Load())
	}

	// 2. Movie backdrop is recorded deterministically but NOT downloaded during scan.
	wantBackdrop := filepath.Join(dataDir, "artwork", "tmdb", "w1280", "in", "InterstellarBG.jpg")
	if m1.BackdropPath != wantBackdrop {
		t.Fatalf("expected lazy sharded backdrop %q, got %q", wantBackdrop, m1.BackdropPath)
	}
	if backdropHits.Load() != 0 {
		t.Fatalf("expected 0 eager backdrop downloads during scan, got %d", backdropHits.Load())
	}
	if _, err := os.Stat(wantBackdrop); !os.IsNotExist(err) {
		t.Fatalf("expected backdrop not yet on disk before lazy fetch, stat err=%v", err)
	}
	resolvedBG, err := client.EnsureArtwork(ctx, m1.BackdropPath)
	if err != nil || resolvedBG != wantBackdrop {
		t.Fatalf("EnsureArtwork backdrop failed: path=%q err=%v", resolvedBG, err)
	}
	if backdropHits.Load() != 1 {
		t.Fatalf("expected 1 lazy backdrop download, got %d", backdropHits.Load())
	}
	// Second EnsureArtwork hits disk cache without re-fetching.
	if _, err := client.EnsureArtwork(ctx, m1.BackdropPath); err != nil {
		t.Fatalf("second EnsureArtwork backdrop failed: %v", err)
	}
	if backdropHits.Load() != 1 {
		t.Fatalf("expected cached backdrop to skip upstream fetch, got %d hits", backdropHits.Load())
	}

	// 3. Episode still is NOT downloaded during scan, and materializes lazily via EnsureArtwork.
	ep, err := client.EnrichEpisodeByTMDBID(ctx, "ep-1", "63639", 1, 1, 0, false)
	if err != nil {
		t.Fatalf("EnrichEpisodeByTMDBID: %v", err)
	}
	wantStill := filepath.Join(dataDir, "artwork", "tmdb", "w1280", "ep", "Ep1Still.jpg")
	if ep.BackdropPath != wantStill {
		t.Fatalf("expected lazy sharded still %q, got %q", wantStill, ep.BackdropPath)
	}
	if stillHits.Load() != 0 {
		t.Fatalf("expected 0 eager episode still downloads during scan, got %d", stillHits.Load())
	}
	resolvedStill, err := client.EnsureArtwork(ctx, ep.BackdropPath)
	if err != nil || resolvedStill != wantStill {
		t.Fatalf("EnsureArtwork still failed: path=%q err=%v", resolvedStill, err)
	}
	if b, err := os.ReadFile(resolvedStill); err != nil || string(b) != "still-bytes" {
		t.Fatalf("unexpected cached still bytes: %q (err=%v)", string(b), err)
	}
	if stillHits.Load() != 1 {
		t.Fatalf("expected 1 lazy still download, got %d", stillHits.Load())
	}

	// 4. Security: EnsureArtwork rejects paths outside artwork/tmdb, wrong shards, or invalid sizes.
	badPaths := []string{
		filepath.Join(dataDir, "artwork", "legacy-uuid", "poster.jpg"),
		filepath.Join(dataDir, "artwork", "tmdb", "original", "ep", "Ep1Still.jpg"),
		filepath.Join(dataDir, "artwork", "tmdb", "w1280", "zz", "Ep1Still.jpg"),
		filepath.Join(dataDir, "artwork", "tmdb", "w1280", "ep", "..", "secret.jpg"),
		"/etc/passwd",
	}
	for _, bp := range badPaths {
		if _, err := client.EnsureArtwork(ctx, bp); !errors.Is(err, ErrArtworkNotFound) {
			t.Fatalf("expected ErrArtworkNotFound for %q, got %v", bp, err)
		}
	}

	// 5. Missing poster self-heals on EnsureArtwork if cache directory was wiped.
	if err := os.Remove(wantPoster); err != nil {
		t.Fatalf("remove cached poster: %v", err)
	}
	if p, err := client.EnsureArtwork(ctx, wantPoster); err != nil || !strings.HasSuffix(p, "InterstellarPoster.jpg") {
		t.Fatalf("expected poster self-heal, got path=%q err=%v", p, err)
	}
	if posterHits.Load() != 2 {
		t.Fatalf("expected poster self-heal to re-fetch once, got %d hits", posterHits.Load())
	}
}
