package requests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// QualityProfile represents a Radarr/Sonarr quality profile.
type QualityProfile struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RootFolder represents a Radarr/Sonarr root folder path.
type RootFolder struct {
	ID        int    `json:"id"`
	Path      string `json:"path"`
	FreeSpace int64  `json:"free_space"`
}

// ArrTestResult is returned when testing a Radarr or Sonarr v3 connection.
type ArrTestResult struct {
	OK              bool             `json:"ok"`
	AppName         string           `json:"app_name,omitempty"`
	Version         string           `json:"version,omitempty"`
	Error           string           `json:"error,omitempty"`
	QualityProfiles []QualityProfile `json:"quality_profiles"`
	RootFolders     []RootFolder     `json:"root_folders"`
}

// ArrClient communicates with Radarr v3 and Sonarr v3 HTTP APIs.
type ArrClient struct {
	httpClient *http.Client
}

// NewArrClient constructs an ArrClient with a 15-second timeout and same-origin redirect enforcement.
func NewArrClient(httpClient *http.Client) *ArrClient {
	var client http.Client
	if httpClient == nil {
		client = http.Client{Timeout: 15 * time.Second}
	} else {
		client = *httpClient
	}
	prevCheck := client.CheckRedirect
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("stopped after 10 redirects")
		}
		if len(via) > 0 {
			orig := via[0].URL
			if !strings.EqualFold(req.URL.Scheme, orig.Scheme) || !strings.EqualFold(req.URL.Host, orig.Host) {
				return fmt.Errorf("refusing cross-origin redirect from %s://%s to %s://%s", orig.Scheme, orig.Host, req.URL.Scheme, req.URL.Host)
			}
		}
		if prevCheck != nil {
			return prevCheck(req, via)
		}
		return nil
	}
	return &ArrClient{httpClient: &client}
}

func validateArrBaseURL(raw string) (string, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(raw), "/")
	if baseURL == "" {
		return "", fmt.Errorf("URL is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if (scheme != "http" && scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("URL must use http or https with a valid host")
	}
	return baseURL, nil
}

// TestConnection validates connectivity, API key, and fetches quality profiles and root folders.
func (c *ArrClient) TestConnection(ctx context.Context, service, baseURL, apiKey string) ArrTestResult {
	res := ArrTestResult{
		QualityProfiles: []QualityProfile{},
		RootFolders:     []RootFolder{},
	}
	baseURL, err := validateArrBaseURL(baseURL)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		res.Error = "API key is required"
		return res
	}

	var status struct {
		AppName string `json:"appName"`
		Version string `json:"version"`
	}
	if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/system/status", apiKey, nil, &status); err != nil {
		res.Error = err.Error()
		return res
	}
	res.AppName = status.AppName
	res.Version = status.Version

	var rawProfiles []struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	}
	if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/qualityprofile", apiKey, nil, &rawProfiles); err != nil {
		res.Error = fmt.Sprintf("connected to %s %s, but failed to fetch quality profiles: %v", status.AppName, status.Version, err)
		return res
	}
	for _, p := range rawProfiles {
		res.QualityProfiles = append(res.QualityProfiles, QualityProfile{ID: p.ID, Name: p.Name})
	}

	var rawFolders []struct {
		ID        int    `json:"id"`
		Path      string `json:"path"`
		FreeSpace int64  `json:"freeSpace"`
	}
	if err := c.doJSON(ctx, http.MethodGet, baseURL+"/api/v3/rootfolder", apiKey, nil, &rawFolders); err != nil {
		res.Error = fmt.Sprintf("connected to %s %s, but failed to fetch root folders: %v", status.AppName, status.Version, err)
		return res
	}
	for _, f := range rawFolders {
		res.RootFolders = append(res.RootFolders, RootFolder{ID: f.ID, Path: f.Path, FreeSpace: f.FreeSpace})
	}

	res.OK = true
	return res
}

