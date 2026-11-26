package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrArtworkNotFound indicates that an upstream artwork asset does not exist (HTTP 404)
// or the requested path/client configuration is invalid.
var ErrArtworkNotFound = errors.New("artwork not found")

// DefaultTMDBAPIKey can optionally be injected at build time via -ldflags for official releases.
// Kept empty in source; users or a custom TMDB proxy provide the key at runtime.
var DefaultTMDBAPIKey = ""

// Enricher fetches descriptive metadata and caches artwork locally.
type Enricher interface {
	EnrichMovie(ctx context.Context, itemID, title string, year *int) (EnrichedInfo, error)
	EnrichEpisode(ctx context.Context, itemID, seriesTitle string, season, episode int, year *int) (EnrichedInfo, error)
}

// AnimeEnricher is an optional Enricher extension providing anime-aware search ranking
// (preferring Animation / JP results) and absolute-to-seasonal episode mapping.
type AnimeEnricher interface {
	Enricher
	EnrichAnimeMovie(ctx context.Context, itemID, title string, year *int) (EnrichedInfo, error)
	EnrichAnimeEpisode(ctx context.Context, itemID, seriesTitle string, season, episode, absoluteEpisode int, year *int) (EnrichedInfo, error)
}

// PersonArtFetcher lazily fetches and caches cast/crew portrait images on disk.
type PersonArtFetcher interface {
	FetchPersonPhoto(ctx context.Context, profilePath string) (string, error)
}

// MetadataSearchResult represents a single selectable TMDB candidate for visual admin re-matching.
type MetadataSearchResult struct {
	TMDBID        string  `json:"tmdb_id"`
	Kind          string  `json:"kind"` // "movie" | "tv"
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title,omitempty"`
	Year          *int    `json:"year,omitempty"`
	Overview      string  `json:"overview,omitempty"`
	PosterURL     string  `json:"poster_url,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
}

// SearchEnricher is an optional Enricher extension supporting interactive search and direct TMDB ID binding.
type SearchEnricher interface {
	SearchMetadata(ctx context.Context, kind, query string, year *int) ([]MetadataSearchResult, error)
	EnrichMovieByTMDBID(ctx context.Context, itemID, tmdbID string) (EnrichedInfo, error)
	EnrichEpisodeByTMDBID(ctx context.Context, itemID, tmdbID string, season, episode, absoluteEpisode int, preferAnime bool) (EnrichedInfo, error)
}

type tmdbSeasonMeta struct {
	SeasonNumber int `json:"season_number"`
	EpisodeCount int `json:"episode_count"`
}

type cachedTVShow struct {
	id            int
	imdbID        string
	name          string
	originalName  string
	genres        []string
	studios       []string
	creators      []string
	cast          []CreditPerson
	contentRating string
	firstAirDate  string
	posterPath    string
	backdropPath  string
	overview      string
	rating        float64
	seasons       []tmdbSeasonMeta
}

type artFlight struct {
	done chan struct{}
	path string
	err  error
}

// TMDBClient fetches movie, series, and episode metadata & caches artwork under <DataDir>/artwork.
type TMDBClient struct {
	apiKey     string
	artworkDir string
	baseURL    string
	imageBase  string
	httpClient *http.Client

	personArtSem chan struct{}

	mu          sync.Mutex
	tvCache     map[string]*cachedTVShow
	artInflight map[string]*artFlight
}

// NewTMDBClient constructs a TMDB v3 client. When baseURL points at a self-hosted
// caching proxy, apiKey may be left empty.
func NewTMDBClient(apiKey, baseURL, dataDir string) *TMDBClient {
	if apiKey == "" {
		apiKey = DefaultTMDBAPIKey
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://api.themoviedb.org/3"
	}
	if apiKey == "" && baseURL == "https://api.themoviedb.org/3" {
		return nil
	}
	return &TMDBClient{
		apiKey:       apiKey,
		artworkDir:   filepath.Join(dataDir, "artwork"),
		baseURL:      baseURL,
		imageBase:    "https://image.tmdb.org/t/p",
		httpClient:   &http.Client{Timeout: 20 * time.Second},
		personArtSem: make(chan struct{}, 8),
		tvCache:      make(map[string]*cachedTVShow),
		artInflight:  make(map[string]*artFlight),
	}
}

func (c *TMDBClient) EnrichMovie(ctx context.Context, itemID, title string, year *int) (EnrichedInfo, error) {
	return c.enrichMovie(ctx, itemID, title, year, false)
}

func (c *TMDBClient) EnrichAnimeMovie(ctx context.Context, itemID, title string, year *int) (EnrichedInfo, error) {
	return c.enrichMovie(ctx, itemID, title, year, true)
}

func (c *TMDBClient) enrichMovie(ctx context.Context, itemID, title string, year *int, preferAnime bool) (EnrichedInfo, error) {
	if c == nil || title == "" {
		return EnrichedInfo{}, nil
	}

	q := url.Values{}
	q.Set("query", title)
	if year != nil && *year > 0 {
		q.Set("year", strconv.Itoa(*year))
	}

	var searchRes struct {
		Results []tmdbSearchCandidate `json:"results"`
	}
	if err := c.getJSON(ctx, "/search/movie", q, &searchRes); err != nil {
		return EnrichedInfo{}, err
	}
	if len(searchRes.Results) == 0 && year != nil && *year > 0 {
		// Retry without year filter (festival/remaster years can differ from TMDB's theatrical year).
		q.Del("year")
		if err := c.getJSON(ctx, "/search/movie", q, &searchRes); err != nil {
			return EnrichedInfo{}, err
		}
	}
	if len(searchRes.Results) == 0 {
		return EnrichedInfo{}, nil
	}

	movieID := pickBestTMDBCandidate(searchRes.Results, title, year, preferAnime)
	return c.enrichMovieByID(ctx, itemID, movieID, false)
}

// EnrichMovieByTMDBID fetches movie details and artwork for an explicitly chosen TMDB movie ID,
// replacing any previously cached artwork for itemID.
func (c *TMDBClient) EnrichMovieByTMDBID(ctx context.Context, itemID, tmdbID string) (EnrichedInfo, error) {
	if c == nil {
		return EnrichedInfo{}, fmt.Errorf("tmdb client not configured")
	}
	movieID, err := strconv.Atoi(strings.TrimSpace(tmdbID))
	if err != nil || movieID <= 0 {
		return EnrichedInfo{}, fmt.Errorf("invalid tmdb movie id %q", tmdbID)
	}
	return c.enrichMovieByID(ctx, itemID, movieID, true)
}

func (c *TMDBClient) enrichMovieByID(ctx context.Context, itemID string, movieID int, forceArtwork bool) (EnrichedInfo, error) {
	var detail struct {
		ID            int     `json:"id"`
		IMDBID        string  `json:"imdb_id"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		Tagline       string  `json:"tagline"`
		Overview      string  `json:"overview"`
		ReleaseDate   string  `json:"release_date"`
		Runtime       int     `json:"runtime"`
		VoteAverage   float64 `json:"vote_average"`
		PosterPath    string  `json:"poster_path"`
		BackdropPath  string  `json:"backdrop_path"`
		Genres        []struct {
			Name string `json:"name"`
		} `json:"genres"`
		ProductionCompanies []struct {
			Name string `json:"name"`
		} `json:"production_companies"`
		Credits struct {
			Cast []struct {
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
			} `json:"cast"`
			Crew []struct {
				Name        string `json:"name"`
				Job         string `json:"job"`
				Department  string `json:"department"`
				ProfilePath string `json:"profile_path"`
			} `json:"crew"`
		} `json:"credits"`
		ReleaseDates struct {
			Results []struct {
				ISO3166_1    string `json:"iso_3166_1"`
				ReleaseDates []struct {
					Certification string `json:"certification"`
				} `json:"release_dates"`
			} `json:"results"`
		} `json:"release_dates"`
	}
	detailQ := url.Values{}
	detailQ.Set("append_to_response", "credits,release_dates")
	if err := c.getJSON(ctx, fmt.Sprintf("/movie/%d", movieID), detailQ, &detail); err != nil {
		return EnrichedInfo{}, err
	}

	info := EnrichedInfo{
		Title:         strings.TrimSpace(detail.Title),
		OriginalTitle: strings.TrimSpace(detail.OriginalTitle),
		Overview:      strings.TrimSpace(detail.Overview),
		Tagline:       strings.TrimSpace(detail.Tagline),
		Rating:        detail.VoteAverage,
		RuntimeMS:     int64(detail.Runtime) * 60_000,
		ReleaseDate:   strings.TrimSpace(detail.ReleaseDate),
		TMDBID:        strconv.Itoa(detail.ID),
		IMDBID:        strings.TrimSpace(detail.IMDBID),
	}
	if strings.EqualFold(info.OriginalTitle, info.Title) {
		info.OriginalTitle = ""
	}
	if len(detail.ReleaseDate) >= 4 {
		if y, err := strconv.Atoi(detail.ReleaseDate[:4]); err == nil && y > 1880 {
			info.Year = &y
		}
	}
	for _, g := range detail.Genres {
		if g.Name != "" {
			info.Genres = append(info.Genres, g.Name)
		}
	}
	for _, pc := range detail.ProductionCompanies {
		if name := strings.TrimSpace(pc.Name); name != "" {
			info.Studios = appendUnique(info.Studios, name, 6)
		}
	}
	// Prefer US certification; fall back to GB or first non-empty certification.
	var fallbackCert string
	for _, country := range detail.ReleaseDates.Results {
		for _, rd := range country.ReleaseDates {
			cert := strings.TrimSpace(rd.Certification)
			if cert == "" {
				continue
			}
			if strings.EqualFold(country.ISO3166_1, "US") {
				info.ContentRating = cert
				break
			}
			if fallbackCert == "" || strings.EqualFold(country.ISO3166_1, "GB") {
				fallbackCert = cert
			}
		}
		if info.ContentRating != "" {
			break
		}
	}
	if info.ContentRating == "" {
		info.ContentRating = fallbackCert
	}

	for _, cr := range detail.Credits.Crew {
		name := strings.TrimSpace(cr.Name)
		if name == "" {
			continue
		}
		switch cr.Job {
		case "Director":
			info.Directors = appendUnique(info.Directors, name, 6)
		case "Screenplay", "Writer", "Story", "Teleplay", "Novel":
			info.Writers = appendUnique(info.Writers, name, 6)
		}
	}
	for _, cast := range detail.Credits.Cast {
		name := strings.TrimSpace(cast.Name)
		if name == "" {
			continue
		}
		info.Cast = append(info.Cast, CreditPerson{
			Name:        name,
			Role:        strings.TrimSpace(cast.Character),
			Department:  "cast",
			ProfilePath: strings.TrimSpace(cast.ProfilePath),
		})
		if len(info.Cast) >= 16 {
			break
		}
	}

	if detail.PosterPath != "" {
		if p, err := c.downloadArtworkOpts(ctx, itemID, "poster.jpg", "/w780"+detail.PosterPath, forceArtwork); err == nil {
			info.PosterPath = p
		}
	}
	if detail.BackdropPath != "" {
		if p, err := c.downloadArtworkOpts(ctx, itemID, "backdrop.jpg", "/w1280"+detail.BackdropPath, forceArtwork); err == nil {
			info.BackdropPath = p
		}
	}
	return info, nil
}

