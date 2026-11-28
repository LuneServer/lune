// Package segments resolves skippable playback segments (intro, recap, credits, preview) for a
// media item by merging container chapter markers with community timestamps from IntroDB
// (https://introdb.app).
package segments

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// DefaultIntroDBBaseURL is the public IntroDB API. Reads need no API key.
const DefaultIntroDBBaseURL = "https://api.introdb.app"

// IntroDBQuery identifies a title on IntroDB. TV episodes use the series IMDb ID with
// TVDB aired-order season/episode numbers; movies set IsMovie and omit season/episode.
type IntroDBQuery struct {
	IMDBID  string
	Season  int
	Episode int
	IsMovie bool
}

// Key is the stable cache key for the query.
func (q IntroDBQuery) Key() string {
	if q.IsMovie {
		return q.IMDBID + ":movie"
	}
	return fmt.Sprintf("%s:%d:%d", q.IMDBID, q.Season, q.Episode)
}

// IntroDBSegment is one timed segment in an IntroDB response.
type IntroDBSegment struct {
	StartMS         int64   `json:"start_ms"`
	EndMS           int64   `json:"end_ms"`
	Confidence      float64 `json:"confidence"`
	SubmissionCount int     `json:"submission_count"`
}

// IntroDBResponse mirrors `GET /segments`. A nil segment means nobody has timed it yet.
type IntroDBResponse struct {
	IMDBID      string          `json:"imdb_id"`
	IsMovie     bool            `json:"is_movie"`
	Season      int             `json:"season"`
	Episode     int             `json:"episode"`
	Intro       *IntroDBSegment `json:"intro"`
	Recap       *IntroDBSegment `json:"recap"`
	Outro       *IntroDBSegment `json:"outro"`
	PostCredits *IntroDBSegment `json:"post_credits"`
}

// IntroDBClient is a minimal read-only IntroDB API client.
type IntroDBClient struct {
	BaseURL   string
	HTTP      *http.Client
	UserAgent string
}

// NewIntroDBClient returns a client for the public IntroDB API.
func NewIntroDBClient(userAgent string) *IntroDBClient {
	return &IntroDBClient{
		BaseURL:   DefaultIntroDBBaseURL,
		HTTP:      &http.Client{Timeout: 10 * time.Second},
		UserAgent: userAgent,
	}
}

// Segments fetches the segments for one title.
func (c *IntroDBClient) Segments(ctx context.Context, q IntroDBQuery) (IntroDBResponse, error) {
	params := url.Values{}
	params.Set("imdb_id", q.IMDBID)
	if q.IsMovie {
		params.Set("is_movie", "true")
	} else {
		params.Set("season", strconv.Itoa(q.Season))
		params.Set("episode", strconv.Itoa(q.Episode))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/segments?"+params.Encode(), nil)
	if err != nil {
		return IntroDBResponse{}, err
	}
	req.Header.Set("Accept", "application/json")
	if c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return IntroDBResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return IntroDBResponse{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return IntroDBResponse{}, fmt.Errorf("introdb: HTTP %d", resp.StatusCode)
	}
	var out IntroDBResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return IntroDBResponse{}, fmt.Errorf("introdb: decode: %w", err)
	}
	return out, nil
}
