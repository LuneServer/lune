package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	ErrFFmpegUnavailable = errors.New("ffmpeg binary not available for remux/transcoding")
	ErrInvalidSegment    = errors.New("invalid HLS segment name")
	ErrTooManyTranscodes = errors.New("too many concurrent transcode sessions")

	reAllowedHLSFile = regexp.MustCompile(`^(index\.m3u8|init\.mp4|seg_\d{5}\.(m4s|ts))$`)
)

const (
	StreamingConfigSettingKey  = "streaming_config"
	defaultMaxGlobalTranscodes = 0 // 0 = unlimited
	defaultMaxUserTranscodes   = 3 // 0 = unlimited
	defaultIdleTTL             = 10 * time.Minute
	defaultReaperInterval      = 60 * time.Second
	hlsSegmentSeconds          = 3
)

// TranscoderStatus reports detected FFmpeg and hardware acceleration diagnostics.
type TranscoderStatus struct {
	FFmpegAvailable bool     `json:"ffmpeg_available"`
	HWAccel         HWAccel  `json:"hw_accel"`
	VAAPIDevice     string   `json:"vaapi_device,omitempty"`
	HDRTonemap      bool     `json:"hdr_tonemap"`
	TonemapVAAPI    bool     `json:"tonemap_vaapi"`
	Libplacebo      bool     `json:"libplacebo"`
	ProbeNotes      []string `json:"probe_notes,omitempty"`
}

// StreamingConfig controls live transcoder concurrency caps and idle cleanup.
type StreamingConfig struct {
	MaxGlobalTranscodes int               `json:"max_global_transcodes"` // 0 = unlimited
	MaxUserTranscodes   int               `json:"max_user_transcodes"`   // 0 = unlimited
	IdleTimeoutMinutes  int               `json:"idle_timeout_minutes"`  // 1..120 (default 10)
	Transcoder          *TranscoderStatus `json:"transcoder,omitempty"`  // read-only runtime diagnostics
}

// HWAccel identifies the video encoder backend selected for transcoding.
type HWAccel string

const (
	HWAccelNone         HWAccel = "software"
	HWAccelVAAPI        HWAccel = "vaapi"
	HWAccelVideoToolbox HWAccel = "videotoolbox"
	HWAccelNVENC        HWAccel = "nvenc"
	HWAccelQSV          HWAccel = "qsv"
)

type limitedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
	max int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.buf.Len() < b.max {
		rem := b.max - b.buf.Len()
		if len(p) > rem {
			b.buf.Write(p[:rem])
		} else {
			b.buf.Write(p)
		}
	}
	return len(p), nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.TrimSpace(b.buf.String())
}

type transcodeSession struct {
	id         string
	userID     string
	dir        string
	hwAccel    HWAccel
	cancel     context.CancelFunc
	cmd        *exec.Cmd
	stderr     *limitedBuffer
	done       chan struct{}
	lastAccess time.Time
}

type subFlight struct {
	done chan struct{}
	path string
	err  error
}