func (c *TMDBClient) EnrichEpisode(ctx context.Context, itemID, seriesTitle string, season, episode int, year *int) (EnrichedInfo, error) {
	return c.enrichEpisode(ctx, itemID, seriesTitle, season, episode, 0, year, false)
}

func (c *TMDBClient) EnrichAnimeEpisode(ctx context.Context, itemID, seriesTitle string, season, episode, absoluteEpisode int, year *int) (EnrichedInfo, error) {
	return c.enrichEpisode(ctx, itemID, seriesTitle, season, episode, absoluteEpisode, year, true)
}

func (c *TMDBClient) enrichEpisode(ctx context.Context, itemID, seriesTitle string, season, episode, absoluteEpisode int, year *int, preferAnime bool) (EnrichedInfo, error) {
	if c == nil || seriesTitle == "" {
		return EnrichedInfo{}, nil
	}

	show, err := c.resolveTVShow(ctx, seriesTitle, year, preferAnime)
	if err != nil || show == nil {
		return EnrichedInfo{}, err
	}
	return c.enrichEpisodeWithShow(ctx, itemID, show, seriesTitle, season, episode, absoluteEpisode, preferAnime, false)
}

// EnrichEpisodeByTMDBID fetches TV/Anime episode details and artwork for an explicitly chosen TMDB TV ID,
// replacing any previously cached episode artwork for itemID.
func (c *TMDBClient) EnrichEpisodeByTMDBID(ctx context.Context, itemID, tmdbID string, season, episode, absoluteEpisode int, preferAnime bool) (EnrichedInfo, error) {
	if c == nil {
		return EnrichedInfo{}, fmt.Errorf("tmdb client not configured")
	}
	tvID, err := strconv.Atoi(strings.TrimSpace(tmdbID))
	if err != nil || tvID <= 0 {
		return EnrichedInfo{}, fmt.Errorf("invalid tmdb tv id %q", tmdbID)
	}
	show, err := c.fetchTVShowByID(ctx, tvID)
	if err != nil || show == nil {
		return EnrichedInfo{}, err
	}
	return c.enrichEpisodeWithShow(ctx, itemID, show, show.name, season, episode, absoluteEpisode, preferAnime, true)
}

