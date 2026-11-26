package requests

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, body any) *http.Response {
	b, _ := json.Marshal(body)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(b)),
	}
}

func setupTestEnv(t *testing.T, tmdbRT, arrRT roundTripFunc) (*Service, *store.Store, store.User, store.User) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	if err := st.CreateInitialAdmin(ctx, store.User{
		ID:           "u-admin",
		Username:     "admin",
		DisplayName:  "Admin",
		PasswordHash: "hash",
	}); err != nil {
		t.Fatalf("CreateInitialAdmin: %v", err)
	}
	if err := st.CreateUser(ctx, store.User{
		ID:           "u-member",
		Username:     "bob",
		DisplayName:  "Bob",
		PasswordHash: "hash",
		Role:         "member",
		AllLibraries: true,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	admin, _ := st.GetUserByID(ctx, "u-admin")
	member, _ := st.GetUserByID(ctx, "u-member")

	tmdb := metadata.NewTMDBClient("test-key", "https://tmdb.test", dataDir)
	if tmdbRT != nil {
		tmdb.SetHTTPClient(&http.Client{Transport: tmdbRT})
	}
	svc := NewService(st, config.Config{DataDir: dataDir}, tmdb, nil)
	if arrRT != nil {
		svc.SetArrHTTPClient(&http.Client{Transport: arrRT})
	}
	return svc, st, admin, member
}

func TestConfigMaskingAndKeyPreservation(t *testing.T) {
	ctx := context.Background()
	svc, _, admin, _ := setupTestEnv(t, nil, nil)

	if svc.IsEnabled(ctx) {
		t.Fatalf("expected requests disabled by default")
	}

	// Save initial config with API key.
	saved, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://radarr:7878/",
			APIKey:           "secret-radarr-key",
			QualityProfileID: 1,
			RootFolder:       "/movies",
			SearchOnAdd:      true,
		},
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	if !saved.Enabled || !saved.TMDBConfigured || saved.Radarr.APIKey != "" || !saved.Radarr.HasAPIKey {
		t.Fatalf("unexpected masked config: %+v", saved)
	}
	if saved.Radarr.URL != "http://radarr:7878" {
		t.Fatalf("expected trailing slash trimmed, got %q", saved.Radarr.URL)
	}

	// Save again with masked placeholder -> must preserve secret-radarr-key.
	saved2, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://radarr:7878",
			APIKey:           maskedAPIKeyValue,
			QualityProfileID: 2,
			RootFolder:       "/movies",
			SearchOnAdd:      true,
		},
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SaveConfig 2: %v", err)
	}
	if !saved2.Radarr.HasAPIKey || saved2.Radarr.QualityProfileID != 2 {
		t.Fatalf("expected API key preserved, got %+v", saved2.Radarr)
	}
}

func TestArrTestConnection(t *testing.T) {
	ctx := context.Background()
	arrRT := func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Api-Key") != "good-key" {
			return jsonResponse(http.StatusUnauthorized, map[string]string{"message": "Unauthorized"}), nil
		}
		switch r.URL.Path {
		case "/api/v3/system/status":
			return jsonResponse(http.StatusOK, map[string]any{"appName": "Radarr", "version": "5.10.0"}), nil
		case "/api/v3/qualityprofile":
			return jsonResponse(http.StatusOK, []map[string]any{{"id": 1, "name": "HD-1080p"}, {"id": 4, "name": "Ultra-HD"}}), nil
		case "/api/v3/rootfolder":
			return jsonResponse(http.StatusOK, []map[string]any{{"id": 1, "path": "/media/movies", "freeSpace": 1000000}}), nil
		default:
			return jsonResponse(http.StatusNotFound, nil), nil
		}
	}

	svc, _, _, _ := setupTestEnv(t, nil, arrRT)
	res := svc.TestArrConnection(ctx, "radarr", "http://radarr.local:7878", "good-key", "")
	if !res.OK || res.AppName != "Radarr" || res.Version != "5.10.0" {
		t.Fatalf("unexpected TestArrConnection result: %+v", res)
	}
	if len(res.QualityProfiles) != 2 || len(res.RootFolders) != 1 || res.RootFolders[0].Path != "/media/movies" {
		t.Fatalf("unexpected profiles/folders: %+v", res)
	}

	bad := svc.TestArrConnection(ctx, "radarr", "http://radarr.local:7878", "wrong-key", "")
	if bad.OK || bad.Error == "" {
		t.Fatalf("expected failure for wrong key, got %+v", bad)
	}
}