// ActiveDirs returns the set of directory paths currently owned by live transcode sessions.
func (t *Transcoder) ActiveDirs() map[string]struct{} {
	out := make(map[string]struct{})
	if t == nil {
		return out
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, s := range t.sessions {
		if s.dir != "" {
			out[filepath.Clean(s.dir)] = struct{}{}
		}
	}
	return out
}

// Transcoder orchestrates on-demand FFmpeg HLS remux/transcode sessions.
type Transcoder struct {
	ffmpegBin        string
	ffprobeBin       string
	baseDir          string
	hwAccel          HWAccel
	vaapiDevice      string
	hasZscaleTonemap bool
	hasTonemapVAAPI  bool
	hasScaleVAAPI    bool
	hasLibplacebo    bool
	probeNotes       []string
	logger           *slog.Logger
	reaperInterval   time.Duration

	closeOnce  sync.Once
	reaperStop chan struct{}
	reaperDone chan struct{}

	mu        sync.Mutex
	maxGlobal int
	maxUser   int
	idleTTL   time.Duration
	sessions  map[string]*transcodeSession

	subMu      sync.Mutex
	subFlights map[string]*subFlight
}

func NewTranscoder(dataDir string, logger *slog.Logger) *Transcoder {
	if logger == nil {
		logger = slog.Default()
	}
	bin, _ := exec.LookPath("ffmpeg")
	probeBin, _ := exec.LookPath("ffprobe")
	baseDir := filepath.Join(dataDir, "transcode")

	// Clean up any stale orphaned transcode session directories from previous runs.
	if entries, err := os.ReadDir(baseDir); err == nil {
		for _, entry := range entries {
			if entry.Name() == "subs" {
				continue
			}
			_ = os.RemoveAll(filepath.Join(baseDir, entry.Name()))
		}
	}

	caps := probeFFmpegCapabilities(bin)
	if bin != "" {
		logger.Info("transcoder initialized",
			"hw_accel", caps.hwAccel,
			"vaapi_device", caps.vaapiDevice,
			"hdr_tonemap", caps.hasZscaleTonemap,
			"tonemap_vaapi", caps.hasTonemapVAAPI,
			"libplacebo", caps.hasLibplacebo,
			"probe_notes", caps.probeNotes,
		)
		if caps.hwAccel == HWAccelNone && len(discoverDRIRenderNodes()) > 0 {
			logger.Warn("GPU render device found under /dev/dri but hardware transcoding could not be initialized; using software libx264",
				"probe_notes", caps.probeNotes,
			)
		}
	}

	t := &Transcoder{
		ffmpegBin:        bin,
		ffprobeBin:       probeBin,
		baseDir:          baseDir,
		hwAccel:          caps.hwAccel,
		vaapiDevice:      caps.vaapiDevice,
		hasZscaleTonemap: caps.hasZscaleTonemap,
		hasTonemapVAAPI:  caps.hasTonemapVAAPI,
		hasScaleVAAPI:    caps.hasScaleVAAPI,
		hasLibplacebo:    caps.hasLibplacebo,
		probeNotes:       caps.probeNotes,
		logger:           logger,
		maxGlobal:        defaultMaxGlobalTranscodes,
		maxUser:          defaultMaxUserTranscodes,
		idleTTL:          defaultIdleTTL,
		reaperInterval:   defaultReaperInterval,
		reaperStop:       make(chan struct{}),
		reaperDone:       make(chan struct{}),
		sessions:         make(map[string]*transcodeSession),
		subFlights:       make(map[string]*subFlight),
	}
	go t.reaperLoop()
	return t
}

// Status returns the detected FFmpeg and hardware acceleration diagnostics.
func (t *Transcoder) Status() TranscoderStatus {
	if t == nil {
		return TranscoderStatus{HWAccel: HWAccelNone}
	}
	notes := make([]string, len(t.probeNotes))
	copy(notes, t.probeNotes)
	return TranscoderStatus{
		FFmpegAvailable: t.ffmpegBin != "",
		HWAccel:         t.hwAccel,
		VAAPIDevice:     t.vaapiDevice,
		HDRTonemap:      t.hasZscaleTonemap,
		TonemapVAAPI:    t.hasTonemapVAAPI,
		Libplacebo:      t.hasLibplacebo,
		ProbeNotes:      notes,
	}
}

// SessionHWAccel returns the active hardware acceleration backend for a running session.
func (t *Transcoder) SessionHWAccel(sessionID string) HWAccel {
	if t == nil {
		return ""
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if sess, ok := t.sessions[sessionID]; ok {
		return sess.hwAccel
	}
	return ""
}

// GetConfig returns the current live transcoder limits and idle timeout.
func (t *Transcoder) GetConfig() StreamingConfig {
	defaultIdleMin := int(defaultIdleTTL / time.Minute)
	if t == nil {
		return StreamingConfig{
			MaxGlobalTranscodes: defaultMaxGlobalTranscodes,
			MaxUserTranscodes:   defaultMaxUserTranscodes,
			IdleTimeoutMinutes:  defaultIdleMin,
		}
	}
	t.mu.Lock()
	idleMin := int(t.idleTTL / time.Minute)
	if idleMin <= 0 {
		idleMin = defaultIdleMin
	}
	cfg := StreamingConfig{
		MaxGlobalTranscodes: t.maxGlobal,
		MaxUserTranscodes:   t.maxUser,
		IdleTimeoutMinutes:  idleMin,
	}
	t.mu.Unlock()
	status := t.Status()
	cfg.Transcoder = &status
	return cfg
}

// UpdateConfig hot-reloads transcoder concurrency limits and idle timeout live.
func (t *Transcoder) UpdateConfig(cfg StreamingConfig) {
	if t == nil {
		return
	}
	if cfg.MaxGlobalTranscodes < 0 {
		cfg.MaxGlobalTranscodes = 0
	}
	if cfg.MaxUserTranscodes < 0 {
		cfg.MaxUserTranscodes = 0
	}
	idleTTL := time.Duration(cfg.IdleTimeoutMinutes) * time.Minute
	if idleTTL <= 0 {
		idleTTL = defaultIdleTTL
	}
	t.mu.Lock()
	t.maxGlobal = cfg.MaxGlobalTranscodes
	t.maxUser = cfg.MaxUserTranscodes
	t.idleTTL = idleTTL
	t.mu.Unlock()
}

func (t *Transcoder) reaperLoop() {
	defer close(t.reaperDone)
	ticker := time.NewTicker(t.reaperInterval)
	defer ticker.Stop()
	for {
		select {
		case <-t.reaperStop:
			return
		case <-ticker.C:
			t.reapIdle()
		}
	}
}

func (t *Transcoder) reapIdle() {
	now := time.Now()
	t.mu.Lock()
	ttl := t.idleTTL
	var expired []string
	for id, sess := range t.sessions {
		if now.Sub(sess.lastAccess) > ttl {
			expired = append(expired, id)
		}
	}
	t.mu.Unlock()
	for _, id := range expired {
		t.logger.Info("reaping idle hls transcode session", "session_id", id)
		t.Stop(id)
	}
}

// CheckLimits reports ErrTooManyTranscodes if starting another session for userID
// would exceed either the global or per-user concurrency cap.
func (t *Transcoder) CheckLimits(userID string) error {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.checkLimitsLocked(userID)
}

func (t *Transcoder) checkLimitsLocked(userID string) error {
	if t.maxGlobal > 0 && len(t.sessions) >= t.maxGlobal {
		return ErrTooManyTranscodes
	}
	if t.maxUser > 0 && userID != "" {
		userCount := 0
		for _, s := range t.sessions {
			if s.userID == userID {
				userCount++
			}
		}
		if userCount >= t.maxUser {
			return ErrTooManyTranscodes
		}
	}
	return nil
}

func (t *Transcoder) Available() bool {
	return t != nil && t.ffmpegBin != ""
}

func (t *Transcoder) HWAccel() HWAccel {
	return t.hwAccel
}

// TranscodeOptions configures on-demand FFmpeg remux/transcode output.
type TranscodeOptions struct {
	Mode                  string
	VideoCodec            string
	VideoProfile          string
	VideoPixFmt           string
	VideoHDR              string
	DVProfile             int
	DVCompatID            int
	SourceHeight          int
	AudioCopy             bool
	AudioChannels         int
	MaxAudioChannels      int
	MaxBitrate            int64
	MaxHeight             int
	AudioStreamIndex      *int
	BurnSubtitleStreamIdx *int
	StartPositionMS       int64
	ForceSoftware         bool
	DisableLibplacebo     bool
	DisableHWDecode       bool
	DisableTonemapVAAPI   bool
}

// EnsureSession launches an FFmpeg HLS process for sessionID if not already running,
// and waits briefly for the requested playlist/segment file to appear on disk.
// If libplacebo (Vulkan), VA-API hardware decode/tone-mapping, or hardware encoding
// fails immediately at startup, it automatically retries through safe fallbacks.
func (t *Transcoder) EnsureSession(ctx context.Context, sessionID, userID, inputPath string, opts TranscodeOptions, fileName string) (string, error) {
	if !t.Available() {
		return "", ErrFFmpegUnavailable
	}
	if !reAllowedHLSFile.MatchString(fileName) {
		return "", ErrInvalidSegment
	}
	if !filepath.IsAbs(inputPath) {
		return "", fmt.Errorf("ffmpeg requires an absolute input path")
	}

	target, hwFailed, err := t.ensureSessionOnce(ctx, sessionID, userID, inputPath, opts, fileName)
	if err != nil && opts.Mode == "transcode" && fileName == "index.m3u8" {
		if t.usesLibplacebo(opts) {
			t.logger.Warn("libplacebo Dolby Vision tone-map failed at startup; retrying with standard fallback", "session_id", sessionID, "error", err)
			t.Stop(sessionID)
			opts.DisableLibplacebo = true
			target, hwFailed, err = t.ensureSessionOnce(ctx, sessionID, userID, inputPath, opts, fileName)
		}
		if err != nil && t.usesVAAPIHWDecode(opts) {
			t.logger.Warn("VA-API hardware decode/tone-map failed at startup; retrying with CPU decode + VA-API encode", "session_id", sessionID, "error", err)
			t.Stop(sessionID)
			opts.DisableHWDecode = true
			opts.DisableTonemapVAAPI = true
			target, hwFailed, err = t.ensureSessionOnce(ctx, sessionID, userID, inputPath, opts, fileName)
		}
		if err != nil && hwFailed && !opts.ForceSoftware && t.hwAccel != HWAccelNone {
			t.logger.Warn("hardware transcode failed at startup; retrying with software libx264", "session_id", sessionID, "hw_accel", t.hwAccel, "error", err)
			t.Stop(sessionID)
			opts.ForceSoftware = true
			target, _, err = t.ensureSessionOnce(ctx, sessionID, userID, inputPath, opts, fileName)
		}
	}
	return target, err
}

func (t *Transcoder) ensureSessionOnce(ctx context.Context, sessionID, userID, inputPath string, opts TranscodeOptions, fileName string) (string, bool, error) {
	t.mu.Lock()
	sess, exists := t.sessions[sessionID]
	if !exists {
		if err := t.checkLimitsLocked(userID); err != nil {
			t.mu.Unlock()
			return "", false, err
		}
		dir := filepath.Join(t.baseDir, sessionID)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.mu.Unlock()
			return "", false, fmt.Errorf("create transcode dir: %w", err)
		}

		effectiveHW := t.effectiveHWAccel(opts)
		procCtx, cancel := context.WithCancel(context.Background())
		args := t.buildFFmpegArgs(inputPath, dir, opts)
		stderrBuf := &limitedBuffer{max: 4096}
		cmd := exec.CommandContext(procCtx, t.ffmpegBin, args...)
		cmd.Stderr = stderrBuf
		if err := cmd.Start(); err != nil {
			cancel()
			_ = os.RemoveAll(dir)
			t.mu.Unlock()
			return "", effectiveHW != HWAccelNone, fmt.Errorf("start ffmpeg: %w", err)
		}

		sess = &transcodeSession{
			id:         sessionID,
			userID:     userID,
			dir:        dir,
			hwAccel:    effectiveHW,
			cancel:     cancel,
			cmd:        cmd,
			stderr:     stderrBuf,
			done:       make(chan struct{}),
			lastAccess: time.Now(),
		}
		t.sessions[sessionID] = sess

		go func(s *transcodeSession) {
			defer close(s.done)
			if waitErr := s.cmd.Wait(); waitErr != nil && procCtx.Err() == nil {
				msg := s.stderr.String()
				if msg != "" {
					t.logger.Warn("ffmpeg hls session exited with error", "session_id", s.id, "hw_accel", s.hwAccel, "error", waitErr, "stderr", msg)
				} else {
					t.logger.Warn("ffmpeg hls session exited with error", "session_id", s.id, "hw_accel", s.hwAccel, "error", waitErr)
				}
			}
		}(sess)

		t.logger.Info("started hls transcode session",
			"session_id", sessionID,
			"user_id", userID,
			"mode", opts.Mode,
			"hw_accel", effectiveHW,
			"audio_copy", opts.AudioCopy,
			"start_ms", opts.StartPositionMS,
		)
	} else {
		sess.lastAccess = time.Now()
	}
	dir := sess.dir
	doneCh := sess.done
	sessHW := sess.hwAccel
	stderrBuf := sess.stderr
	t.mu.Unlock()

	target := filepath.Join(dir, fileName)
	deadline := time.NewTimer(25 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(target); err == nil && info.Size() > 0 {
			return target, false, nil
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-deadline.C:
			return "", false, fmt.Errorf("timed out waiting for HLS file %s", fileName)
		case <-doneCh:
			if info, err := os.Stat(target); err == nil && info.Size() > 0 {
				return target, false, nil
			}
			detail := ""
			if stderrBuf != nil {
				detail = stderrBuf.String()
			}
			if detail != "" {
				return "", sessHW != HWAccelNone, fmt.Errorf("transcode process exited before producing %s: %s", fileName, detail)
			}
			return "", sessHW != HWAccelNone, fmt.Errorf("transcode process exited before producing %s", fileName)
		case <-ticker.C:
		}
	}
}

// Stop terminates a running transcode session and removes its temporary segments.
func (t *Transcoder) Stop(sessionID string) {
	t.mu.Lock()
	sess, ok := t.sessions[sessionID]
	if ok {
		delete(t.sessions, sessionID)
	}
	t.mu.Unlock()

	if !ok {
		return
	}
	sess.cancel()
	<-sess.done
	_ = os.RemoveAll(sess.dir)
}

// StopAsync detaches and cancels a running transcode session immediately so the caller
// (e.g. a seek renegotiation) does not block on FFmpeg teardown and segment cleanup.
func (t *Transcoder) StopAsync(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	sess, ok := t.sessions[sessionID]
	if ok {
		delete(t.sessions, sessionID)
	}
	t.mu.Unlock()

	if !ok {
		return
	}
	sess.cancel()
	go func() {
		<-sess.done
		_ = os.RemoveAll(sess.dir)
	}()
}

// ResolveSeekKeyframeMS probes the exact stream start offset (in milliseconds) that FFmpeg
// will shift to 0.000s via "-avoid_negative_ts make_zero" when remuxing (-c:v copy) with
// "-fflags +genpts -noaccurate_seek -ss <targetMS>".
//
// Probing with FFmpeg itself using identical input seek flags guarantees the returned offset
// matches the actual remuxed HLS stream even on B-frame streams where ffprobe -read_intervals
// or re-passing the keyframe PTS to -ss would jump back an entire extra GOP.
// The caller MUST keep passing the original targetMS to FFmpeg's -ss and use the returned
// offset only as the client's timeline/subtitle resume_position_ms.
func (t *Transcoder) ResolveSeekKeyframeMS(ctx context.Context, inputPath string, targetMS int64) (int64, bool) {
	if t == nil || t.ffmpegBin == "" || targetMS <= 0 || !filepath.IsAbs(inputPath) {
		return targetMS, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	targetSec := float64(targetMS) / 1000.0
	out, err := exec.CommandContext(probeCtx, t.ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-fflags", "+genpts",
		"-noaccurate_seek",
		"-ss", fmt.Sprintf("%.3f", targetSec),
		"-copyts",
		"-i", inputPath,
		"-map", "0:v:0",
		"-c:v", "copy",
		"-frames:v", "1",
		"-f", "framecrc",
		"-",
	).Output()
	if err != nil {
		return targetMS, false
	}
	startMS, ok := parseFrameCRCStartMS(string(out))
	if !ok {
		return targetMS, false
	}
	// Sanity check: only accept offsets at or before targetMS (within 30s).
	if startMS >= 0 && startMS <= targetMS+500 && targetMS-startMS <= 30_000 {
		return startMS, true
	}
	return targetMS, false
}

// parseFrameCRCStartMS extracts the first video packet's DTS (falling back to PTS when DTS is
// unavailable or negative) in milliseconds from FFmpeg's "-f framecrc -" output.
// Because "-avoid_negative_ts make_zero" shifts all stream timestamps by -min_dts, using the
// first keyframe's DTS ensures client_offset + video.currentTime == exact source PTS (even
// when B-frame reordering makes first video PTS > 0 in the fMP4 segment).
func parseFrameCRCStartMS(output string) (int64, bool) {
	tbNum, tbDen := int64(1), int64(1000)
	for _, rawLine := range strings.Split(output, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "#tb 0:") {
			tbStr := strings.TrimSpace(strings.TrimPrefix(line, "#tb 0:"))
			parts := strings.Split(tbStr, "/")
			if len(parts) == 2 {
				if n, err1 := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); err1 == nil && n > 0 {
					if d, err2 := strconv.ParseInt(strings.TrimSpace(parts[1]), 10, 64); err2 == nil && d > 0 {
						tbNum, tbDen = n, d
					}
				}
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 3 || strings.TrimSpace(fields[0]) != "0" {
			continue
		}
		dtsStr := strings.TrimSpace(fields[1])
		ptsStr := strings.TrimSpace(fields[2])
		if dts, err := strconv.ParseInt(dtsStr, 10, 64); err == nil && dts >= 0 {
			ms := int64(math.Round(float64(dts) * float64(tbNum) * 1000.0 / float64(tbDen)))
			return ms, true
		}
		if pts, err := strconv.ParseInt(ptsStr, 10, 64); err == nil && pts >= 0 {
			ms := int64(math.Round(float64(pts) * float64(tbNum) * 1000.0 / float64(tbDen)))
			return ms, true
		}
	}
	return 0, false
}

// Close stops the background idle reaper and terminates all active transcode sessions cleanly.
func (t *Transcoder) Close() {
	if t == nil {
		return
	}
	t.closeOnce.Do(func() {
		if t.reaperStop != nil {
			close(t.reaperStop)
			<-t.reaperDone
		}
	})

	t.mu.Lock()
	ids := make([]string, 0, len(t.sessions))
	for id := range t.sessions {
		ids = append(ids, id)
	}
	t.mu.Unlock()

	for _, id := range ids {
		t.Stop(id)
	}
}

// Shutdown stops all active transcode sessions and background workers cleanly.
func (t *Transcoder) Shutdown() {
	t.Close()
}

// ExtractSubtitleVTT extracts an embedded text subtitle stream (ASS/SSA/SRT/WebVTT/mov_text)
// from inputPath into a cached WebVTT file under <baseDir>/subs, using per-track singleflight
// deduplication so concurrent requests for different tracks do not block each other.
func (t *Transcoder) ExtractSubtitleVTT(ctx context.Context, cacheKey, inputPath string, streamIndex int) (string, error) {
	if !t.Available() {
		return "", ErrFFmpegUnavailable
	}
	if !filepath.IsAbs(inputPath) {
		return "", fmt.Errorf("ffmpeg requires an absolute input path")
	}
	if streamIndex < 0 {
		return "", fmt.Errorf("invalid subtitle stream index %d", streamIndex)
	}
	subsDir := filepath.Join(t.baseDir, "subs")
	flightKey := fmt.Sprintf("%s_%d", cacheKey, streamIndex)
	dst := filepath.Join(subsDir, flightKey+".vtt")
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		return dst, nil
	}

	t.subMu.Lock()
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		t.subMu.Unlock()
		return dst, nil
	}
	if flight, ok := t.subFlights[flightKey]; ok {
		t.subMu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-flight.done:
			return flight.path, flight.err
		}
	}
	flight := &subFlight{done: make(chan struct{})}
	t.subFlights[flightKey] = flight
	t.subMu.Unlock()

	defer func() {
		t.subMu.Lock()
		delete(t.subFlights, flightKey)
		t.subMu.Unlock()
		close(flight.done)
	}()

	if err := os.MkdirAll(subsDir, 0o750); err != nil {
		flight.err = fmt.Errorf("create subtitle cache dir: %w", err)
		return "", flight.err
	}

	tmp := dst + ".tmp"
	extractCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(extractCtx, t.ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", inputPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn", "-an", "-dn",
		"-f", "webvtt",
		tmp,
	)
	stderrBuf := &limitedBuffer{max: 2048}
	cmd.Stderr = stderrBuf
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		msg := stderrBuf.String()
		if msg != "" {
			flight.err = fmt.Errorf("extract subtitle stream %d: %w (%s)", streamIndex, err, msg)
		} else {
			flight.err = fmt.Errorf("extract subtitle stream %d: %w", streamIndex, err)
		}
		return "", flight.err
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		flight.err = fmt.Errorf("cache subtitle stream %d: %w", streamIndex, err)
		return "", flight.err
	}
	flight.path = dst
	return dst, nil
}