func (c *TMDBClient) enrichEpisodeWithShow(ctx context.Context, itemID string, show *cachedTVShow, seriesTitle string, season, episode, absoluteEpisode int, preferAnime, forceArtwork bool) (EnrichedInfo, error) {
	info := EnrichedInfo{
		SeriesTitle:   show.name,
		OriginalTitle: show.originalName,
		Overview:      show.overview,
		Rating:        show.rating,
		ContentRating: show.contentRating,
		ReleaseDate:   show.firstAirDate,
		Genres:        append([]string(nil), show.genres...),
		Studios:       append([]string(nil), show.studios...),
		Cast:          append([]CreditPerson(nil), show.cast...),
		TMDBID:        strconv.Itoa(show.id),
		IMDBID:        show.imdbID,
	}
	if len(show.firstAirDate) >= 4 {
		if y, err := strconv.Atoi(show.firstAirDate[:4]); err == nil && y > 1880 {
			info.Year = &y
		}
	}
	if strings.EqualFold(info.OriginalTitle, show.name) || strings.EqualFold(info.OriginalTitle, seriesTitle) {
		info.OriginalTitle = ""
	}

	targetSeason, targetEpisode := season, episode
	mapped := false

	// For anime files using absolute numbering (e.g. Episode 28 in a 24-ep Season 1 show),
	// proactively map the absolute number to TMDB's seasonal (season, episode) coordinates.
	if preferAnime && targetSeason == 1 && (absoluteEpisode > 0 || targetEpisode > 0) {
		absNum := absoluteEpisode
		if absNum <= 0 {
			absNum = targetEpisode
		}
		if ms, me, ok := mapAbsoluteToTMDBSeason(show.seasons, absNum); ok && (ms != targetSeason || me != targetEpisode) {
			targetSeason, targetEpisode = ms, me
			mapped = true
		}
	}

	// Fetch specific episode details when season & episode numbers are known.
	if targetSeason >= 0 && targetEpisode > 0 {
		var epDetail struct {
			Name        string  `json:"name"`
			Overview    string  `json:"overview"`
			AirDate     string  `json:"air_date"`
			Runtime     int     `json:"runtime"`
			VoteAverage float64 `json:"vote_average"`
			StillPath   string  `json:"still_path"`
			Crew        []struct {
				Name string `json:"name"`
				Job  string `json:"job"`
			} `json:"crew"`
			GuestStars []struct {
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
			} `json:"guest_stars"`
		}
		epEndpoint := fmt.Sprintf("/tv/%d/season/%d/episode/%d", show.id, targetSeason, targetEpisode)
		epErr := c.getJSON(ctx, epEndpoint, nil, &epDetail)
		// If direct lookup failed and we haven't tried absolute mapping yet, attempt fallback mapping.
		if epErr != nil && !mapped && targetSeason == 1 && targetEpisode > 0 {
			if ms, me, ok := mapAbsoluteToTMDBSeason(show.seasons, targetEpisode); ok && (ms != targetSeason || me != targetEpisode) {
				targetSeason, targetEpisode = ms, me
				mapped = true
				epEndpoint = fmt.Sprintf("/tv/%d/season/%d/episode/%d", show.id, targetSeason, targetEpisode)
				epErr = c.getJSON(ctx, epEndpoint, nil, &epDetail)
			}
		}
		if epErr == nil {
			if mapped {
				ms, me := targetSeason, targetEpisode
				info.MappedSeason = &ms
				info.MappedEpisode = &me
			}
			if epDetail.Name != "" {
				info.Title = strings.TrimSpace(epDetail.Name)
			}
			if epDetail.Overview != "" {
				info.Overview = strings.TrimSpace(epDetail.Overview)
			}
			if strings.TrimSpace(epDetail.AirDate) != "" {
				info.ReleaseDate = strings.TrimSpace(epDetail.AirDate)
			}
			if epDetail.Runtime > 0 {
				info.RuntimeMS = int64(epDetail.Runtime) * 60_000
			}
			if epDetail.VoteAverage > 0 {
				info.Rating = epDetail.VoteAverage
			}
			for _, cr := range epDetail.Crew {
				name := strings.TrimSpace(cr.Name)
				if name == "" {
					continue
				}
				switch cr.Job {
				case "Director":
					info.Directors = appendUnique(info.Directors, name, 6)
				case "Writer", "Teleplay", "Story", "Screenplay":
					info.Writers = appendUnique(info.Writers, name, 6)
				}
			}
			seenCast := make(map[string]bool, len(info.Cast))
			for _, p := range info.Cast {
				seenCast[strings.ToLower(p.Name)] = true
			}
			for _, gs := range epDetail.GuestStars {
				name := strings.TrimSpace(gs.Name)
				if name == "" || seenCast[strings.ToLower(name)] {
					continue
				}
				seenCast[strings.ToLower(name)] = true
				info.Cast = append(info.Cast, CreditPerson{
					Name:        name,
					Role:        strings.TrimSpace(gs.Character),
					Department:  "cast",
					ProfilePath: strings.TrimSpace(gs.ProfilePath),
				})
				if len(info.Cast) >= 18 {
					break
				}
			}
			if epDetail.StillPath != "" {
				if p, err := c.downloadArtworkOpts(ctx, itemID, "backdrop.jpg", "/w1280"+epDetail.StillPath, forceArtwork); err == nil {
					info.BackdropPath = p
				}
			}
		}
	}
	if len(info.Writers) == 0 && len(show.creators) > 0 {
		info.Writers = append([]string(nil), show.creators...)
	}

	// Cache series poster under a shared series key so 100 episodes only download the poster once.
	seriesArtKey := fmt.Sprintf("tv-%d", show.id)
	if show.posterPath != "" {
		if p, err := c.downloadArtwork(ctx, seriesArtKey, "poster.jpg", "/w780"+show.posterPath); err == nil {
			info.PosterPath = p
		}
	}
	if info.BackdropPath == "" && show.backdropPath != "" {
		if p, err := c.downloadArtwork(ctx, seriesArtKey, "backdrop.jpg", "/w1280"+show.backdropPath); err == nil {
			info.BackdropPath = p
		}
	}
	return info, nil
}