func TestDiscoverAndRequestWorkflow(t *testing.T) {
	ctx := context.Background()

	tmdbRT := func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/trending/all/week":
			return jsonResponse(http.StatusOK, map[string]any{
				"page":        1,
				"total_pages": 3,
				"results": []map[string]any{
					{
						"id":                101,
						"media_type":        "movie",
						"title":             "Local Movie",
						"release_date":      "2023-05-01",
						"poster_path":       "/local.jpg",
						"vote_average":      7.8,
						"genre_ids":         []int{18},
						"original_language": "en",
					},
					{
						"id":                202,
						"media_type":        "tv",
						"name":              "Frieren",
						"original_name":     "葬送のフリーレン",
						"first_air_date":    "2023-09-29",
						"poster_path":       "/frieren.jpg",
						"backdrop_path":     "/frieren-bg.jpg",
						"vote_average":      8.9,
						"genre_ids":         []int{16, 10765},
						"original_language": "ja",
						"origin_country":    []string{"JP"},
					},
					{
						"id":         303,
						"media_type": "person",
						"name":       "Ignored Person",
					},
				},
			}), nil
		case "/tv/202":
			return jsonResponse(http.StatusOK, map[string]any{
				"id":                202,
				"name":              "Frieren",
				"original_name":     "葬送のフリーレン",
				"overview":          "After a ten-year quest...",
				"first_air_date":    "2023-09-29",
				"poster_path":       "/frieren.jpg",
				"backdrop_path":     "/frieren-bg.jpg",
				"vote_average":      8.9,
				"original_language": "ja",
				"origin_country":    []string{"JP"},
				"genres":            []map[string]any{{"id": 16, "name": "Animation"}},
				"external_ids":      map[string]any{"tvdb_id": 424536},
			}), nil
		case "/movie/101":
			return jsonResponse(http.StatusOK, map[string]any{
				"id":           101,
				"title":        "Local Movie",
				"release_date": "2023-05-01",
			}), nil
		case "/movie/555":
			return jsonResponse(http.StatusOK, map[string]any{
				"id":           555,
				"title":        "New Sci-Fi",
				"release_date": "2025-01-10",
				"poster_path":  "/scifi.jpg",
			}), nil
		default:
			return jsonResponse(http.StatusNotFound, nil), nil
		}
	}

	var sonarrAddedSeriesType string
	var radarrPostCount int
	arrRT := func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasPrefix(r.URL.Host, "sonarr-anime") && r.Method == http.MethodGet && r.URL.Path == "/api/v3/series/lookup":
			return jsonResponse(http.StatusOK, []map[string]any{{
				"title":     "Frieren",
				"tvdbId":    424536,
				"titleSlug": "frieren",
			}}), nil
		case strings.HasPrefix(r.URL.Host, "sonarr-anime") && r.Method == http.MethodPost && r.URL.Path == "/api/v3/series":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			sonarrAddedSeriesType, _ = body["seriesType"].(string)
			return jsonResponse(http.StatusCreated, map[string]any{"id": 77}), nil
		case strings.HasPrefix(r.URL.Host, "radarr") && r.Method == http.MethodGet && r.URL.Path == "/api/v3/movie/lookup/tmdb":
			return jsonResponse(http.StatusOK, map[string]any{
				"title":     "New Sci-Fi",
				"tmdbId":    555,
				"titleSlug": "new-sci-fi",
			}), nil
		case strings.HasPrefix(r.URL.Host, "radarr") && r.Method == http.MethodPost && r.URL.Path == "/api/v3/movie":
			radarrPostCount++
			return jsonResponse(http.StatusCreated, map[string]any{"id": 88}), nil
		default:
			return jsonResponse(http.StatusInternalServerError, map[string]string{"error": "unexpected call: " + r.URL.String()}), nil
		}
	}

	svc, st, admin, member := setupTestEnv(t, tmdbRT, arrRT)

	// Enable requests with Radarr + SonarrAnime configured.
	if _, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://radarr:7878",
			APIKey:           "radarr-key",
			QualityProfileID: 1,
			RootFolder:       "/movies",
			SearchOnAdd:      true,
		},
		SonarrAnime: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://sonarr-anime:8989",
			APIKey:           "sonarr-anime-key",
			QualityProfileID: 2,
			RootFolder:       "/anime",
			SeriesType:       "anime",
			SeasonFolder:     true,
			SearchOnAdd:      true,
		},
	}, "127.0.0.1"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Seed local movie with TMDB ID 101.
	_ = st.CreateLibrary(ctx, store.Library{ID: "lib-m", Name: "Movies", Kind: "movies", RootPath: "/movies"})
	_ = st.UpsertMediaItem(ctx, store.MediaItem{
		ID:        "item-101",
		LibraryID: "lib-m",
		Kind:      "movie",
		Title:     "Local Movie",
		SortTitle: "local movie",
		FilePath:  "/movies/local.mkv",
		TMDBID:    "101",
	})

	// 1. Discover annotates in_library and is_anime, filters out person results.
	disc, err := svc.Discover(ctx, member, "trending", "", 1, metadata.DiscoverFilter{})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(disc.Items) != 2 || disc.TotalPages != 3 {
		t.Fatalf("unexpected discover items: %+v", disc)
	}
	if !disc.Items[0].InLibrary || disc.Items[0].LibraryItemID != "item-101" {
		t.Fatalf("expected Local Movie to be marked in_library: %+v", disc.Items[0])
	}
	if !disc.Items[1].IsAnime || disc.Items[1].PosterURL != "https://image.tmdb.org/t/p/w342/frieren.jpg" {
		t.Fatalf("expected Frieren to be detected as anime with full poster URL: %+v", disc.Items[1])
	}

	// 2. Requesting an item already in library returns ErrAlreadyInLibrary.
	if _, err := svc.CreateRequest(ctx, member, "movie", 101, nil, "127.0.0.1"); !errors.Is(err, ErrAlreadyInLibrary) {
		t.Fatalf("expected ErrAlreadyInLibrary, got %v", err)
	}

	// 3. Regular member creates pending request for Frieren.
	req, err := svc.CreateRequest(ctx, member, "tv", 202, nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateRequest member: %v", err)
	}
	if req.Status != store.RequestStatusPending || !req.IsAnime || req.TVDBID != 424536 {
		t.Fatalf("unexpected pending request: %+v", req)
	}

	// 4. Discover now annotates Frieren with request_id & request_status='pending'.
	disc2, err := svc.Discover(ctx, member, "trending", "", 1, metadata.DiscoverFilter{})
	if err != nil {
		t.Fatalf("Discover 2: %v", err)
	}
	if disc2.Items[1].RequestID != req.ID || disc2.Items[1].RequestStatus != store.RequestStatusPending {
		t.Fatalf("expected Frieren annotated with pending request: %+v", disc2.Items[1])
	}

	// 5. Admin approves Frieren -> dispatches to sonarr_anime -> status stays 'approved' until active in queue.
	approved, err := svc.ApproveRequest(ctx, admin, req.ID, nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("ApproveRequest: %v", err)
	}
	if approved.Status != store.RequestStatusApproved || approved.ExternalService != "sonarr_anime" || approved.ExternalID != 77 {
		t.Fatalf("unexpected approved request: %+v", approved)
	}
	if sonarrAddedSeriesType != "anime" {
		t.Fatalf("expected seriesType=anime, got %q", sonarrAddedSeriesType)
	}

	// 6. Member cannot delete a non-pending request.
	if err := svc.DeleteRequest(ctx, member, req.ID, "127.0.0.1"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden when member deletes approved request, got %v", err)
	}

	// 7. Admin creates movie request -> auto-approved and immediately dispatched to Radarr.
	autoReq, err := svc.CreateRequest(ctx, admin, "movie", 555, nil, "127.0.0.1")
	if err != nil {
		t.Fatalf("CreateRequest admin: %v", err)
	}
	if autoReq.Status != store.RequestStatusApproved || autoReq.ExternalService != "radarr" || autoReq.ExternalID != 88 || radarrPostCount != 1 {
		t.Fatalf("expected auto-approved movie dispatched to radarr, got %+v (posts=%d)", autoReq, radarrPostCount)
	}
}