// StreamSubtitleVTT serves an embedded text subtitle stream as WebVTT.
// If the full track is already cached on disk, it is served immediately.
// Otherwise, FFmpeg streams WebVTT chunks progressively to w (using fast -ss + -copyts
// when startSec > 0) so the first cues reach the player in ~0.5–1.5s even on 50GB+ MKVs.
// When startSec <= 0 and extraction completes cleanly, the output is atomically cached.
func (t *Transcoder) StreamSubtitleVTT(ctx context.Context, w http.ResponseWriter, r *http.Request, cacheKey, inputPath string, streamIndex int, startSec float64) error {
	if !t.Available() {
		return ErrFFmpegUnavailable
	}
	if !filepath.IsAbs(inputPath) {
		return fmt.Errorf("ffmpeg requires an absolute input path")
	}
	if streamIndex < 0 {
		return fmt.Errorf("invalid subtitle stream index %d", streamIndex)
	}
	subsDir := filepath.Join(t.baseDir, "subs")
	flightKey := fmt.Sprintf("%s_%d", cacheKey, streamIndex)
	dst := filepath.Join(subsDir, flightKey+".vtt")
	if info, err := os.Stat(dst); err == nil && info.Size() > 0 {
		w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
		w.Header().Set("Cache-Control", "private, max-age=3600")
		http.ServeFile(w, r, dst)
		return nil
	}

	args := []string{"-hide_banner", "-loglevel", "error"}
	if startSec > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.3f", startSec), "-copyts")
	}
	args = append(args,
		"-i", inputPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn", "-an", "-dn",
		"-f", "webvtt",
		"pipe:1",
	)

	cmd := exec.CommandContext(ctx, t.ffmpegBin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("subtitle stdout pipe: %w", err)
	}
	stderrBuf := &limitedBuffer{max: 2048}
	cmd.Stderr = stderrBuf

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start subtitle extraction: %w", err)
	}

	var cacheFile *os.File
	var cacheTmp string
	if startSec <= 0 {
		if mkErr := os.MkdirAll(subsDir, 0o750); mkErr == nil {
			cacheTmp = fmt.Sprintf("%s.%d.tmp", dst, time.Now().UnixNano())
			if f, fErr := os.Create(cacheTmp); fErr == nil {
				cacheFile = f
			} else {
				cacheTmp = ""
			}
		}
	}

	flusher, _ := w.(http.Flusher)
	headerSent := false
	buf := make([]byte, 4096)
	var writeErr error
	for {
		n, readErr := stdout.Read(buf)
		if n > 0 {
			if !headerSent {
				w.Header().Set("Content-Type", "text/vtt; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				w.Header().Set("X-Accel-Buffering", "no")
				w.WriteHeader(http.StatusOK)
				headerSent = true
			}
			if cacheFile != nil {
				if _, cfErr := cacheFile.Write(buf[:n]); cfErr != nil {
					_ = cacheFile.Close()
					_ = os.Remove(cacheTmp)
					cacheFile = nil
					cacheTmp = ""
				}
			}
			if _, wErr := w.Write(buf[:n]); wErr != nil {
				writeErr = wErr
				break
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) && writeErr == nil {
				writeErr = readErr
			}
			break
		}
	}

	waitErr := cmd.Wait()
	if cacheFile != nil {
		closeErr := cacheFile.Close()
		if waitErr == nil && writeErr == nil && closeErr == nil && ctx.Err() == nil {
			if info, sErr := os.Stat(cacheTmp); sErr == nil && info.Size() > 0 {
				if rErr := os.Rename(cacheTmp, dst); rErr != nil {
					_ = os.Remove(cacheTmp)
				}
			} else {
				_ = os.Remove(cacheTmp)
			}
		} else {
			_ = os.Remove(cacheTmp)
		}
	}

	if writeErr != nil {
		return nil
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return nil
		}
		msg := stderrBuf.String()
		t.logger.Warn("embedded subtitle streaming failed",
			"stream_index", streamIndex,
			"start_sec", startSec,
			"error", waitErr,
			"stderr", msg,
		)
		if !headerSent {
			return fmt.Errorf("stream subtitle stream %d: %w", streamIndex, waitErr)
		}
	}
	return nil
}