type tmdbSearchCandidate struct {
	ID               int      `json:"id"`
	Title            string   `json:"title"`
	OriginalTitle    string   `json:"original_title"`
	Name             string   `json:"name"`
	OriginalName     string   `json:"original_name"`
	ReleaseDate      string   `json:"release_date"`
	FirstAirDate     string   `json:"first_air_date"`
	Overview         string   `json:"overview"`
	PosterPath       string   `json:"poster_path"`
	Popularity       float64  `json:"popularity"`
	VoteCount        int      `json:"vote_count"`
	GenreIDs         []int    `json:"genre_ids"`
	OriginCountry    []string `json:"origin_country"`
	OriginalLanguage string   `json:"original_language"`
}

func pickBestTMDBCandidate(results []tmdbSearchCandidate, queryTitle string, year *int, preferAnime bool) int {
	if len(results) == 0 {
		return 0
	}
	normQuery := normalizeMatchTitle(queryTitle)
	normQueryNoArticle := stripLeadingArticle(normQuery)
	queryTokens := matchTokens(normQuery)

	bestID := results[0].ID
	bestScore := math.Inf(-1)

	for i, r := range results {
		score := -float64(i) * 0.25 // gentle tiebreaker preserving TMDB order

		// 1. Title similarity (best of localized title/name vs original title/name).
		if normQuery != "" {
			titleScore := 0.0
			for _, candRaw := range []string{r.Title, r.Name, r.OriginalTitle, r.OriginalName} {
				cand := normalizeMatchTitle(candRaw)
				if cand == "" {
					continue
				}
				var s float64
				switch {
				case cand == normQuery:
					s = 100
				case normQueryNoArticle != "" && stripLeadingArticle(cand) == normQueryNoArticle:
					s = 92
				default:
					candTokens := matchTokens(cand)
					s = tokenDiceSimilarity(queryTokens, candTokens) * 65
					if strings.HasPrefix(cand, normQuery) || strings.HasPrefix(normQuery, cand) {
						s += 10
					}
				}
				if s > titleScore {
					titleScore = s
				}
			}
			score += titleScore
		}

		// 2. Release / first-air year proximity.
		if year != nil && *year > 0 {
			candYear := parseTMDBCandidateYear(r.ReleaseDate, r.FirstAirDate)
			if candYear > 0 {
				diff := candYear - *year
				if diff < 0 {
					diff = -diff
				}
				switch {
				case diff == 0:
					score += 50
				case diff == 1:
					score += 25
				case diff == 2:
					score += 0
				default:
					pen := float64(diff) * 3
					if pen > 40 {
						pen = 40
					}
					score -= pen
				}
			} else {
				score -= 10
			}
		}

		// 3. Popularity / vote-count confidence tiebreaker so real releases beat 0-vote shorts/homonyms.
		if r.VoteCount > 0 {
			score += math.Log10(float64(r.VoteCount)+1) * 8
		}
		if r.Popularity > 0 {
			popBoost := math.Log10(r.Popularity+1) * 4
			if popBoost > 10 {
				popBoost = 10
			}
			score += popBoost
		}
		if r.PosterPath != "" {
			score += 4
		}
		if r.Overview != "" {
			score += 2
		}

		// 4. Anime library preference: strongly favor Animation (16) + Japanese origin/language.
		if preferAnime {
			for _, g := range r.GenreIDs {
				if g == 16 {
					score += 60
					break
				}
			}
			if strings.EqualFold(r.OriginalLanguage, "ja") {
				score += 30
			}
			for _, c := range r.OriginCountry {
				if strings.EqualFold(c, "JP") {
					score += 20
					break
				}
			}
		}

		if score > bestScore {
			bestScore = score
			bestID = r.ID
		}
	}
	return bestID
}

func parseTMDBCandidateYear(releaseDate, firstAirDate string) int {
	for _, d := range []string{releaseDate, firstAirDate} {
		d = strings.TrimSpace(d)
		if len(d) >= 4 {
			if y, err := strconv.Atoi(d[:4]); err == nil && y > 1880 && y < 2200 {
				return y
			}
		}
	}
	return 0
}

func normalizeMatchTitle(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return ""
	}
	s = strings.ReplaceAll(s, "&", " and ")
	var b strings.Builder
	b.Grow(len(s))
	lastSpace := true
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
			lastSpace = false
		} else if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func stripLeadingArticle(norm string) string {
	for _, prefix := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(norm, prefix) {
			return strings.TrimSpace(norm[len(prefix):])
		}
	}
	return norm
}

func matchTokens(norm string) map[string]struct{} {
	fields := strings.Fields(norm)
	if len(fields) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(fields))
	for _, f := range fields {
		set[f] = struct{}{}
	}
	return set
}

func tokenDiceSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	overlap := 0
	for k := range a {
		if _, ok := b[k]; ok {
			overlap++
		}
	}
	return (2.0 * float64(overlap)) / float64(len(a)+len(b))
}

func mapAbsoluteToTMDBSeason(seasons []tmdbSeasonMeta, absoluteEp int) (int, int, bool) {
	if absoluteEp <= 0 || len(seasons) == 0 {
		return 0, 0, false
	}
	remaining := absoluteEp
	for _, s := range seasons {
		if s.SeasonNumber <= 0 || s.EpisodeCount <= 0 {
			continue
		}
		if remaining <= s.EpisodeCount {
			return s.SeasonNumber, remaining, true
		}
		remaining -= s.EpisodeCount
	}
	return 0, 0, false
}

func (c *TMDBClient) resolveTVShow(ctx context.Context, seriesTitle string, year *int, preferAnime bool) (*cachedTVShow, error) {
	prefix := "tv:"
	if preferAnime {
		prefix = "anime:"
	}
	key := prefix + strings.ToLower(strings.TrimSpace(seriesTitle))
	c.mu.Lock()
	if cached, ok := c.tvCache[key]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	q := url.Values{}
	q.Set("query", seriesTitle)
	if year != nil && *year > 0 {
		q.Set("first_air_date_year", strconv.Itoa(*year))
	}

	var searchRes struct {
		Results []tmdbSearchCandidate `json:"results"`
	}
	if err := c.getJSON(ctx, "/search/tv", q, &searchRes); err != nil {
		return nil, err
	}
	if len(searchRes.Results) == 0 && year != nil && *year > 0 {
		// Retry without year filter (anime cour/remaster folders often carry a different year than S01).
		q.Del("first_air_date_year")
		if err := c.getJSON(ctx, "/search/tv", q, &searchRes); err != nil {
			return nil, err
		}
	}
	if len(searchRes.Results) == 0 {
		return nil, nil
	}

	tvID := pickBestTMDBCandidate(searchRes.Results, seriesTitle, year, preferAnime)
	show, err := c.fetchTVShowByID(ctx, tvID)
	if err != nil || show == nil {
		return nil, err
	}

	c.mu.Lock()
	c.tvCache[key] = show
	c.mu.Unlock()
	return show, nil
}

