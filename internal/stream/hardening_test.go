package stream

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LuneServer/lune/internal/store"
)

func setupStreamHardeningTest(t *testing.T) (*Service, *store.Store, store.User, store.MediaItem, string) {
	t.Helper()
	ctx := context.Background()
	dataDir := t.TempDir()
	db, err := store.Open(ctx, filepath.Join(dataDir, "lune.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	user := store.User{
		ID:           "user-1",
		Username:     "alice",
		DisplayName:  "Alice",
		PasswordHash: "hash",
		Role:         "member",
		AllLibraries: true,
	}
	if err := db.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}

	mediaDir := t.TempDir()
	mediaFile := filepath.Join(mediaDir, "sample.mp4")
	if err := os.WriteFile(mediaFile, []byte("fake-mp4-data"), 0o644); err != nil {
		t.Fatalf("WriteFile media: %v", err)
	}
	subFile := filepath.Join(mediaDir, "sample.vtt")
	if err := os.WriteFile(subFile, []byte("WEBVTT\n\n00:00:00.000 --> 00:00:02.000\nHello\n"), 0o644); err != nil {
		t.Fatalf("WriteFile sub: %v", err)
	}

	lib := store.Library{
		ID:       "lib-1",
		Name:     "Movies",
		Kind:     "movies",
		RootPath: mediaDir,
	}
	if err := db.CreateLibrary(ctx, lib); err != nil {
		t.Fatalf("CreateLibrary: %v", err)
	}

	item := store.MediaItem{
		ID:            "item-1",
		LibraryID:     lib.ID,
		Kind:          "movie",
		Title:         "Sample",
		SortTitle:     "sample",
		FilePath:      mediaFile,
		FileSize:      13,
		Container:     "mp4",
		VideoCodec:    "h264",
		AudioCodec:    "aac",
		SubtitlesJSON: `[{"index":0,"language":"en","label":"English","format":"vtt","path":` + jsonQuote(subFile) + `}]`,
	}
	if err := db.UpsertMediaItem(ctx, item); err != nil {
		t.Fatalf("UpsertMediaItem: %v", err)
	}

	svc, err := NewService(ctx, db, dataDir, nil)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Shutdown)

	return svc, db, user, item, dataDir
}

func jsonQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(s) + `"`
}

func TestStoppedHLSSessionReturns401AndCannotRestart(t *testing.T) {
	svc, _, user, item, _ := setupStreamHardeningTest(t)

	now := svc.now().UTC()
	sessionID := "hls-sess-revoked"
	svc.mu.Lock()
	svc.active[sessionID] = &ActiveStream{
		SessionID:  sessionID,
		UserID:     user.ID,
		Username:   user.Username,
		ItemID:     item.ID,
		ItemTitle:  item.Title,
		Mode:       "transcode",
		StartedAt:  now,
		LastSeenAt: now,
	}
	svc.mu.Unlock()

	token, err := SignToken(svc.secret, TokenClaims{
		SessionID: sessionID,
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      "transcode",
		ExpiresAt: now.Add(streamTokenTTL).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}

	// Stop the session; the 6h HMAC token is still cryptographically valid,
	// but ServeHLS must reject it with 401 and must never spawn FFmpeg.
	if err := svc.StopSessionForUser(sessionID, user); err != nil {
		t.Fatalf("StopSessionForUser: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/hls/"+token+"/index.m3u8", nil)
	svc.ServeHLS(rec, req, token, "index.m3u8")

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for stopped HLS session, got %d (%s)", rec.Code, rec.Body.String())
	}

	svc.transcoder.mu.Lock()
	_, restarted := svc.transcoder.sessions[sessionID]
	svc.transcoder.mu.Unlock()
	if restarted {
		t.Fatalf("expected stopped HLS session not to restart in transcoder")
	}
}

func TestDisabledUserCannotStreamWithExistingToken(t *testing.T) {
	ctx := context.Background()
	svc, db, user, item, _ := setupStreamHardeningTest(t)

	now := svc.now().UTC()
	directToken, err := SignToken(svc.secret, TokenClaims{
		SessionID: "sess-direct",
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      "direct",
		ExpiresAt: now.Add(streamTokenTTL).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken direct: %v", err)
	}

	hlsSessID := "sess-hls-disabled"
	svc.mu.Lock()
	svc.active[hlsSessID] = &ActiveStream{
		SessionID:  hlsSessID,
		UserID:     user.ID,
		ItemID:     item.ID,
		Mode:       "transcode",
		StartedAt:  now,
		LastSeenAt: now,
	}
	svc.mu.Unlock()

	hlsToken, err := SignToken(svc.secret, TokenClaims{
		SessionID: hlsSessID,
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      "transcode",
		ExpiresAt: now.Add(streamTokenTTL).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken hls: %v", err)
	}

	// Verify direct stream works while enabled.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/direct/"+directToken, nil)
	svc.ServeDirect(rec, req, directToken)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 while user enabled, got %d", rec.Code)
	}

	// Disable the user account.
	if err := db.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatalf("SetUserDisabled: %v", err)
	}

	// Direct play must return 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/stream/direct/"+directToken, nil)
	svc.ServeDirect(rec, req, directToken)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled user on ServeDirect, got %d", rec.Code)
	}

	// HLS must return 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/stream/hls/"+hlsToken+"/index.m3u8", nil)
	svc.ServeHLS(rec, req, hlsToken, "index.m3u8")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled user on ServeHLS, got %d", rec.Code)
	}

	// Subtitles must return 403.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/stream/sub/"+directToken+"/0.vtt", nil)
	svc.ServeSubtitle(rec, req, directToken, "0.vtt")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 for disabled user on ServeSubtitle, got %d", rec.Code)
	}
}