func (t *Transcoder) effectiveHWAccel(opts TranscodeOptions) HWAccel {
	if opts.Mode != "transcode" || opts.ForceSoftware {
		return HWAccelNone
	}
	return t.hwAccel
}

func (t *Transcoder) usesLibplacebo(opts TranscodeOptions) bool {
	if opts.Mode != "transcode" || !t.hasLibplacebo || opts.DisableLibplacebo {
		return false
	}
	hdr := strings.ToLower(strings.TrimSpace(opts.VideoHDR))
	return hdr == "dovi" || hdr == "dovi_hdr10"
}

func isHDRSource(hdr string) bool {
	switch strings.ToLower(strings.TrimSpace(hdr)) {
	case "hdr10", "hdr10plus", "hlg", "hdr", "dovi", "dovi_hdr10":
		return true
	default:
		return false
	}
}

func isVAAPIHWDecodable(codec, profile, pixFmt string) bool {
	switch strings.ToLower(strings.TrimSpace(codec)) {
	case "h264", "avc":
		// Intel/AMD VA-API decoders do not support 10-bit H.264 (Hi10P).
		return !is10BitVideo(profile, pixFmt)
	case "hevc", "h265", "vp9", "av1", "mpeg2video", "vc1":
		return true
	default:
		return false
	}
}

