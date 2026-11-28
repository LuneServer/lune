package segments

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

// Segment kinds. They match metadata.ChapterInfo.Kind values.
const (
	KindIntro   = "intro"
	KindRecap   = "recap"
	KindOutro   = "outro"
	KindPreview = "preview"
)

// Segment sources.
const (
	SourceChapter = "chapter"
	SourceIntroDB = "introdb"
)

// Segment is a skippable span of playback returned in PlaybackPlan.segments.
type Segment struct {
	Kind    string `json:"kind" enum:"intro,recap,outro,preview" doc:"intro, recap, outro (end credits) or preview (next-episode preview)"`
	StartMS int64  `json:"start_ms"`
	EndMS   int64  `json:"end_ms"`
	Source  string `json:"source" enum:"chapter,introdb" doc:"chapter (container chapter marker) or introdb (introdb.app community timestamps)"`
}

const (
	// hitTTL is how long a lookup that found segments stays fresh.
	hitTTL = 14 * 24 * time.Hour
	// missTTL is how long an empty lookup stays fresh (new submissions appear quickly).
	missTTL = 2 * 24 * time.Hour
	// errorBackoff suppresses repeat fetches of a key after a network/HTTP failure.
	errorBackoff = 15 * time.Minute
	// fetchTimeout bounds one detached IntroDB request.
	fetchTimeout = 10 * time.Second
	// DefaultWaitBudget is how long playback negotiation waits for an uncached lookup.
	DefaultWaitBudget = 1500 * time.Millisecond
	// durationSlackMS tolerates small runtime differences between IntroDB's source and the local file.
	durationSlackMS = 5000
	pruneEvery      = 24 * time.Hour
	pruneAge        = 90 * 24 * time.Hour
)

var imdbIDPattern = regexp.MustCompile(`^tt\d{5,10}$`)

// CacheStore persists IntroDB lookups.
type CacheStore interface {
	GetIntroDBCache(ctx context.Context, key string) (string, time.Time, error)
	PutIntroDBCache(ctx context.Context, key, payload string, fetchedAt time.Time) error
	PruneIntroDBCache(ctx context.Context, cutoff time.Time) (int64, error)
}

// Fetcher fetches IntroDB segments.
type Fetcher interface {
	Segments(ctx context.Context, q IntroDBQuery) (IntroDBResponse, error)
}

// Resolver merges chapter segments with cached IntroDB lookups.
type Resolver struct {
	cache      CacheStore
	fetcher    Fetcher
	logger     *slog.Logger
	now        func() time.Time
	waitBudget time.Duration
	enabled    atomic.Bool

	mu        sync.Mutex
	inflight  map[string]chan struct{}
	backoff   map[string]time.Time
	lastPrune time.Time
}

// NewResolver returns an enabled resolver.
func NewResolver(cache CacheStore, fetcher Fetcher, logger *slog.Logger) *Resolver {
	if logger == nil {
		logger = slog.Default()
	}
	r := &Resolver{
		cache:      cache,
		fetcher:    fetcher,
		logger:     logger,
		now:        time.Now,
		waitBudget: DefaultWaitBudget,
		inflight:   make(map[string]chan struct{}),
		backoff:    make(map[string]time.Time),
	}
	r.enabled.Store(true)
	return r
}

// SetEnabled turns IntroDB lookups on or off. Chapter segments are always returned.
func (r *Resolver) SetEnabled(on bool) {
	if r != nil {
		r.enabled.Store(on)
	}
}

// Enabled reports whether IntroDB lookups are on.
func (r *Resolver) Enabled() bool {
	return r != nil && r.enabled.Load()
}

// QueryFor derives the IntroDB query for an item. ok is false when the item cannot be looked up
// (no IMDb ID, or an episode without season/episode numbers).
func QueryFor(item store.MediaItem) (IntroDBQuery, bool) {
	imdb := strings.ToLower(strings.TrimSpace(item.IMDBID))
	if !imdbIDPattern.MatchString(imdb) {
		return IntroDBQuery{}, false
	}
	switch item.Kind {
	case "movie":
		return IntroDBQuery{IMDBID: imdb, IsMovie: true}, true
	case "episode":
		if item.SeasonNumber == nil || item.EpisodeNumber == nil || *item.SeasonNumber < 0 || *item.EpisodeNumber <= 0 {
			return IntroDBQuery{}, false
		}
		return IntroDBQuery{IMDBID: imdb, Season: *item.SeasonNumber, Episode: *item.EpisodeNumber}, true
	}
	return IntroDBQuery{}, false
}

// FromChapters converts skippable container chapters into segments.
func FromChapters(chapters []metadata.ChapterInfo) []Segment {
	out := make([]Segment, 0, 4)
	for _, ch := range chapters {
		switch ch.Kind {
		case KindIntro, KindRecap, KindOutro, KindPreview:
		default:
			continue
		}
		if ch.EndMS <= ch.StartMS {
			continue
		}
		// Adjacent chapters of one kind (e.g. "OP Part 1", "OP Part 2") become one segment.
		if n := len(out); n > 0 && out[n-1].Kind == ch.Kind && ch.StartMS-out[n-1].EndMS <= 1000 {
			out[n-1].EndMS = max(out[n-1].EndMS, ch.EndMS)
			continue
		}
		out = append(out, Segment{Kind: ch.Kind, StartMS: ch.StartMS, EndMS: ch.EndMS, Source: SourceChapter})
	}
	return out
}

