package stream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
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

// StreamingConfig controls live transcoder concurrency caps and idle cleanup.
type StreamingConfig struct {
	MaxGlobalTranscodes int `json:"max_global_transcodes"` // 0 = unlimited
	MaxUserTranscodes   int `json:"max_user_transcodes"`   // 0 = unlimited
	IdleTimeoutMinutes  int `json:"idle_timeout_minutes"`  // 1..120 (default 10)
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

// Transcoder orchestrates on-demand FFmpeg HLS remux/transcode sessions.
type Transcoder struct {
	ffmpegBin        string
	baseDir          string
	hwAccel          HWAccel
	hasZscaleTonemap bool
	hasLibplacebo    bool
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

	hw, hasTonemap, hasLibplacebo := probeFFmpegCapabilities(bin)
	if bin != "" {
		logger.Info("transcoder initialized", "hw_accel", hw, "hdr_tonemap", hasTonemap, "libplacebo", hasLibplacebo)
	}

	t := &Transcoder{
		ffmpegBin:        bin,
		baseDir:          baseDir,
		hwAccel:          hw,
		hasZscaleTonemap: hasTonemap,
		hasLibplacebo:    hasLibplacebo,
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
	defer t.mu.Unlock()
	idleMin := int(t.idleTTL / time.Minute)
	if idleMin <= 0 {
		idleMin = defaultIdleMin
	}
	return StreamingConfig{
		MaxGlobalTranscodes: t.maxGlobal,
		MaxUserTranscodes:   t.maxUser,
		IdleTimeoutMinutes:  idleMin,
	}
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
	VideoHDR              string
	AudioCopy             bool
	MaxBitrate            int64
	MaxHeight             int
	AudioStreamIndex      *int
	BurnSubtitleStreamIdx *int
	StartPositionMS       int64
	ForceSoftware         bool
	DisableLibplacebo     bool
}

// EnsureSession launches an FFmpeg HLS process for sessionID if not already running,
// and waits briefly for the requested playlist/segment file to appear on disk.
// If libplacebo (Vulkan) or a hardware-accelerated transcode fails immediately at startup,
// it automatically retries with safe software fallbacks.
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
	ticker := time.NewTicker(80 * time.Millisecond)
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
	extractCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()

	cmd := exec.CommandContext(extractCtx, t.ffmpegBin,
		"-hide_banner", "-loglevel", "error", "-y",
		"-i", inputPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-vn", "-an", "-dn",
		"-f", "webvtt",
		tmp,
	)
	if err := cmd.Run(); err != nil {
		_ = os.Remove(tmp)
		flight.err = fmt.Errorf("extract subtitle stream %d: %w", streamIndex, err)
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

func (t *Transcoder) effectiveHWAccel(opts TranscodeOptions) HWAccel {
	if opts.Mode != "transcode" || opts.ForceSoftware {
		return HWAccelNone
	}
	return t.hwAccel
}

func (t *Transcoder) usesLibplacebo(opts TranscodeOptions) bool {
	return opts.Mode == "transcode" &&
		t.hasLibplacebo &&
		!opts.DisableLibplacebo &&
		strings.EqualFold(strings.TrimSpace(opts.VideoHDR), "dovi")
}

func isHDRSource(hdr string) bool {
	switch strings.ToLower(strings.TrimSpace(hdr)) {
	case "hdr10", "hlg", "hdr", "dovi":
		return true
	default:
		return false
	}
}

func (t *Transcoder) buildFFmpegArgs(inputPath, outDir string, opts TranscodeOptions) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-y"}
	effectiveHW := t.effectiveHWAccel(opts)

	if opts.Mode == "transcode" && effectiveHW == HWAccelVAAPI {
		args = append(args, "-vaapi_device", "/dev/dri/renderD128")
	}
	if opts.StartPositionMS > 0 {
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
		if strings.EqualFold(opts.VideoCodec, "hevc") {
			args = append(args, "-tag:v", "hvc1")
		}
	} else {
		// Build video filter pipeline (optional Dolby Vision libplacebo or HDR->SDR tone-map, scaling, optional bitmap subtitle overlay, pixel format).
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
		}
		if opts.MaxHeight > 0 && effectiveHW != HWAccelVAAPI {
			preFilters = append(preFilters, fmt.Sprintf("scale=-2:min(ih\\,%d)", opts.MaxHeight))
		}

		hasBurnIn := opts.BurnSubtitleStreamIdx != nil && *opts.BurnSubtitleStreamIdx >= 0
		var postFilters []string
		switch effectiveHW {
		case HWAccelVAAPI:
			if opts.MaxHeight > 0 && (len(preFilters) > 0 || hasBurnIn) {
				postFilters = append(postFilters, fmt.Sprintf("scale=-2:min(ih\\,%d)", opts.MaxHeight))
			}
			postFilters = append(postFilters, "format=nv12", "hwupload")
			if opts.MaxHeight > 0 && len(preFilters) == 0 && !hasBurnIn {
				postFilters = append(postFilters, fmt.Sprintf("scale_vaapi=-2:min(ih\\,%d)", opts.MaxHeight))
			}
		default:
			postFilters = append(postFilters, "format=yuv420p")
		}

		if hasBurnIn {
			// Overlay bitmap subtitle (PGS / VobSub / DVB) onto video stream before final format/upload.
			subRef := fmt.Sprintf("[0:%d]", *opts.BurnSubtitleStreamIdx)
			var fc strings.Builder
			vLabel := "[0:v:0]"
			if len(preFilters) > 0 {
				fc.WriteString(vLabel)
				fc.WriteString(strings.Join(preFilters, ","))
				fc.WriteString("[vpre];")
				vLabel = "[vpre]"
			}
			fc.WriteString(vLabel)
			fc.WriteString(subRef)
			fc.WriteString("overlay=eof_action=pass")
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

		// Align keyframes cleanly with HLS segment boundaries for fast startup and crisp seeking.
		args = append(args, "-force_key_frames", fmt.Sprintf("expr:gte(t,n_forced*%d)", hlsSegmentSeconds))
	}

	if opts.AudioCopy {
		args = append(args, "-c:a", "copy")
	} else {
		audioBitrate := "256k"
		if opts.MaxBitrate > 0 && opts.MaxBitrate <= 4_000_000 {
			audioBitrate = "192k"
		}
		args = append(args, "-c:a", "aac", "-ac", "2", "-b:a", audioBitrate, "-af", "aresample=async=1:first_pts=0")
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

func probeFFmpegCapabilities(ffmpegBin string) (HWAccel, bool, bool) {
	if ffmpegBin == "" {
		return HWAccelNone, false, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	encOut, _ := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-encoders").Output()
	encoders := string(encOut)

	fltOut, _ := exec.CommandContext(ctx, ffmpegBin, "-hide_banner", "-filters").Output()
	filters := string(fltOut)
	hasTonemap := strings.Contains(filters, " zscale ") && strings.Contains(filters, " tonemap ")
	hasLibplacebo := strings.Contains(filters, " libplacebo ")

	if runtime.GOOS == "darwin" {
		if strings.Contains(encoders, "h264_videotoolbox") {
			return HWAccelVideoToolbox, hasTonemap, hasLibplacebo
		}
		return HWAccelNone, hasTonemap, hasLibplacebo
	}

	if strings.Contains(encoders, "h264_nvenc") {
		if _, err := os.Stat("/dev/nvidiactl"); err == nil {
			return HWAccelNVENC, hasTonemap, hasLibplacebo
		}
	}
	if strings.Contains(encoders, "h264_vaapi") {
		if _, err := os.Stat("/dev/dri/renderD128"); err == nil {
			return HWAccelVAAPI, hasTonemap, hasLibplacebo
		}
	}
	if strings.Contains(encoders, "h264_qsv") {
		if _, err := os.Stat("/dev/dri/renderD128"); err == nil {
			return HWAccelQSV, hasTonemap, hasLibplacebo
		}
	}
	return HWAccelNone, hasTonemap, hasLibplacebo
}