func (t *Transcoder) usesVAAPIHWDecode(opts TranscodeOptions) bool {
	if t.effectiveHWAccel(opts) != HWAccelVAAPI || opts.DisableHWDecode {
		return false
	}
	if opts.BurnSubtitleStreamIdx != nil && *opts.BurnSubtitleStreamIdx >= 0 {
		return false
	}
	if t.usesLibplacebo(opts) {
		return false
	}
	if !isVAAPIHWDecodable(opts.VideoCodec, opts.VideoProfile, opts.VideoPixFmt) {
		return false
	}
	if isHDRSource(opts.VideoHDR) {
		// Dolby Vision Profile 5 uses proprietary IPTPQc2 and cannot be tone-mapped by tonemap_vaapi.
		if opts.DVProfile == 5 {
			return false
		}
		// Only keep frames in VA-API GPU memory for HDR if tonemap_vaapi is available.
		return t.hasTonemapVAAPI && !opts.DisableTonemapVAAPI
	}
	return true
}

func needsDownscale(opts TranscodeOptions) bool {
	return opts.MaxHeight > 0 && (opts.SourceHeight == 0 || opts.SourceHeight > opts.MaxHeight)
}

func (t *Transcoder) resolvedVAAPIDevice() string {
	if t != nil && t.vaapiDevice != "" {
		return t.vaapiDevice
	}
	return "/dev/dri/renderD128"
}