func (c *TMDBClient) fetchTVShowByID(ctx context.Context, tvID int) (*cachedTVShow, error) {
	idKey := fmt.Sprintf("id:%d", tvID)
	c.mu.Lock()
	if cached, ok := c.tvCache[idKey]; ok {
		c.mu.Unlock()
		return cached, nil
	}
	c.mu.Unlock()

	var detail struct {
		ID           int              `json:"id"`
		Name         string           `json:"name"`
		OriginalName string           `json:"original_name"`
		Overview     string           `json:"overview"`
		FirstAirDate string           `json:"first_air_date"`
		VoteAverage  float64          `json:"vote_average"`
		PosterPath   string           `json:"poster_path"`
		BackdropPath string           `json:"backdrop_path"`
		Seasons      []tmdbSeasonMeta `json:"seasons"`
		Genres       []struct {
			Name string `json:"name"`
		} `json:"genres"`
		CreatedBy []struct {
			Name string `json:"name"`
		} `json:"created_by"`
		Networks []struct {
			Name string `json:"name"`
		} `json:"networks"`
		ProductionCompanies []struct {
			Name string `json:"name"`
		} `json:"production_companies"`
		Credits struct {
			Cast []struct {
				Name        string `json:"name"`
				Character   string `json:"character"`
				ProfilePath string `json:"profile_path"`
			} `json:"cast"`
		} `json:"credits"`
		ContentRatings struct {
			Results []struct {
				ISO3166_1 string `json:"iso_3166_1"`
				Rating    string `json:"rating"`
			} `json:"results"`
		} `json:"content_ratings"`
		ExternalIDs struct {
			IMDBID string `json:"imdb_id"`
		} `json:"external_ids"`
	}
	tvQ := url.Values{}
	tvQ.Set("append_to_response", "credits,content_ratings,external_ids")
	if err := c.getJSON(ctx, fmt.Sprintf("/tv/%d", tvID), tvQ, &detail); err != nil {
		return nil, err
	}

	show := &cachedTVShow{
		id:           detail.ID,
		imdbID:       strings.TrimSpace(detail.ExternalIDs.IMDBID),
		name:         strings.TrimSpace(detail.Name),
		originalName: strings.TrimSpace(detail.OriginalName),
		overview:     strings.TrimSpace(detail.Overview),
		firstAirDate: strings.TrimSpace(detail.FirstAirDate),
		rating:       detail.VoteAverage,
		posterPath:   detail.PosterPath,
		backdropPath: detail.BackdropPath,
		seasons:      detail.Seasons,
	}
	for _, g := range detail.Genres {
		if g.Name != "" {
			show.genres = append(show.genres, g.Name)
		}
	}
	for _, cr := range detail.CreatedBy {
		if name := strings.TrimSpace(cr.Name); name != "" {
			show.creators = appendUnique(show.creators, name, 6)
		}
	}
	for _, n := range detail.Networks {
		if name := strings.TrimSpace(n.Name); name != "" {
			show.studios = appendUnique(show.studios, name, 6)
		}
	}
	for _, pc := range detail.ProductionCompanies {
		if name := strings.TrimSpace(pc.Name); name != "" {
			show.studios = appendUnique(show.studios, name, 6)
		}
	}
	var fallbackRating string
	for _, r := range detail.ContentRatings.Results {
		val := strings.TrimSpace(r.Rating)
		if val == "" {
			continue
		}
		if strings.EqualFold(r.ISO3166_1, "US") {
			show.contentRating = val
			break
		}
		if fallbackRating == "" || strings.EqualFold(r.ISO3166_1, "GB") {
			fallbackRating = val
		}
	}
	if show.contentRating == "" {
		show.contentRating = fallbackRating
	}
	for _, cast := range detail.Credits.Cast {
		name := strings.TrimSpace(cast.Name)
		if name == "" {
			continue
		}
		show.cast = append(show.cast, CreditPerson{
			Name:        name,
			Role:        strings.TrimSpace(cast.Character),
			Department:  "cast",
			ProfilePath: strings.TrimSpace(cast.ProfilePath),
		})
		if len(show.cast) >= 16 {
			break
		}
	}

	c.mu.Lock()
	c.tvCache[idKey] = show
	c.mu.Unlock()
	return show, nil
}

