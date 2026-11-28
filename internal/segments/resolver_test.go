package segments

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

type memCache struct {
	mu   sync.Mutex
	rows map[string]struct {
		payload string
		at      time.Time
	}
}

func newMemCache() *memCache {
	return &memCache{rows: map[string]struct {
		payload string
		at      time.Time
	}{}}
}

func (m *memCache) GetIntroDBCache(_ context.Context, key string) (string, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[key]
	if !ok {
		return "", time.Time{}, store.ErrNotFound
	}
	return row.payload, row.at, nil
}

func (m *memCache) PutIntroDBCache(_ context.Context, key, payload string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[key] = struct {
		payload string
		at      time.Time
	}{payload, at}
	return nil
}

func (m *memCache) PruneIntroDBCache(context.Context, time.Time) (int64, error) { return 0, nil }

type fakeFetcher struct {
	calls atomic.Int32
	delay time.Duration
	resp  IntroDBResponse
	err   error
}

func (f *fakeFetcher) Segments(ctx context.Context, q IntroDBQuery) (IntroDBResponse, error) {
	f.calls.Add(1)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return IntroDBResponse{}, ctx.Err()
		}
	}
	return f.resp, f.err
}

func intp(v int) *int { return &v }

func episode() store.MediaItem {
	return store.MediaItem{
		ID: "ep1", Kind: "episode", IMDBID: "tt0903747",
		SeasonNumber: intp(1), EpisodeNumber: intp(2), DurationMS: 47 * 60 * 1000,
	}
}

func TestQueryFor(t *testing.T) {
	q, ok := QueryFor(episode())
	if !ok || q.Key() != "tt0903747:1:2" {
		t.Fatalf("episode query = %+v ok=%v", q, ok)
	}
	q, ok = QueryFor(store.MediaItem{Kind: "movie", IMDBID: " TT1160419 "})
	if !ok || !q.IsMovie || q.Key() != "tt1160419:movie" {
		t.Fatalf("movie query = %+v ok=%v", q, ok)
	}
	for _, item := range []store.MediaItem{
		{Kind: "movie"},
		{Kind: "movie", IMDBID: "nm0000123"},
		{Kind: "episode", IMDBID: "tt0903747"},
		{Kind: "track", IMDBID: "tt0903747"},
	} {
		if _, ok := QueryFor(item); ok {
			t.Errorf("QueryFor(%+v) should not be ok", item)
		}
	}
}