// Resolve returns the item's skippable segments, sorted by start time. Chapter markers win:
// IntroDB only fills kinds the file's own chapters do not cover. An uncached IntroDB lookup is
// waited for up to the wait budget; a slower one keeps running and is cached for next time.
func (r *Resolver) Resolve(ctx context.Context, item store.MediaItem, chapters []metadata.ChapterInfo) []Segment {
	segs := FromChapters(chapters)
	if r.Enabled() {
		if q, ok := QueryFor(item); ok {
			if resp, ok := r.lookup(ctx, q, true); ok {
				segs = mergeIntroDB(segs, resp, item.DurationMS)
			}
		}
	}
	sort.SliceStable(segs, func(i, j int) bool { return segs[i].StartMS < segs[j].StartMS })
	return segs
}

// Prefetch warms the cache for an item (e.g. the next episode) without waiting.
func (r *Resolver) Prefetch(item store.MediaItem) {
	if !r.Enabled() {
		return
	}
	if q, ok := QueryFor(item); ok {
		r.lookup(context.Background(), q, false)
	}
}

func mergeIntroDB(segs []Segment, resp IntroDBResponse, durationMS int64) []Segment {
	covered := make(map[string]bool, len(segs))
	for _, s := range segs {
		covered[s.Kind] = true
	}
	add := func(kind string, seg *IntroDBSegment) {
		if seg == nil || covered[kind] {
			return
		}
		start, end := max(seg.StartMS, 0), seg.EndMS
		if durationMS > 0 {
			// A segment past the end of the file means IntroDB timed a different cut.
			if start >= durationMS || end > durationMS+durationSlackMS {
				return
			}
			end = min(end, durationMS)
		}
		if end-start < 1000 {
			return
		}
		segs = append(segs, Segment{Kind: kind, StartMS: start, EndMS: end, Source: SourceIntroDB})
	}
	add(KindRecap, resp.Recap)
	add(KindIntro, resp.Intro)
	add(KindOutro, resp.Outro)
	return segs
}

// lookup returns a cached response, fetching it when missing or stale. With wait, a missing entry
// is waited for up to the wait budget.
func (r *Resolver) lookup(ctx context.Context, q IntroDBQuery, wait bool) (IntroDBResponse, bool) {
	key := q.Key()
	cached, fresh, have := r.readCache(ctx, key)
	if fresh {
		return cached, true
	}
	done := r.startFetch(q)
	if have || !wait || done == nil {
		return cached, have
	}
	timer := time.NewTimer(r.waitBudget)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		return IntroDBResponse{}, false
	case <-ctx.Done():
		return IntroDBResponse{}, false
	}
	cached, _, have = r.readCache(ctx, key)
	return cached, have
}

func (r *Resolver) readCache(ctx context.Context, key string) (resp IntroDBResponse, fresh, ok bool) {
	payload, fetchedAt, err := r.cache.GetIntroDBCache(ctx, key)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			r.logger.Warn("introdb cache read failed", "key", key, "error", err)
		}
		return IntroDBResponse{}, false, false
	}
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return IntroDBResponse{}, false, false
	}
	ttl := missTTL
	if resp.Intro != nil || resp.Recap != nil || resp.Outro != nil {
		ttl = hitTTL
	}
	return resp, r.now().Sub(fetchedAt) < ttl, true
}

// startFetch starts (or joins) a detached fetch for q and returns a channel closed when it ends,
// or nil when the key is backing off after a recent failure.
func (r *Resolver) startFetch(q IntroDBQuery) <-chan struct{} {
	key := q.Key()
	r.mu.Lock()
	if ch, ok := r.inflight[key]; ok {
		r.mu.Unlock()
		return ch
	}
	if until, ok := r.backoff[key]; ok && r.now().Before(until) {
		r.mu.Unlock()
		return nil
	}
	ch := make(chan struct{})
	r.inflight[key] = ch
	prune := r.now().Sub(r.lastPrune) >= pruneEvery
	if prune {
		r.lastPrune = r.now()
	}
	r.mu.Unlock()

	go func() {
		defer func() {
			r.mu.Lock()
			delete(r.inflight, key)
			r.mu.Unlock()
			close(ch)
		}()
		ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
		defer cancel()
		if prune {
			if _, err := r.cache.PruneIntroDBCache(ctx, r.now().Add(-pruneAge)); err != nil {
				r.logger.Warn("introdb cache prune failed", "error", err)
			}
		}
		resp, err := r.fetcher.Segments(ctx, q)
		if err != nil {
			r.logger.Debug("introdb lookup failed", "key", key, "error", err)
			r.mu.Lock()
			if len(r.backoff) > 1024 {
				for k, until := range r.backoff {
					if r.now().After(until) {
						delete(r.backoff, k)
					}
				}
			}
			r.backoff[key] = r.now().Add(errorBackoff)
			r.mu.Unlock()
			return
		}
		payload, err := json.Marshal(resp)
		if err != nil {
			return
		}
		if err := r.cache.PutIntroDBCache(ctx, key, string(payload), r.now()); err != nil {
			r.logger.Warn("introdb cache write failed", "key", key, "error", err)
		}
		r.mu.Lock()
		delete(r.backoff, key)
		r.mu.Unlock()
	}()
	return ch
}