// SearchMetadata queries TMDB for movies or TV/Anime series and returns selectable visual candidates.
func (c *TMDBClient) SearchMetadata(ctx context.Context, kind, query string, year *int) ([]MetadataSearchResult, error) {
	if c == nil {
		return nil, fmt.Errorf("tmdb client not configured")
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return []MetadataSearchResult{}, nil
	}

	isTV := strings.EqualFold(kind, "tv") || strings.EqualFold(kind, "series") || strings.EqualFold(kind, "episode")
	endpoint := "/search/movie"
	yearParam := "year"
	resultKind := "movie"
	if isTV {
		endpoint = "/search/tv"
		yearParam = "first_air_date_year"
		resultKind = "tv"
	}

	q := url.Values{}
	q.Set("query", query)
	if year != nil && *year > 0 {
		q.Set(yearParam, strconv.Itoa(*year))
	}

	var raw struct {
		Results []struct {
			ID            int     `json:"id"`
			Title         string  `json:"title"`
			OriginalTitle string  `json:"original_title"`
			Name          string  `json:"name"`
			OriginalName  string  `json:"original_name"`
			ReleaseDate   string  `json:"release_date"`
			FirstAirDate  string  `json:"first_air_date"`
			Overview      string  `json:"overview"`
			PosterPath    string  `json:"poster_path"`
			VoteAverage   float64 `json:"vote_average"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, endpoint, q, &raw); err != nil {
		return nil, err
	}
	if len(raw.Results) == 0 && year != nil && *year > 0 {
		q.Del(yearParam)
		if err := c.getJSON(ctx, endpoint, q, &raw); err != nil {
			return nil, err
		}
	}

	limit := len(raw.Results)
	if limit > 12 {
		limit = 12
	}
	out := make([]MetadataSearchResult, 0, limit)
	for _, r := range raw.Results[:limit] {
		title := strings.TrimSpace(r.Title)
		orig := strings.TrimSpace(r.OriginalTitle)
		dateStr := strings.TrimSpace(r.ReleaseDate)
		if isTV {
			title = strings.TrimSpace(r.Name)
			orig = strings.TrimSpace(r.OriginalName)
			dateStr = strings.TrimSpace(r.FirstAirDate)
		}
		if strings.EqualFold(orig, title) {
			orig = ""
		}
		var yr *int
		if len(dateStr) >= 4 {
			if y, err := strconv.Atoi(dateStr[:4]); err == nil && y > 1880 {
				yr = &y
			}
		}
		posterURL := ""
		if strings.HasPrefix(r.PosterPath, "/") {
			posterURL = c.imageBase + "/w342" + r.PosterPath
		}
		out = append(out, MetadataSearchResult{
			TMDBID:        strconv.Itoa(r.ID),
			Kind:          resultKind,
			Title:         title,
			OriginalTitle: orig,
			Year:          yr,
			Overview:      strings.TrimSpace(r.Overview),
			PosterURL:     posterURL,
			Rating:        r.VoteAverage,
		})
	}
	return out, nil
}

// DiscoverItem represents a TMDB movie or TV/anime title returned by Discover or detail lookups.
type DiscoverItem struct {
	MediaType     string  `json:"media_type"` // "movie" | "tv"
	TMDBID        int     `json:"tmdb_id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title,omitempty"`
	Year          *int    `json:"year,omitempty"`
	Overview      string  `json:"overview,omitempty"`
	PosterPath    string  `json:"poster_path,omitempty"`
	BackdropPath  string  `json:"backdrop_path,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
	IsAnime       bool    `json:"is_anime"`
}

// SetHTTPClient replaces the underlying HTTP client (useful for testing with custom transports).
func (c *TMDBClient) SetHTTPClient(client *http.Client) {
	if c != nil && client != nil {
		c.httpClient = client
	}
}

func isAnimeTMDB(genreIDs []int, originalLanguage string, originCountry []string) bool {
	hasAnimation := false
	for _, g := range genreIDs {
		if g == 16 {
			hasAnimation = true
			break
		}
	}
	if !hasAnimation {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(originalLanguage), "ja") {
		return true
	}
	for _, cc := range originCountry {
		if strings.EqualFold(strings.TrimSpace(cc), "JP") {
			return true
		}
	}
	return false
}

// DiscoverFilter holds optional Seerr-style filtering and sorting parameters for TMDB discovery.
type DiscoverFilter struct {
	Sort      string  // "" | "popularity" | "rating" | "newest" | "upcoming"
	Genre     int     // optional TMDB genre ID (> 0)
	Year      int     // optional release/first-air year (> 1880)
	DateFrom  string  // optional YYYY-MM-DD release/first-air range start
	DateTo    string  // optional YYYY-MM-DD release/first-air range end
	MinRating float64 // optional minimum TMDB vote_average (0..10)
	Language  string  // optional ISO 639-1 original language (e.g. "en", "ja", "ko")
}

func validISODate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) != 10 {
		return ""
	}
	if _, err := time.Parse("2006-01-02", s); err != nil {
		return ""
	}
	return s
}