func TestFromChaptersMergesAdjacentParts(t *testing.T) {
	segs := FromChapters([]metadata.ChapterInfo{
		{Kind: "chapter", StartMS: 0, EndMS: 60000},
		{Kind: "intro", StartMS: 60000, EndMS: 90000},
		{Kind: "intro", StartMS: 90000, EndMS: 150000},
		{Kind: "chapter", StartMS: 150000, EndMS: 1300000},
		{Kind: "outro", StartMS: 1300000, EndMS: 1390000},
		{Kind: "preview", StartMS: 1390000, EndMS: 1420000},
	})
	if len(segs) != 3 {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[0] != (Segment{Kind: KindIntro, StartMS: 60000, EndMS: 150000, Source: SourceChapter}) {
		t.Fatalf("intro = %+v", segs[0])
	}
	if segs[1].Kind != KindOutro || segs[2].Kind != KindPreview {
		t.Fatalf("kinds = %+v", segs)
	}
}

func TestResolveChaptersWinAndIntroDBFillsGaps(t *testing.T) {
	f := &fakeFetcher{resp: IntroDBResponse{
		Intro: &IntroDBSegment{StartMS: 1000, EndMS: 50000},
		Recap: &IntroDBSegment{StartMS: 0, EndMS: 900},              // too short: dropped
		Outro: &IntroDBSegment{StartMS: 2700000, EndMS: 99_000_000}, // past the file: dropped
	}}
	r := NewResolver(newMemCache(), f, nil)
	chapters := []metadata.ChapterInfo{{Kind: "intro", StartMS: 5000, EndMS: 65000}}
	segs := r.Resolve(context.Background(), episode(), chapters)
	if len(segs) != 1 || segs[0].Source != SourceChapter || segs[0].StartMS != 5000 {
		t.Fatalf("segments = %+v", segs)
	}

	f2 := &fakeFetcher{resp: IntroDBResponse{
		Intro: &IntroDBSegment{StartMS: 1000, EndMS: 50000},
		Outro: &IntroDBSegment{StartMS: 2700000, EndMS: 2822000}, // within 5s slack: clamped
	}}
	r2 := NewResolver(newMemCache(), f2, nil)
	segs = r2.Resolve(context.Background(), episode(), nil)
	if len(segs) != 2 {
		t.Fatalf("segments = %+v", segs)
	}
	if segs[0] != (Segment{Kind: KindIntro, StartMS: 1000, EndMS: 50000, Source: SourceIntroDB}) {
		t.Fatalf("intro = %+v", segs[0])
	}
	if segs[1].Kind != KindOutro || segs[1].EndMS != episode().DurationMS {
		t.Fatalf("outro = %+v", segs[1])
	}
}

func TestResolveCachesAndDisables(t *testing.T) {
	f := &fakeFetcher{resp: IntroDBResponse{Intro: &IntroDBSegment{StartMS: 1000, EndMS: 50000}}}
	r := NewResolver(newMemCache(), f, nil)
	for range 3 {
		if segs := r.Resolve(context.Background(), episode(), nil); len(segs) != 1 {
			t.Fatalf("segments = %+v", segs)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("fetch calls = %d, want 1", n)
	}
	r.SetEnabled(false)
	if segs := r.Resolve(context.Background(), episode(), nil); len(segs) != 0 {
		t.Fatalf("disabled segments = %+v", segs)
	}
}

func TestResolveSlowLookupDoesNotBlockAndIsCachedLater(t *testing.T) {
	f := &fakeFetcher{delay: 200 * time.Millisecond, resp: IntroDBResponse{Intro: &IntroDBSegment{StartMS: 1000, EndMS: 50000}}}
	r := NewResolver(newMemCache(), f, nil)
	r.waitBudget = 20 * time.Millisecond
	start := time.Now()
	if segs := r.Resolve(context.Background(), episode(), nil); len(segs) != 0 {
		t.Fatalf("segments before fetch finished = %+v", segs)
	}
	if time.Since(start) > 150*time.Millisecond {
		t.Fatalf("Resolve waited %v, budget 20ms", time.Since(start))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if segs := r.Resolve(context.Background(), episode(), nil); len(segs) == 1 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("segments never cached")
}

func TestResolveBacksOffAfterError(t *testing.T) {
	f := &fakeFetcher{err: errors.New("boom")}
	r := NewResolver(newMemCache(), f, nil)
	for range 3 {
		r.Resolve(context.Background(), episode(), nil)
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("fetch calls = %d, want 1 (backoff)", n)
	}
}

func TestResolveServesStaleWhileRefreshing(t *testing.T) {
	cache := newMemCache()
	f := &fakeFetcher{resp: IntroDBResponse{Intro: &IntroDBSegment{StartMS: 1000, EndMS: 50000}}}
	r := NewResolver(cache, f, nil)
	r.Resolve(context.Background(), episode(), nil)
	r.now = func() time.Time { return time.Now().Add(hitTTL + time.Hour) }
	f.resp = IntroDBResponse{Intro: &IntroDBSegment{StartMS: 2000, EndMS: 52000}}
	segs := r.Resolve(context.Background(), episode(), nil)
	if len(segs) != 1 || segs[0].StartMS != 1000 {
		t.Fatalf("stale segments = %+v", segs)
	}
}

func TestIntroDBClientSegments(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/segments" || r.URL.Query().Get("imdb_id") != "tt0903747" ||
			r.URL.Query().Get("season") != "1" || r.URL.Query().Get("episode") != "2" {
			http.Error(w, `{"error":"bad"}`, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"imdb_id":"tt0903747","media_type":"tv","is_movie":false,"season":1,"episode":2,
			"intro":{"start_sec":2.5,"end_sec":58,"start_ms":2500,"end_ms":58000,"confidence":0.92,"submission_count":14,"updated_at":"2026-09-12T21:03:17.409Z"},
			"recap":null,"outro":null,"post_credits":null,"no_intro":false,"no_recap":true,"no_outro":false}`))
	}))
	defer srv.Close()
	c := NewIntroDBClient("Lune/test")
	c.BaseURL = srv.URL
	resp, err := c.Segments(context.Background(), IntroDBQuery{IMDBID: "tt0903747", Season: 1, Episode: 2})
	if err != nil {
		t.Fatalf("Segments: %v", err)
	}
	if resp.Intro == nil || resp.Intro.StartMS != 2500 || resp.Intro.EndMS != 58000 || resp.Recap != nil {
		t.Fatalf("response = %+v", resp)
	}
	if _, err := c.Segments(context.Background(), IntroDBQuery{IMDBID: "tt1", IsMovie: true}); err == nil {
		t.Fatal("expected HTTP error")
	}
}