func TestSaveConfigClearsKeyOnOriginChange(t *testing.T) {
	ctx := context.Background()
	svc, _, admin, _ := setupTestEnv(t, nil, nil)

	_, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://radarr.internal:7878",
			APIKey:           "secret-radarr-key",
			QualityProfileID: 1,
			RootFolder:       "/movies",
		},
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SaveConfig initial: %v", err)
	}

	// Changing host without providing a new key must clear the stored API key.
	changed, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://attacker.example:7878",
			APIKey:           maskedAPIKeyValue,
			QualityProfileID: 1,
			RootFolder:       "/movies",
		},
	}, "127.0.0.1")
	if err != nil {
		t.Fatalf("SaveConfig origin change: %v", err)
	}
	if changed.Radarr.HasAPIKey {
		t.Fatalf("expected HasAPIKey=false after changing URL origin without explicit key, got %+v", changed.Radarr)
	}

	// Verify the stored raw config also has an empty API key.
	rawCfg, err := loadStoredConfig(ctx, svc.store, svc.envCfg, true)
	if err != nil {
		t.Fatalf("loadStoredConfig: %v", err)
	}
	if rawCfg.Radarr.APIKey != "" {
		t.Fatalf("expected stored APIKey to be cleared, got %q", rawCfg.Radarr.APIKey)
	}
}