// Discover browses trending/popular movies, TV, or anime, or runs a multi-search when query is non-empty.
func (c *TMDBClient) Discover(ctx context.Context, tab string, query string, page int, filter DiscoverFilter) ([]DiscoverItem, int, error) {
	if c == nil {
		return nil, 0, fmt.Errorf("tmdb client not configured")
	}
	if page < 1 {
		page = 1
	}

	q := url.Values{}
	q.Set("page", strconv.Itoa(page))

	endpoint := "/trending/all/week"
	forcedMediaType := ""
	forcedAnime := false

	query = strings.TrimSpace(query)
	if query != "" {
		endpoint = "/search/multi"
		q.Set("query", query)
		q.Set("include_adult", "false")
	} else {
		sortKey := strings.ToLower(strings.TrimSpace(filter.Sort))
		lang := strings.ToLower(strings.TrimSpace(filter.Language))
		validLang := ""
		if len(lang) == 2 && lang[0] >= 'a' && lang[0] <= 'z' && lang[1] >= 'a' && lang[1] <= 'z' {
			validLang = lang
		}
		dateFrom := validISODate(filter.DateFrom)
		dateTo := validISODate(filter.DateTo)
		hasFilter := (sortKey != "" && sortKey != "popularity") ||
			filter.Genre > 0 ||
			filter.Year > 1880 ||
			dateFrom != "" ||
			dateTo != "" ||
			filter.MinRating > 0 ||
			validLang != ""

		tabKey := strings.ToLower(strings.TrimSpace(tab))
		isDiscover := false
		switch tabKey {
		case "movies":
			endpoint = "/discover/movie"
			forcedMediaType = "movie"
			isDiscover = true
		case "tv":
			endpoint = "/discover/tv"
			forcedMediaType = "tv"
			isDiscover = true
		case "anime":
			endpoint = "/discover/tv"
			forcedMediaType = "tv"
			forcedAnime = true
			isDiscover = true
		default:
			if hasFilter {
				endpoint = "/discover/movie"
				forcedMediaType = "movie"
				isDiscover = true
			} else {
				endpoint = "/trending/all/week"
			}
		}

		if isDiscover {
			today := time.Now().UTC().Format("2006-01-02")
			minVoteCount := 0
			switch sortKey {
			case "rating":
				q.Set("sort_by", "vote_average.desc")
				minVoteCount = 200
			case "newest":
				if forcedMediaType == "tv" {
					q.Set("sort_by", "first_air_date.desc")
					q.Set("first_air_date.lte", today)
				} else {
					q.Set("sort_by", "primary_release_date.desc")
					q.Set("primary_release_date.lte", today)
				}
				minVoteCount = 20
			case "upcoming":
				q.Set("sort_by", "popularity.desc")
				if forcedMediaType == "tv" {
					q.Set("first_air_date.gte", today)
				} else {
					q.Set("primary_release_date.gte", today)
				}
			default:
				q.Set("sort_by", "popularity.desc")
			}

			if forcedAnime {
				if filter.Genre > 0 && filter.Genre != 16 {
					q.Set("with_genres", fmt.Sprintf("16,%d", filter.Genre))
				} else {
					q.Set("with_genres", "16")
				}
				q.Set("with_original_language", "ja")
			} else {
				if filter.Genre > 0 {
					q.Set("with_genres", strconv.Itoa(filter.Genre))
				}
				if validLang != "" {
					q.Set("with_original_language", validLang)
				}
			}

			if dateFrom != "" || dateTo != "" {
				if forcedMediaType == "tv" {
					if dateFrom != "" {
						q.Set("first_air_date.gte", dateFrom)
					}
					if dateTo != "" {
						q.Set("first_air_date.lte", dateTo)
					}
				} else {
					if dateFrom != "" {
						q.Set("primary_release_date.gte", dateFrom)
					}
					if dateTo != "" {
						q.Set("primary_release_date.lte", dateTo)
					}
				}
			} else if filter.Year > 1880 {
				if forcedMediaType == "tv" {
					q.Set("first_air_date_year", strconv.Itoa(filter.Year))
				} else {
					q.Set("primary_release_year", strconv.Itoa(filter.Year))
				}
			}

			if filter.MinRating > 0 {
				q.Set("vote_average.gte", fmt.Sprintf("%.1f", filter.MinRating))
				if minVoteCount < 50 {
					minVoteCount = 50
				}
			}

			if minVoteCount > 0 {
				q.Set("vote_count.gte", strconv.Itoa(minVoteCount))
			}
		}
	}

	var raw struct {
		Page       int `json:"page"`
		TotalPages int `json:"total_pages"`
		Results    []struct {
			ID               int      `json:"id"`
			MediaType        string   `json:"media_type"`
			Title            string   `json:"title"`
			OriginalTitle    string   `json:"original_title"`
			Name             string   `json:"name"`
			OriginalName     string   `json:"original_name"`
			ReleaseDate      string   `json:"release_date"`
			FirstAirDate     string   `json:"first_air_date"`
			Overview         string   `json:"overview"`
			PosterPath       string   `json:"poster_path"`
			BackdropPath     string   `json:"backdrop_path"`
			VoteAverage      float64  `json:"vote_average"`
			GenreIDs         []int    `json:"genre_ids"`
			OriginalLanguage string   `json:"original_language"`
			OriginCountry    []string `json:"origin_country"`
		} `json:"results"`
	}
	if err := c.getJSON(ctx, endpoint, q, &raw); err != nil {
		return nil, 0, err
	}

	items := make([]DiscoverItem, 0, len(raw.Results))
	for _, r := range raw.Results {
		mt := forcedMediaType
		if mt == "" {
			mt = strings.ToLower(strings.TrimSpace(r.MediaType))
		}
		if mt != "movie" && mt != "tv" {
			continue
		}
		title := strings.TrimSpace(r.Title)
		orig := strings.TrimSpace(r.OriginalTitle)
		dateStr := strings.TrimSpace(r.ReleaseDate)
		if mt == "tv" {
			title = strings.TrimSpace(r.Name)
			orig = strings.TrimSpace(r.OriginalName)
			dateStr = strings.TrimSpace(r.FirstAirDate)
		}
		if title == "" {
			continue
		}
		if strings.EqualFold(orig, title) {
			orig = ""
		}
		var yr *int
		if len(dateStr) >= 4 {
			if y, err := strconv.Atoi(dateStr[:4]); err == nil && y > 1880 {
				yr = &y
			}
		}
		isAnime := forcedAnime || isAnimeTMDB(r.GenreIDs, r.OriginalLanguage, r.OriginCountry)
		items = append(items, DiscoverItem{
			MediaType:     mt,
			TMDBID:        r.ID,
			Title:         title,
			OriginalTitle: orig,
			Year:          yr,
			Overview:      strings.TrimSpace(r.Overview),
			PosterPath:    strings.TrimSpace(r.PosterPath),
			BackdropPath:  strings.TrimSpace(r.BackdropPath),
			Rating:        r.VoteAverage,
			IsAnime:       isAnime,
		})
	}

	totalPages := raw.TotalPages
	if totalPages < 1 {
		totalPages = 1
	}
	return items, totalPages, nil
}

