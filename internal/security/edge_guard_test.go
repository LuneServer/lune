package security

import (
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestEdgeGuardRateLimitAndAutoBan(t *testing.T) {
	now := time.Date(2026, 11, 25, 10, 0, 0, 0, time.UTC)
	guard := NewEdgeGuardWithClock(func() time.Time { return now })

	ip := netip.MustParseAddr("203.0.113.10")
	// Public burst is 30; 31st immediate request should be rejected with 429.
	for i := 0; i < 30; i++ {
		ok, _, _ := guard.CheckRequest(ip, false, false)
		if !ok {
			t.Fatalf("expected request %d within burst to succeed", i+1)
		}
	}
	if ok, status, retry := guard.CheckRequest(ip, false, false); ok || status != http.StatusTooManyRequests || retry <= 0 {
		t.Fatalf("expected 429 after burst exhausted, got ok=%v status=%d retry=%v", ok, status, retry)
	}

	// Streams bypass rate limit when not banned.
	if ok, _, _ := guard.CheckRequest(ip, false, true); !ok {
		t.Fatalf("expected stream bypass to succeed when IP is not banned")
	}

	// Trigger auto-ban via abuse strikes (5 * 3 = 15).
	var bannedIP string
	guard.SetOnBan(func(bIP, reason string, dur time.Duration, permanent bool) {
		bannedIP = bIP
	})
	for i := 0; i < 5; i++ {
		guard.RecordAbuse("203.0.113.10", "invalid stream token", 3)
	}
	if bannedIP != "203.0.113.10" {
		t.Fatalf("expected onBan callback for 203.0.113.10, got %q", bannedIP)
	}
	if ok, status, _ := guard.CheckRequest(ip, true, true); ok || status != http.StatusTooManyRequests {
		t.Fatalf("expected banned IP to be blocked even on stream bypass, got ok=%v status=%d", ok, status)
	}
	if bans := guard.ListBans(); len(bans) != 1 || bans[0].IP != "203.0.113.10" {
		t.Fatalf("expected 1 active ban, got %+v", bans)
	}

	// Unban restores access.
	if !guard.UnbanIP("203.0.113.10") {
		t.Fatalf("expected UnbanIP to return true")
	}
	if ok, _, _ := guard.CheckRequest(ip, true, true); !ok {
		t.Fatalf("expected access restored after unban")
	}
}

func TestEdgeGuardAllowlistAndBlocklist(t *testing.T) {
	guard := NewEdgeGuard()
	allow := []netip.Prefix{netip.MustParsePrefix("198.51.100.0/24")}
	block := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	guard.UpdateLists(allow, block)

	allowedIP := netip.MustParseAddr("198.51.100.42")
	for i := 0; i < 200; i++ {
		if ok, _, _ := guard.CheckRequest(allowedIP, false, false); !ok {
			t.Fatalf("allowlisted IP should never be rate-limited")
		}
	}

	blockedIP := netip.MustParseAddr("192.0.2.99")
	if ok, status, _ := guard.CheckRequest(blockedIP, true, true); ok || status != http.StatusForbidden {
		t.Fatalf("expected 403 for blocklisted IP, got ok=%v status=%d", ok, status)
	}
}

func TestCompressMiddlewareSkipsStreamsAndCompressesJSON(t *testing.T) {
	h := CompressMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/stream/") {
			w.Header().Set("Content-Type", "video/mp4")
			_, _ = w.Write([]byte("raw-video-bytes"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"hello":"world","items":[1,2,3,4,5]}`))
	}))

	// JSON should be gzipped.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/system/info", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("expected gzip Content-Encoding for JSON, got %q", rec.Header().Get("Content-Encoding"))
	}
	gr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("failed to open gzip reader: %v", err)
	}
	body, _ := io.ReadAll(gr)
	_ = gr.Close()
	if !strings.Contains(string(body), `"hello":"world"`) {
		t.Fatalf("unexpected decompressed body: %q", string(body))
	}

	// Stream should NOT be gzipped.
	req2 := httptest.NewRequest(http.MethodGet, "/stream/direct/abc", nil)
	req2.Header.Set("Accept-Encoding", "gzip")
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Header().Get("Content-Encoding") != "" {
		t.Fatalf("expected no compression on /stream/, got %q", rec2.Header().Get("Content-Encoding"))
	}
	if rec2.Body.String() != "raw-video-bytes" {
		t.Fatalf("unexpected stream body: %q", rec2.Body.String())
	}
}

func TestEdgeGuardLoadBansAndUnbanCallback(t *testing.T) {
	now := time.Date(2026, 11, 25, 11, 0, 0, 0, time.UTC)
	guard := NewEdgeGuardWithClock(func() time.Time { return now })

	future := now.Add(time.Hour)
	past := now.Add(-time.Minute)
	guard.LoadBans([]IPBan{
		{IP: "203.0.113.1", Reason: "permanent ban", BannedAt: now, Permanent: true},
		{IP: "203.0.113.2", Reason: "active temp", BannedAt: now, ExpiresAt: &future},
		{IP: "203.0.113.3", Reason: "expired temp", BannedAt: past, ExpiresAt: &past},
	})

	if ok, status, _ := guard.CheckRequest(netip.MustParseAddr("203.0.113.1"), true, true); ok || status != http.StatusForbidden {
		t.Fatalf("expected permanent loaded ban to return 403, got ok=%v status=%d", ok, status)
	}
	if ok, status, _ := guard.CheckRequest(netip.MustParseAddr("203.0.113.2"), true, true); ok || status != http.StatusTooManyRequests {
		t.Fatalf("expected active temp loaded ban to return 429, got ok=%v status=%d", ok, status)
	}
	if ok, _, _ := guard.CheckRequest(netip.MustParseAddr("203.0.113.3"), true, true); !ok {
		t.Fatalf("expected expired ban to be ignored on load")
	}

	var unbanned string
	guard.SetOnUnban(func(ip string) { unbanned = ip })
	guard.UnbanIP("203.0.113.1")
	if unbanned != "203.0.113.1" {
		t.Fatalf("expected onUnban callback for 203.0.113.1, got %q", unbanned)
	}
}