func TestArrTestConnectionRefusesSavedKeyAcrossOrigins(t *testing.T) {
	ctx := context.Background()
	var seenHosts []string
	var seenKeys []string
	arrRT := func(r *http.Request) (*http.Response, error) {
		seenHosts = append(seenHosts, r.URL.Host)
		seenKeys = append(seenKeys, r.Header.Get("X-Api-Key"))
		switch r.URL.Path {
		case "/api/v3/system/status":
			return jsonResponse(http.StatusOK, map[string]any{"appName": "Radarr", "version": "5.10.0"}), nil
		case "/api/v3/qualityprofile":
			return jsonResponse(http.StatusOK, []map[string]any{}), nil
		case "/api/v3/rootfolder":
			return jsonResponse(http.StatusOK, []map[string]any{}), nil
		default:
			return jsonResponse(http.StatusNotFound, nil), nil
		}
	}

	svc, _, admin, _ := setupTestEnv(t, nil, arrRT)
	if _, err := svc.SaveConfig(ctx, admin, RequestsConfig{
		Enabled: true,
		Radarr: ArrInstanceConfig{
			Enabled:          true,
			URL:              "http://radarr.local:7878",
			APIKey:           "super-secret-key",
			QualityProfileID: 1,
			RootFolder:       "/movies",
		},
	}, "127.0.0.1"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// Different host with empty or masked key must be rejected without making any HTTP request.
	for _, key := range []string{"", maskedAPIKeyValue} {
		res := svc.TestArrConnection(ctx, "radarr", "http://evil.example:7878", key, "radarr")
		if res.OK {
			t.Fatalf("expected failure for different host (key=%q), got %+v", key, res)
		}
		if res.Error != "API key is required when changing the server URL" {
			t.Fatalf("unexpected error message: %q", res.Error)
		}
	}
	if len(seenHosts) != 0 {
		t.Fatalf("expected no outbound requests when origin changed, saw hosts=%v", seenHosts)
	}

	// Same origin (case-insensitive) with empty key should reuse saved key and succeed.
	res := svc.TestArrConnection(ctx, "radarr", "HTTP://RADARR.LOCAL:7878/", "", "radarr")
	if !res.OK {
		t.Fatalf("expected same-origin test to succeed, got %+v", res)
	}
	if len(seenKeys) == 0 || seenKeys[0] != "super-secret-key" {
		t.Fatalf("expected saved key sent to same origin, got %v", seenKeys)
	}
}

func TestArrClientRejectsInvalidURLAndCrossOriginRedirect(t *testing.T) {
	ctx := context.Background()

	var redirectTargetHit bool
	arrRT := func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "evil.example" {
			redirectTargetHit = true
			return jsonResponse(http.StatusOK, map[string]any{"appName": "Evil"}), nil
		}
		return &http.Response{
			StatusCode: http.StatusFound,
			Header:     http.Header{"Location": []string{"http://evil.example/steal"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	}

	client := NewArrClient(&http.Client{Transport: roundTripFunc(arrRT)})

	// Invalid scheme must fail before sending a request.
	badScheme := client.TestConnection(ctx, "radarr", "file:///etc/passwd", "k")
	if badScheme.OK || !strings.Contains(badScheme.Error, "http or https") {
		t.Fatalf("expected scheme validation error, got %+v", badScheme)
	}

	// Cross-origin redirect must be blocked without contacting the redirect target.
	redir := client.TestConnection(ctx, "radarr", "http://radarr.local:7878", "secret-key")
	if redir.OK || !strings.Contains(redir.Error, "cross-origin redirect") {
		t.Fatalf("expected cross-origin redirect error, got %+v", redir)
	}
	if redirectTargetHit {
		t.Fatalf("redirect target host should not have been contacted")
	}
}

func TestDiscoverEnforcesCanRequestPermission(t *testing.T) {
	ctx := context.Background()
	tmdbRT := func(r *http.Request) (*http.Response, error) {
		return jsonResponse(http.StatusOK, map[string]any{
			"page":        1,
			"total_pages": 1,
			"results":     []map[string]any{},
		}), nil
	}

	svc, _, admin, member := setupTestEnv(t, tmdbRT, nil)
	if _, err := svc.SaveConfig(ctx, admin, RequestsConfig{Enabled: true}, "127.0.0.1"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	restrictedMember := member
	restrictedMember.CanRequest = false
	if _, err := svc.Discover(ctx, restrictedMember, "trending", "", 1, metadata.DiscoverFilter{}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("expected ErrForbidden for member with CanRequest=false, got %v", err)
	}

	// Admin is always allowed.
	if _, err := svc.Discover(ctx, admin, "trending", "", 1, metadata.DiscoverFilter{}); err != nil {
		t.Fatalf("expected admin Discover to succeed, got %v", err)
	}
}

func TestDiscoverFilters(t *testing.T) {
	ctx := context.Background()

	type capturedReq struct {
		path  string
		query map[string]string
	}
	var last capturedReq

	tmdbRT := func(r *http.Request) (*http.Response, error) {
		q := make(map[string]string)
		for k, vals := range r.URL.Query() {
			if len(vals) > 0 {
				q[k] = vals[0]
			}
		}
		last = capturedReq{path: r.URL.Path, query: q}
		return jsonResponse(http.StatusOK, map[string]any{
			"page":        1,
			"total_pages": 1,
			"results":     []map[string]any{},
		}), nil
	}

	svc, _, admin, member := setupTestEnv(t, tmdbRT, nil)
	if _, err := svc.SaveConfig(ctx, admin, RequestsConfig{Enabled: true}, "127.0.0.1"); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	// 1. Trending with no filter uses /trending/all/week.
	if _, err := svc.Discover(ctx, member, "trending", "", 1, metadata.DiscoverFilter{}); err != nil {
		t.Fatalf("Discover trending: %v", err)
	}
	if last.path != "/trending/all/week" {
		t.Fatalf("expected /trending/all/week, got %s", last.path)
	}

	// 2. Trending with an active filter switches to /discover/movie.
	if _, err := svc.Discover(ctx, member, "trending", "", 1, metadata.DiscoverFilter{
		Sort:      "rating",
		Genre:     878,
		Year:      2024,
		MinRating: 7.5,
		Language:  "en",
	}); err != nil {
		t.Fatalf("Discover trending filtered: %v", err)
	}
	if last.path != "/discover/movie" {
		t.Fatalf("expected /discover/movie, got %s", last.path)
	}
	if last.query["sort_by"] != "vote_average.desc" ||
		last.query["vote_count.gte"] != "200" ||
		last.query["with_genres"] != "878" ||
		last.query["primary_release_year"] != "2024" ||
		last.query["vote_average.gte"] != "7.5" ||
		last.query["with_original_language"] != "en" {
		t.Fatalf("unexpected movie filter query: %+v", last.query)
	}

	// 3. TV with newest + min_rating bumps vote_count.gte from 20 to 50 and uses first_air_date.
	if _, err := svc.Discover(ctx, member, "tv", "", 1, metadata.DiscoverFilter{
		Sort:      "newest",
		Year:      2025,
		MinRating: 8.0,
		Language:  "ko",
	}); err != nil {
		t.Fatalf("Discover tv newest: %v", err)
	}
	if last.path != "/discover/tv" {
		t.Fatalf("expected /discover/tv, got %s", last.path)
	}
	if last.query["sort_by"] != "first_air_date.desc" ||
		last.query["first_air_date.lte"] == "" ||
		last.query["first_air_date_year"] != "2025" ||
		last.query["vote_average.gte"] != "8.0" ||
		last.query["vote_count.gte"] != "50" ||
		last.query["with_original_language"] != "ko" {
		t.Fatalf("unexpected tv newest query: %+v", last.query)
	}

	// 4. Anime tab combines genre 16 with extra genre and preserves ja original language.
	if _, err := svc.Discover(ctx, member, "anime", "", 1, metadata.DiscoverFilter{
		Sort:     "upcoming",
		Genre:    10759,
		Language: "en", // ignored for anime tab
	}); err != nil {
		t.Fatalf("Discover anime upcoming: %v", err)
	}
	if last.path != "/discover/tv" {
		t.Fatalf("expected /discover/tv, got %s", last.path)
	}
	if last.query["sort_by"] != "popularity.desc" ||
		last.query["first_air_date.gte"] == "" ||
		last.query["with_genres"] != "16,10759" ||
		last.query["with_original_language"] != "ja" {
		t.Fatalf("unexpected anime upcoming query: %+v", last.query)
	}
}