func (t *Transcoder) buildFFmpegArgs(inputPath, outDir string, opts TranscodeOptions) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y", "-fflags", "+genpts"}
	effectiveHW := t.effectiveHWAccel(opts)
	useVAAPIHWDec := t.usesVAAPIHWDecode(opts)

	if opts.Mode == "transcode" {
		switch effectiveHW {
		case HWAccelVAAPI:
			dev := t.resolvedVAAPIDevice()
			if useVAAPIHWDec {
				args = append(args, "-hwaccel", "vaapi", "-hwaccel_device", dev, "-hwaccel_output_format", "vaapi")
			} else {
				args = append(args, "-vaapi_device", dev)
			}
		case HWAccelQSV:
			args = append(args, "-qsv_device", t.resolvedVAAPIDevice())
		}
	}
	if opts.StartPositionMS > 0 {
		if opts.Mode == "remux" {
			// Prevent FFmpeg's default accurate_seek from discarding transcoded audio packets
			// between the preceding video keyframe and -ss while video (-c:v copy) starts at the keyframe.
			args = append(args, "-noaccurate_seek")
		}
		args = append(args, "-ss", fmt.Sprintf("%.3f", float64(opts.StartPositionMS)/1000.0))
	}

	audioMap := "0:a:0?"
	if opts.AudioStreamIndex != nil && *opts.AudioStreamIndex >= 0 {
		audioMap = fmt.Sprintf("0:%d?", *opts.AudioStreamIndex)
	}
	args = append(args, "-i", inputPath)

	if opts.Mode == "remux" {
		args = append(args, "-map", "0:v:0?", "-map", audioMap, "-sn", "-dn")
		args = append(args, "-c:v", "copy")
		if strings.EqualFold(opts.VideoCodec, "hevc") || strings.EqualFold(opts.VideoCodec, "h265") {
			args = append(args, "-tag:v", "hvc1")
		}
	} else {
		hasBurnIn := opts.BurnSubtitleStreamIdx != nil && *opts.BurnSubtitleStreamIdx >= 0
		downscale := needsDownscale(opts)

		if useVAAPIHWDec {
			// Full GPU decode -> GPU filter/tone-map/scale -> VA-API encode pipeline (zero CPU frame copies).
			var gpuFilters []string
			if isHDRSource(opts.VideoHDR) {
				gpuFilters = append(gpuFilters, "tonemap_vaapi=format=nv12:matrix=bt709:primaries=bt709:transfer=bt709")
				if downscale {
					gpuFilters = append(gpuFilters, fmt.Sprintf("scale_vaapi=w=-2:h=%d", opts.MaxHeight))
				}
			} else if downscale {
				gpuFilters = append(gpuFilters, fmt.Sprintf("scale_vaapi=w=-2:h=%d:format=nv12", opts.MaxHeight))
			} else {
				gpuFilters = append(gpuFilters, "scale_vaapi=format=nv12")
			}
			args = append(args, "-map", "0:v:0?", "-map", audioMap, "-sn", "-dn", "-vf", strings.Join(gpuFilters, ","))
		} else {
			// CPU decode / filter pipeline (supports libplacebo DV, CPU zscale+tonemap, bitmap subtitle burn-in,
			// 10-bit H.264, and SW-decode + HW-encode fallback).
			var preFilters []string
			if t.usesLibplacebo(opts) {
				preFilters = append(preFilters,
					"libplacebo=tonemapping=bt.2390:colorspace=bt709:color_primaries=bt709:color_trc=bt709:range=tv:apply_dolbyvision=true",
				)
			} else if isHDRSource(opts.VideoHDR) && t.hasZscaleTonemap {
				preFilters = append(preFilters,
					"zscale=t=linear:npl=100",
					"format=gbrpf32le",
					"zscale=p=bt709",
					"tonemap=tonemap=hable:desat=0",
					"zscale=t=bt709:m=bt709:r=tv",
				)
			} else if isHDRSource(opts.VideoHDR) {
				// Fallback when zscale/tonemap/libplacebo are unavailable: convert BT.2020 matrix to BT.709 SDR
				// and overwrite frame-level color metadata so encoders (e.g. VideoToolbox) emit valid BT.709 SDR VUI.
				preFilters = append(preFilters,
					"scale=in_color_matrix=bt2020:out_color_matrix=bt709",
					"setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv",
				)
			}
			if downscale && !hasBurnIn && effectiveHW != HWAccelVAAPI {
				preFilters = append(preFilters, fmt.Sprintf("scale=trunc(oh*dar/2)*2:min(ih\\,%d),setsar=1", opts.MaxHeight))
			}

			var postFilters []string
			if hasBurnIn && downscale {
				postFilters = append(postFilters, fmt.Sprintf("scale=trunc(oh*dar/2)*2:min(ih\\,%d),setsar=1", opts.MaxHeight))
			}
			switch effectiveHW {
			case HWAccelVAAPI:
				if downscale && !hasBurnIn && len(preFilters) > 0 {
					postFilters = append(postFilters, fmt.Sprintf("scale=trunc(oh*dar/2)*2:min(ih\\,%d),setsar=1", opts.MaxHeight))
				}
				postFilters = append(postFilters, "format=nv12", "hwupload")
				if downscale && !hasBurnIn && len(preFilters) == 0 {
					postFilters = append(postFilters, fmt.Sprintf("scale_vaapi=w=-2:h=%d", opts.MaxHeight))
				}
			case HWAccelQSV:
				postFilters = append(postFilters, "format=nv12")
			default:
				postFilters = append(postFilters, "format=yuv420p")
			}

			if hasBurnIn {
				// Scale bitmap subtitle (PGS / VobSub / DVB) to match video dimensions via scale2ref
				// before overlaying so 1080p PGS on 4K video is never tiny or cropped.
				subRef := fmt.Sprintf("[0:%d]", *opts.BurnSubtitleStreamIdx)
				var fc strings.Builder
				vLabel := "[0:v:0]"
				if len(preFilters) > 0 {
					fc.WriteString(vLabel)
					fc.WriteString(strings.Join(preFilters, ","))
					fc.WriteString("[vpre];")
					vLabel = "[vpre]"
				}
				fc.WriteString(subRef)
				fc.WriteString(vLabel)
				fc.WriteString("scale2ref[sub_scaled][vbase];[vbase][sub_scaled]overlay=eof_action=pass")
				if len(postFilters) > 0 {
					fc.WriteString(",")
					fc.WriteString(strings.Join(postFilters, ","))
				}
				fc.WriteString("[vout]")
				args = append(args, "-filter_complex", fc.String(), "-map", "[vout]", "-map", audioMap, "-dn")
			} else {
				args = append(args, "-map", "0:v:0?", "-map", audioMap, "-sn", "-dn")
				allFilters := append(preFilters, postFilters...)
				if len(allFilters) > 0 {
					args = append(args, "-vf", strings.Join(allFilters, ","))
				}
			}
		}

		switch effectiveHW {
		case HWAccelVAAPI:
			args = append(args, "-c:v", "h264_vaapi")
			if opts.MaxBitrate > 0 {
				kbps := strconv.FormatInt(opts.MaxBitrate/1000, 10) + "k"
				args = append(args, "-b:v", kbps, "-maxrate", kbps, "-bufsize", strconv.FormatInt((opts.MaxBitrate*2)/1000, 10)+"k")
			} else {
				args = append(args, "-qp", "21")
			}
		case HWAccelVideoToolbox:
			args = append(args, "-c:v", "h264_videotoolbox", "-profile:v", "high", "-allow_sw", "1")
			if opts.MaxBitrate > 0 {
				kbps := strconv.FormatInt(opts.MaxBitrate/1000, 10) + "k"
				args = append(args, "-b:v", kbps, "-maxrate", kbps, "-bufsize", strconv.FormatInt((opts.MaxBitrate*2)/1000, 10)+"k")
			} else {
				args = append(args, "-q:v", "65")
			}
		case HWAccelNVENC:
			args = append(args, "-c:v", "h264_nvenc", "-preset", "p4", "-profile:v", "high", "-rc", "vbr", "-cq", "21")
			if opts.MaxBitrate > 0 {
				kbps := strconv.FormatInt(opts.MaxBitrate/1000, 10) + "k"
				args = append(args, "-maxrate", kbps, "-bufsize", strconv.FormatInt((opts.MaxBitrate*2)/1000, 10)+"k")
			}
		case HWAccelQSV:
			args = append(args, "-c:v", "h264_qsv", "-preset", "veryfast", "-global_quality", "22")
			if opts.MaxBitrate > 0 {
				kbps := strconv.FormatInt(opts.MaxBitrate/1000, 10) + "k"
				args = append(args, "-maxrate", kbps, "-bufsize", strconv.FormatInt((opts.MaxBitrate*2)/1000, 10)+"k")
			}
		default:
			args = append(args, "-c:v", "libx264", "-preset", "veryfast", "-profile:v", "high", "-crf", "20", "-sc_threshold", "0")
			if opts.MaxBitrate > 0 {
				kbps := strconv.FormatInt(opts.MaxBitrate/1000, 10) + "k"
				args = append(args, "-maxrate", kbps, "-bufsize", strconv.FormatInt((opts.MaxBitrate*2)/1000, 10)+"k")
			}
		}

		// Explicitly tag SDR BT.709 color metadata so HDR/DoVi sources never leak PQ/BT.2020 tags into 8-bit SDR H.264.
		args = append(args,
			"-color_primaries", "bt709",
			"-color_trc", "bt709",
			"-colorspace", "bt709",
			"-color_range", "tv",
		)

		// Align keyframes cleanly with HLS segment boundaries for fast startup and crisp seeking.
		args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", hlsSegmentSeconds))
	}

	if opts.AudioCopy {
		args = append(args, "-c:a", "copy")
	} else {
		audioChannels := 2
		audioBitrate := "256k"
		if opts.AudioChannels > 2 && opts.MaxAudioChannels >= 6 {
			audioChannels = 6
			audioBitrate = "384k"
		} else if opts.MaxBitrate > 0 && opts.MaxBitrate <= 4_000_000 {
			audioBitrate = "192k"
		}
		audioFilter := "aresample=async=1:first_pts=0"
		if opts.Mode == "remux" {
			// In remux (-c:v copy) mode, omit first_pts=0 so -avoid_negative_ts make_zero shifts
			// copied video and transcoded audio by the exact same offset without introducing A/V skew.
			audioFilter = "aresample=async=1"
		}
		args = append(args, "-c:a", "aac", "-ac", strconv.Itoa(audioChannels), "-b:a", audioBitrate, "-af", audioFilter)
	}

	segPattern := filepath.Join(outDir, "seg_%05d.m4s")
	playlistPath := filepath.Join(outDir, "index.m3u8")

	args = append(args,
		"-max_muxing_queue_size", "2048",
		"-avoid_negative_ts", "make_zero",
		"-f", "hls",
		"-hls_time", strconv.Itoa(hlsSegmentSeconds),
		"-hls_list_size", "0",
		"-hls_playlist_type", "event",
		"-hls_segment_type", "fmp4",
		"-hls_fmp4_init_filename", "init.mp4",
		"-hls_flags", "independent_segments+temp_file",
		"-hls_segment_filename", segPattern,
		playlistPath,
	)
	return args
}

