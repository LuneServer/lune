package stream

import (
	"testing"
	"time"
)

func TestHasActivePlayback_CountsDirectPlayAndHeartbeats(t *testing.T) {
	now := time.Date(2026, 11, 27, 20, 0, 0, 0, time.UTC)
	s := &Service{
		now:     func() time.Time { return now },
		active:  map[string]*ActiveStream{},
		revoked: map[string]time.Time{},
	}
	if s.HasActivePlayback() {
		t.Fatal("expected no playback with no sessions")
	}

	// A direct-play session (no FFmpeg process, e.g. Apple TV AVPlayer) counts while recent.
	s.active["sid"] = &ActiveStream{SessionID: "sid", UserID: "u1", ItemID: "i1", Mode: "direct", LastSeenAt: now}
	if !s.HasActivePlayback() {
		t.Fatal("expected direct-play session to count as active playback")
	}

	// AVPlayer buffers ahead, so a gap shorter than the window still counts.
	now = now.Add(PlaybackActivityWindow - time.Second)
	if !s.HasActivePlayback() {
		t.Fatal("expected session inside the activity window to count")
	}

	// Past the window with no range requests or heartbeats: idle.
	now = now.Add(2 * time.Second)
	if s.HasActivePlayback() {
		t.Fatal("expected stale session to stop counting as playback")
	}

	// A progress heartbeat for the same user+item revives it.
	s.touchActiveByUserItem("u1", "i1")
	if !s.HasActivePlayback() {
		t.Fatal("expected progress heartbeat to mark playback active again")
	}

	// Stopping the session ends playback immediately.
	s.mu.Lock()
	delete(s.active, "sid")
	s.mu.Unlock()
	if s.HasActivePlayback() {
		t.Fatal("expected no playback after the session is removed")
	}
}