// AddMovie looks up a movie by TMDB ID in Radarr and adds it if not already present.
// Returns Radarr's movie ID.
func (c *ArrClient) AddMovie(ctx context.Context, cfg ArrInstanceConfig, tmdbID int, title string) (int, error) {
	baseURL, err := validateArrBaseURL(cfg.URL)
	if err != nil {
		return 0, fmt.Errorf("radarr %w", err)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return 0, fmt.Errorf("radarr API key is required")
	}
	if cfg.QualityProfileID <= 0 {
		return 0, fmt.Errorf("radarr quality_profile_id must be configured")
	}
	if strings.TrimSpace(cfg.RootFolder) == "" {
		return 0, fmt.Errorf("radarr root_folder must be configured")
	}

	var lookup map[string]any
	lookupURL := fmt.Sprintf("%s/api/v3/movie/lookup/tmdb?tmdbId=%d", baseURL, tmdbID)
	if err := c.doJSON(ctx, http.MethodGet, lookupURL, cfg.APIKey, nil, &lookup); err != nil {
		return 0, fmt.Errorf("radarr lookup failed: %w", err)
	}
	if existingID := intFromAny(lookup["id"]); existingID > 0 {
		return existingID, nil
	}

	minAvail := strings.TrimSpace(cfg.MinimumAvailability)
	if minAvail == "" {
		minAvail = "released"
	}
	if lookup["title"] == nil || lookup["title"] == "" {
		lookup["title"] = title
	}
	lookup["tmdbId"] = tmdbID
	lookup["qualityProfileId"] = cfg.QualityProfileID
	lookup["rootFolderPath"] = cfg.RootFolder
	lookup["monitored"] = true
	lookup["minimumAvailability"] = minAvail
	lookup["addOptions"] = map[string]any{
		"searchForMovie": cfg.SearchOnAdd,
	}

	var created struct {
		ID int `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/movie", cfg.APIKey, lookup, &created); err != nil {
		return 0, fmt.Errorf("radarr add movie failed: %w", err)
	}
	return created.ID, nil
}

// AddSeries looks up a TV/anime series in Sonarr (by TVDB ID if available, falling back to TMDB ID)
// and adds it if not already present. Returns Sonarr's series ID.
func (c *ArrClient) AddSeries(ctx context.Context, cfg ArrInstanceConfig, tmdbID, tvdbID int, title string) (int, error) {
	baseURL, err := validateArrBaseURL(cfg.URL)
	if err != nil {
		return 0, fmt.Errorf("sonarr %w", err)
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return 0, fmt.Errorf("sonarr API key is required")
	}
	if cfg.QualityProfileID <= 0 {
		return 0, fmt.Errorf("sonarr quality_profile_id must be configured")
	}
	if strings.TrimSpace(cfg.RootFolder) == "" {
		return 0, fmt.Errorf("sonarr root_folder must be configured")
	}

	var results []map[string]any
	lookupTerms := make([]string, 0, 2)
	if tvdbID > 0 {
		lookupTerms = append(lookupTerms, "tvdb:"+strconv.Itoa(tvdbID))
	}
	if tmdbID > 0 {
		lookupTerms = append(lookupTerms, "tmdb:"+strconv.Itoa(tmdbID))
	}

	var lastErr error
	for _, term := range lookupTerms {
		lookupURL := baseURL + "/api/v3/series/lookup?term=" + url.QueryEscape(term)
		var batch []map[string]any
		if err := c.doJSON(ctx, http.MethodGet, lookupURL, cfg.APIKey, nil, &batch); err != nil {
			lastErr = err
			continue
		}
		if len(batch) > 0 {
			results = batch
			break
		}
	}
	if len(results) == 0 {
		if lastErr != nil {
			return 0, fmt.Errorf("sonarr lookup failed: %w", lastErr)
		}
		return 0, fmt.Errorf("sonarr lookup returned no series for %q", title)
	}

	target := results[0]
	if existingID := intFromAny(target["id"]); existingID > 0 {
		return existingID, nil
	}

	seriesType := strings.TrimSpace(cfg.SeriesType)
	if seriesType == "" {
		seriesType = "standard"
	}
	if target["title"] == nil || target["title"] == "" {
		target["title"] = title
	}
	target["qualityProfileId"] = cfg.QualityProfileID
	target["rootFolderPath"] = cfg.RootFolder
	target["seriesType"] = seriesType
	target["seasonFolder"] = cfg.SeasonFolder
	target["monitored"] = true
	target["addOptions"] = map[string]any{
		"searchForMissingEpisodes": cfg.SearchOnAdd,
	}

	var created struct {
		ID int `json:"id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, baseURL+"/api/v3/series", cfg.APIKey, target, &created); err != nil {
		return 0, fmt.Errorf("sonarr add series failed: %w", err)
	}
	return created.ID, nil
}

// ActiveQueueIDs returns the set of external Radarr movieIds or Sonarr seriesIds
// currently active in /api/v3/queue. Uses a tight 3s timeout so UI listing never stalls.
func (c *ArrClient) ActiveQueueIDs(ctx context.Context, isRadarr bool, cfg ArrInstanceConfig) (map[int]bool, error) {
	if strings.TrimSpace(cfg.URL) == "" || strings.TrimSpace(cfg.APIKey) == "" {
		return nil, nil
	}
	baseURL, err := validateArrBaseURL(cfg.URL)
	if err != nil {
		return nil, err
	}
	qCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	var resp struct {
		Records []struct {
			MovieID  int `json:"movieId"`
			SeriesID int `json:"seriesId"`
		} `json:"records"`
	}
	if err := c.doJSON(qCtx, http.MethodGet, baseURL+"/api/v3/queue?pageSize=200", cfg.APIKey, nil, &resp); err != nil {
		return nil, err
	}
	ids := make(map[int]bool, len(resp.Records))
	for _, rec := range resp.Records {
		if isRadarr && rec.MovieID > 0 {
			ids[rec.MovieID] = true
		} else if !isRadarr && rec.SeriesID > 0 {
			ids[rec.SeriesID] = true
		}
	}
	return ids, nil
}

func (c *ArrClient) doJSON(ctx context.Context, method, endpoint, apiKey string, body any, dst any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := strings.TrimSpace(string(raw))
		if len(msg) > 240 {
			msg = msg[:240] + "…"
		}
		if msg == "" {
			return fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, msg)
	}
	if dst != nil && len(raw) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	case json.Number:
		i, _ := n.Int64()
		return int(i)
	default:
		return 0
	}
}