func TestTranscodeConcurrencyLimits(t *testing.T) {
	svc, _, user, item, _ := setupStreamHardeningTest(t)

	// Force ffmpegBin to a non-empty value so limit checks run even if ffmpeg is absent.
	svc.transcoder.ffmpegBin = "/usr/bin/true"

	// Populate 3 active transcode sessions for user-1 (hits defaultMaxUserTranscodes = 3).
	svc.transcoder.mu.Lock()
	for _, id := range []string{"t1", "t2", "t3"} {
		done := make(chan struct{})
		close(done)
		svc.transcoder.sessions[id] = &transcodeSession{
			id:         id,
			userID:     user.ID,
			dir:        t.TempDir(),
			cancel:     func() {},
			done:       done,
			lastAccess: time.Now(),
		}
	}
	svc.transcoder.mu.Unlock()

	// EnsureSession for a 4th session by the same user must return ErrTooManyTranscodes.
	_, err := svc.transcoder.EnsureSession(context.Background(), "t4", user.ID, item.FilePath, TranscodeOptions{Mode: "transcode"}, "index.m3u8")
	if !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("expected ErrTooManyTranscodes for 4th user session, got %v", err)
	}

	// ServeHLS must map ErrTooManyTranscodes to HTTP 429.
	now := svc.now().UTC()
	svc.mu.Lock()
	svc.active["t4"] = &ActiveStream{
		SessionID:  "t4",
		UserID:     user.ID,
		ItemID:     item.ID,
		Mode:       "transcode",
		StartedAt:  now,
		LastSeenAt: now,
	}
	svc.mu.Unlock()

	tok, err := SignToken(svc.secret, TokenClaims{
		SessionID: "t4",
		UserID:    user.ID,
		ItemID:    item.ID,
		Mode:      "transcode",
		ExpiresAt: now.Add(streamTokenTTL).Unix(),
	})
	if err != nil {
		t.Fatalf("SignToken: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/hls/"+tok+"/index.m3u8", nil)
	svc.ServeHLS(rec, req, tok, "index.m3u8")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 Too Many Requests when user transcode cap exceeded, got %d (%s)", rec.Code, rec.Body.String())
	}

	// Negotiate for a transcode must also return ErrTooManyTranscodes.
	_, err = svc.Negotiate(context.Background(), user, item.ID, ClientCapabilities{ForceTranscode: true}, "127.0.0.1")
	if !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("expected ErrTooManyTranscodes from Negotiate, got %v", err)
	}

	// Default config has MaxGlobalTranscodes == 0 (unlimited) and MaxUserTranscodes == 3.
	if cfg := svc.transcoder.GetConfig(); cfg.MaxGlobalTranscodes != 0 || cfg.MaxUserTranscodes != 3 || cfg.IdleTimeoutMinutes != 10 {
		t.Fatalf("unexpected default streaming config: %+v", cfg)
	}

	// Another user is allowed even with many distinct users active while global cap is unlimited (0).
	svc.transcoder.mu.Lock()
	for i := 4; i <= 15; i++ {
		id := fmt.Sprintf("g-%d", i)
		done := make(chan struct{})
		close(done)
		svc.transcoder.sessions[id] = &transcodeSession{
			id:         id,
			userID:     "other-" + id,
			dir:        t.TempDir(),
			cancel:     func() {},
			done:       done,
			lastAccess: time.Now(),
		}
	}
	svc.transcoder.mu.Unlock()

	if err := svc.transcoder.CheckLimits("user-brand-new"); err != nil {
		t.Fatalf("expected unlimited global cap (0) to allow distinct user, got %v", err)
	}
	// Per-user cap (3) is still enforced for user-1.
	if err := svc.transcoder.CheckLimits(user.ID); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("expected per-user cap (3) to remain enforced while global cap is 0, got %v", err)
	}

	// Explicitly configuring a finite global cap enforces it live.
	svc.transcoder.UpdateConfig(StreamingConfig{
		MaxGlobalTranscodes: 2,
		MaxUserTranscodes:   3,
		IdleTimeoutMinutes:  10,
	})
	if err := svc.transcoder.CheckLimits("user-brand-new"); !errors.Is(err, ErrTooManyTranscodes) {
		t.Fatalf("expected ErrTooManyTranscodes when global cap (2) is exceeded, got %v", err)
	}
}