type ffmpegCapabilities struct {
	hwAccel          HWAccel
	vaapiDevice      string
	hasZscaleTonemap bool
	hasTonemapVAAPI  bool
	hasScaleVAAPI    bool
	hasLibplacebo    bool
	probeNotes       []string
}

func discoverDRIRenderNodes() []string {
	if explicit := strings.TrimSpace(os.Getenv("LUNE_VAAPI_DEVICE")); explicit != "" {
		return []string{explicit}
	}
	matches, _ := filepath.Glob("/dev/dri/renderD*")
	sort.Strings(matches)
	return matches
}

func checkDRIAccess(dev string) error {
	f, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return f.Close()
}

func compactFFmpegErr(out []byte, err error) string {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line != "" {
			if len(line) > 160 {
				return line[:160] + "..."
			}
			return line
		}
	}
	if err != nil {
		return err.Error()
	}
	return "unknown error"
}

func testFFmpegEncode(ffmpegBin string, args ...string) (bool, string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, ffmpegBin, args...).CombinedOutput()
	if err != nil {
		return false, compactFFmpegErr(out, err)
	}
	return true, ""
}

func probeVAAPI(ffmpegBin, encoders string, nodes []string) (string, []string) {
	var notes []string
	if !strings.Contains(encoders, "h264_vaapi") {
		return "", []string{"vaapi: ffmpeg build missing h264_vaapi encoder"}
	}
	if len(nodes) == 0 {
		return "", []string{"vaapi: no /dev/dri/renderD* devices found"}
	}
	for _, dev := range nodes {
		if err := checkDRIAccess(dev); err != nil {
			notes = append(notes, fmt.Sprintf("vaapi (%s): %v", dev, err))
			continue
		}
		ok, detail := testFFmpegEncode(ffmpegBin,
			"-hide_banner", "-loglevel", "error",
			"-vaapi_device", dev,
			"-f", "lavfi", "-i", "color=c=black:s=256x144:r=24:d=0.1",
			"-frames:v", "2",
			"-vf", "format=nv12,hwupload",
			"-c:v", "h264_vaapi",
			"-f", "null", "-",
		)
		if ok {
			notes = append(notes, fmt.Sprintf("vaapi (%s): verified h264_vaapi", dev))
			return dev, notes
		}
		notes = append(notes, fmt.Sprintf("vaapi (%s): test encode failed (%s)", dev, detail))
	}
	return "", notes
}