// GetRequestTargetDetails fetches authoritative TMDB metadata for a movie or TV show
// (including external TVDB ID for TV series) when creating a media request.
func (c *TMDBClient) GetRequestTargetDetails(ctx context.Context, mediaType string, tmdbID int) (DiscoverItem, int, error) {
	if c == nil {
		return DiscoverItem{}, 0, fmt.Errorf("tmdb client not configured")
	}
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if mt != "movie" && mt != "tv" {
		return DiscoverItem{}, 0, fmt.Errorf("unsupported media_type %q", mediaType)
	}
	if tmdbID <= 0 {
		return DiscoverItem{}, 0, fmt.Errorf("invalid tmdb_id %d", tmdbID)
	}

	if mt == "movie" {
		var detail struct {
			ID               int     `json:"id"`
			Title            string  `json:"title"`
			OriginalTitle    string  `json:"original_title"`
			Overview         string  `json:"overview"`
			ReleaseDate      string  `json:"release_date"`
			PosterPath       string  `json:"poster_path"`
			BackdropPath     string  `json:"backdrop_path"`
			VoteAverage      float64 `json:"vote_average"`
			OriginalLanguage string  `json:"original_language"`
			OriginCountry    []string `json:"origin_country"`
			Genres           []struct {
				ID int `json:"id"`
			} `json:"genres"`
			ProductionCountries []struct {
				ISO3166_1 string `json:"iso_3166_1"`
			} `json:"production_countries"`
		}
		if err := c.getJSON(ctx, fmt.Sprintf("/movie/%d", tmdbID), nil, &detail); err != nil {
			return DiscoverItem{}, 0, err
		}
		genreIDs := make([]int, 0, len(detail.Genres))
		for _, g := range detail.Genres {
			genreIDs = append(genreIDs, g.ID)
		}
		countries := append([]string(nil), detail.OriginCountry...)
		for _, pc := range detail.ProductionCountries {
			if pc.ISO3166_1 != "" {
				countries = append(countries, pc.ISO3166_1)
			}
		}
		title := strings.TrimSpace(detail.Title)
		orig := strings.TrimSpace(detail.OriginalTitle)
		if strings.EqualFold(orig, title) {
			orig = ""
		}
		var yr *int
		if len(detail.ReleaseDate) >= 4 {
			if y, err := strconv.Atoi(detail.ReleaseDate[:4]); err == nil && y > 1880 {
				yr = &y
			}
		}
		return DiscoverItem{
			MediaType:     "movie",
			TMDBID:        detail.ID,
			Title:         title,
			OriginalTitle: orig,
			Year:          yr,
			Overview:      strings.TrimSpace(detail.Overview),
			PosterPath:    strings.TrimSpace(detail.PosterPath),
			BackdropPath:  strings.TrimSpace(detail.BackdropPath),
			Rating:        detail.VoteAverage,
			IsAnime:       isAnimeTMDB(genreIDs, detail.OriginalLanguage, countries),
		}, 0, nil
	}

	var detail struct {
		ID               int      `json:"id"`
		Name             string   `json:"name"`
		OriginalName     string   `json:"original_name"`
		Overview         string   `json:"overview"`
		FirstAirDate     string   `json:"first_air_date"`
		PosterPath       string   `json:"poster_path"`
		BackdropPath     string   `json:"backdrop_path"`
		VoteAverage      float64  `json:"vote_average"`
		OriginalLanguage string   `json:"original_language"`
		OriginCountry    []string `json:"origin_country"`
		Genres           []struct {
			ID int `json:"id"`
		} `json:"genres"`
		ExternalIDs struct {
			TVDBID int `json:"tvdb_id"`
		} `json:"external_ids"`
	}
	q := url.Values{}
	q.Set("append_to_response", "external_ids")
	if err := c.getJSON(ctx, fmt.Sprintf("/tv/%d", tmdbID), q, &detail); err != nil {
		return DiscoverItem{}, 0, err
	}
	genreIDs := make([]int, 0, len(detail.Genres))
	for _, g := range detail.Genres {
		genreIDs = append(genreIDs, g.ID)
	}
	title := strings.TrimSpace(detail.Name)
	orig := strings.TrimSpace(detail.OriginalName)
	if strings.EqualFold(orig, title) {
		orig = ""
	}
	var yr *int
	if len(detail.FirstAirDate) >= 4 {
		if y, err := strconv.Atoi(detail.FirstAirDate[:4]); err == nil && y > 1880 {
			yr = &y
		}
	}
	return DiscoverItem{
		MediaType:     "tv",
		TMDBID:        detail.ID,
		Title:         title,
		OriginalTitle: orig,
		Year:          yr,
		Overview:      strings.TrimSpace(detail.Overview),
		PosterPath:    strings.TrimSpace(detail.PosterPath),
		BackdropPath:  strings.TrimSpace(detail.BackdropPath),
		Rating:        detail.VoteAverage,
		IsAnime:       isAnimeTMDB(genreIDs, detail.OriginalLanguage, detail.OriginCountry),
	}, detail.ExternalIDs.TVDBID, nil
}

func (c *TMDBClient) getJSON(ctx context.Context, endpoint string, q url.Values, dst any) error {
	if q == nil {
		q = url.Values{}
	}
	if c.apiKey != "" {
		q.Set("api_key", c.apiKey)
	}
	fullURL := c.baseURL + endpoint
	if encoded := q.Encode(); encoded != "" {
		fullURL += "?" + encoded
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fullURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("tmdb %s returned status %d", endpoint, resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (c *TMDBClient) downloadArtwork(ctx context.Context, subdir, name, sizeAndPath string) (string, error) {
	return c.downloadArtworkOpts(ctx, subdir, name, sizeAndPath, false)
}

func (c *TMDBClient) downloadArtworkOpts(ctx context.Context, subdir, name, sizeAndPath string, force bool) (string, error) {
	dir := filepath.Join(c.artworkDir, subdir)
	dst := filepath.Join(dir, name)
	// Skip re-downloading if already cached on disk unless explicitly forcing a re-match refresh.
	if !force {
		if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
			return dst, nil
		}
	}

	// Deduplicate concurrent requests for the same artwork file.
	// Detach the upstream fetch from caller cancellation so navigating away mid-load
	// does not abort the cache fill or poison concurrent waiters.
	if !force {
		c.mu.Lock()
		if c.artInflight == nil {
			c.artInflight = make(map[string]*artFlight)
		}
		fl, exists := c.artInflight[dst]
		if !exists {
			fl = &artFlight{done: make(chan struct{})}
			c.artInflight[dst] = fl
			go func() {
				bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
				defer cancel()
				path, err := c.fetchAndSaveArtwork(bgCtx, dir, dst, sizeAndPath)
				fl.path, fl.err = path, err
				close(fl.done)

				c.mu.Lock()
				delete(c.artInflight, dst)
				c.mu.Unlock()
			}()
		}
		c.mu.Unlock()

		select {
		case <-fl.done:
			return fl.path, fl.err
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	return c.fetchAndSaveArtwork(ctx, dir, dst, sizeAndPath)
}

func (c *TMDBClient) fetchAndSaveArtwork(ctx context.Context, dir, dst, sizeAndPath string) (string, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.imageBase+sizeAndPath, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return "", fmt.Errorf("%w: artwork status 404", ErrArtworkNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("artwork status %d", resp.StatusCode)
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	if _, err := io.Copy(tmp, io.LimitReader(resp.Body, 10<<20)); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, dst); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	return dst, nil
}

// FetchPersonPhoto lazily downloads and caches a TMDB person portrait under <DataDir>/artwork/people/.
func (c *TMDBClient) FetchPersonPhoto(ctx context.Context, profilePath string) (string, error) {
	if c == nil {
		return "", fmt.Errorf("%w: tmdb client not configured", ErrArtworkNotFound)
	}
	clean := strings.TrimPrefix(strings.TrimSpace(profilePath), "/")
	if !isSafeArtFilename(clean) {
		return "", fmt.Errorf("%w: invalid person photo path", ErrArtworkNotFound)
	}
	dst := filepath.Join(c.artworkDir, "people", clean)
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}
	if c.personArtSem != nil {
		select {
		case c.personArtSem <- struct{}{}:
			defer func() { <-c.personArtSem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return c.downloadArtwork(ctx, "people", clean, "/w185/"+clean)
}

func isSafeArtFilename(name string) bool {
	if name == "" || len(name) > 128 || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	lower := strings.ToLower(name)
	if !strings.HasSuffix(lower, ".jpg") && !strings.HasSuffix(lower, ".jpeg") && !strings.HasSuffix(lower, ".png") && !strings.HasSuffix(lower, ".webp") {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.') {
			return false
		}
	}
	return true
}

func appendUnique(dst []string, val string, max int) []string {
	if len(dst) >= max {
		return dst
	}
	for _, existing := range dst {
		if strings.EqualFold(existing, val) {
			return dst
		}
	}
	return append(dst, val)
}