func TestTranscoderStartupCleanupAndPathValidation(t *testing.T) {
	dataDir := t.TempDir()
	orphanDir := filepath.Join(dataDir, "transcode", "orphaned-session")
	subsDir := filepath.Join(dataDir, "transcode", "subs")
	if err := os.MkdirAll(orphanDir, 0o750); err != nil {
		t.Fatalf("MkdirAll orphan: %v", err)
	}
	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		t.Fatalf("MkdirAll subs: %v", err)
	}
	subCache := filepath.Join(subsDir, "cached.vtt")
	if err := os.WriteFile(subCache, []byte("WEBVTT\n"), 0o644); err != nil {
		t.Fatalf("WriteFile subCache: %v", err)
	}

	tr := NewTranscoder(dataDir, nil)
	defer tr.Close()

	if _, err := os.Stat(orphanDir); !os.IsNotExist(err) {
		t.Fatalf("expected orphaned transcode directory to be removed on startup")
	}
	if _, err := os.Stat(subCache); err != nil {
		t.Fatalf("expected subs cache to be preserved on startup: %v", err)
	}

	tr.ffmpegBin = "/usr/bin/true"
	if _, err := tr.EnsureSession(context.Background(), "s1", "u1", "relative/video.mp4", TranscodeOptions{Mode: "transcode"}, "index.m3u8"); err == nil {
		t.Fatalf("expected error for relative input path in EnsureSession")
	}
	if _, err := tr.ExtractSubtitleVTT(context.Background(), "k1", "-dash-file.mkv", 0); err == nil {
		t.Fatalf("expected error for relative input path in ExtractSubtitleVTT")
	}
}

func TestStopSessionForUserDoesNotKillUnknownSessionForNonAdmin(t *testing.T) {
	svc, _, user, _, _ := setupStreamHardeningTest(t)

	stopped := false
	done := make(chan struct{})
	close(done)
	svc.transcoder.mu.Lock()
	svc.transcoder.sessions["orphan-transcode"] = &transcodeSession{
		id:         "orphan-transcode",
		userID:     "other-user",
		dir:        t.TempDir(),
		cancel:     func() { stopped = true },
		done:       done,
		lastAccess: time.Now(),
	}
	svc.transcoder.mu.Unlock()

	// Non-admin calling StopSessionForUser on a session not in s.active must not stop transcoder.
	if err := svc.StopSessionForUser("orphan-transcode", user); err != nil {
		t.Fatalf("StopSessionForUser: %v", err)
	}
	if stopped {
		t.Fatalf("expected non-admin StopSessionForUser not to stop unknown transcoder session")
	}

	// Admin can still stop it.
	admin := store.User{ID: "admin-1", Role: "admin"}
	if err := svc.StopSessionForUser("orphan-transcode", admin); err != nil {
		t.Fatalf("admin StopSessionForUser: %v", err)
	}
	if !stopped {
		t.Fatalf("expected admin StopSessionForUser to stop transcoder session")
	}
}