func probeQSV(ffmpegBin, encoders string, nodes []string) (string, []string) {
	var notes []string
	if !strings.Contains(encoders, "h264_qsv") {
		return "", []string{"qsv: ffmpeg build missing h264_qsv encoder"}
	}
	if len(nodes) == 0 {
		return "", []string{"qsv: no /dev/dri/renderD* devices found"}
	}
	for _, dev := range nodes {
		if err := checkDRIAccess(dev); err != nil {
			notes = append(notes, fmt.Sprintf("qsv (%s): %v", dev, err))
			continue
		}
		ok, detail := testFFmpegEncode(ffmpegBin,
			"-hide_banner", "-loglevel", "error",
			"-qsv_device", dev,
			"-f", "lavfi", "-i", "color=c=black:s=256x144:r=24:d=0.1",
			"-frames:v", "2",
			"-vf", "format=nv12",
			"-c:v", "h264_qsv",
			"-f", "null", "-",
		)
		if ok {
			notes = append(notes, fmt.Sprintf("qsv (%s): verified h264_qsv", dev))
			return dev, notes
		}
		notes = append(notes, fmt.Sprintf("qsv (%s): test encode failed (%s)", dev, detail))
	}
	return "", notes
}

func probeNVENC(ffmpegBin, encoders string) (bool, string) {
	if !strings.Contains(encoders, "h264_nvenc") {
		return false, "nvenc: ffmpeg build missing h264_nvenc encoder"
	}
	if _, err := os.Stat("/dev/nvidiactl"); err != nil {
		return false, "nvenc: /dev/nvidiactl not present"
	}
	ok, detail := testFFmpegEncode(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x144:r=24:d=0.1",
		"-frames:v", "2",
		"-vf", "format=yuv420p",
		"-c:v", "h264_nvenc",
		"-f", "null", "-",
	)
	if ok {
		return true, "nvenc: verified h264_nvenc"
	}
	return false, fmt.Sprintf("nvenc: test encode failed (%s)", detail)
}

func probeVideoToolbox(ffmpegBin, encoders string) (bool, string) {
	if runtime.GOOS != "darwin" || !strings.Contains(encoders, "h264_videotoolbox") {
		return false, "videotoolbox: unavailable"
	}
	ok, detail := testFFmpegEncode(ffmpegBin,
		"-hide_banner", "-loglevel", "error",
		"-f", "lavfi", "-i", "color=c=black:s=256x144:r=24:d=0.1",
		"-frames:v", "2",
		"-vf", "format=yuv420p",
		"-c:v", "h264_videotoolbox",
		"-allow_sw", "1",
		"-f", "null", "-",
	)
	if ok {
		return true, "videotoolbox: verified h264_videotoolbox"
	}
	return false, fmt.Sprintf("videotoolbox: test encode failed (%s)", detail)
}

func probeFFmpegCapabilities(ffmpegBin string) ffmpegCapabilities {
	caps := ffmpegCapabilities{hwAccel: HWAccelNone}
	if ffmpegBin == "" {
		caps.probeNotes = []string{"ffmpeg binary not found in PATH"}
		return caps
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	encOut, _ := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders").Output()
	encoders := string(encOut)

	fltOut, _ := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-filters").Output()
	filters := string(fltOut)
	caps.hasZscaleTonemap = strings.Contains(filters, " zscale ") && strings.Contains(filters, " tonemap ")
	caps.hasTonemapVAAPI = strings.Contains(filters, " tonemap_vaapi ")
	caps.hasScaleVAAPI = strings.Contains(filters, " scale_vaapi ")
	caps.hasLibplacebo = strings.Contains(filters, " libplacebo ")

	pref := strings.ToLower(strings.TrimSpace(os.Getenv("LUNE_HW_ACCEL")))
	switch pref {
	case "software", "none", "off", "false":
		caps.probeNotes = []string{"hardware acceleration disabled via LUNE_HW_ACCEL"}
		return caps
	}

	nodes := discoverDRIRenderNodes()

	switch pref {
	case "videotoolbox":
		ok, note := probeVideoToolbox(ffmpegBin, encoders)
		caps.probeNotes = append(caps.probeNotes, note)
		if ok {
			caps.hwAccel = HWAccelVideoToolbox
		}
		return caps
	case "nvenc":
		ok, note := probeNVENC(ffmpegBin, encoders)
		caps.probeNotes = append(caps.probeNotes, note)
		if ok {
			caps.hwAccel = HWAccelNVENC
		}
		return caps
	case "vaapi":
		dev, notes := probeVAAPI(ffmpegBin, encoders, nodes)
		caps.probeNotes = append(caps.probeNotes, notes...)
		if dev != "" {
			caps.hwAccel = HWAccelVAAPI
			caps.vaapiDevice = dev
		}
		return caps
	case "qsv":
		dev, notes := probeQSV(ffmpegBin, encoders, nodes)
		caps.probeNotes = append(caps.probeNotes, notes...)
		if dev != "" {
			caps.hwAccel = HWAccelQSV
			caps.vaapiDevice = dev
		}
		return caps
	}

	// Auto-detection order.
	if runtime.GOOS == "darwin" {
		if ok, note := probeVideoToolbox(ffmpegBin, encoders); ok {
			caps.hwAccel = HWAccelVideoToolbox
			caps.probeNotes = append(caps.probeNotes, note)
			return caps
		} else {
			caps.probeNotes = append(caps.probeNotes, note)
		}
		return caps
	}

	if _, err := os.Stat("/dev/nvidiactl"); err == nil {
		ok, note := probeNVENC(ffmpegBin, encoders)
		caps.probeNotes = append(caps.probeNotes, note)
		if ok {
			caps.hwAccel = HWAccelNVENC
			return caps
		}
	}

	if len(nodes) > 0 {
		if dev, notes := probeVAAPI(ffmpegBin, encoders, nodes); dev != "" {
			caps.hwAccel = HWAccelVAAPI
			caps.vaapiDevice = dev
			caps.probeNotes = append(caps.probeNotes, notes...)
			return caps
		} else {
			caps.probeNotes = append(caps.probeNotes, notes...)
		}

		if dev, notes := probeQSV(ffmpegBin, encoders, nodes); dev != "" {
			caps.hwAccel = HWAccelQSV
			caps.vaapiDevice = dev
			caps.probeNotes = append(caps.probeNotes, notes...)
			return caps
		} else {
			caps.probeNotes = append(caps.probeNotes, notes...)
		}
	} else {
		caps.probeNotes = append(caps.probeNotes, "no GPU devices detected (/dev/dri, /dev/nvidiactl); using software libx264")
	}

	return caps
}
