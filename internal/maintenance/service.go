package maintenance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LuneServer/lune/internal/store"
)

const (
	ConfigSettingKey        = "maintenance_config"
	LastRunSettingKey       = "maintenance_last_run"
	LastScheduledSettingKey = "maintenance_last_scheduled_at"
)

var (
	ErrAlreadyRunning = errors.New("maintenance run already in progress")
	ErrInvalidConfig  = errors.New("invalid maintenance configuration")
	ErrInvalidTask    = errors.New("invalid maintenance task")
)

const (
	TaskArtwork           = "artwork"
	TaskTranscodeCache    = "transcode_cache"
	TaskAudit             = "audit"
	TaskChapterThumbnails = "chapter_thumbnails"
	TaskTrickplay         = "trickplay"
	TaskOptimizeDB        = "optimize_db"

	// taskNewMedia is internal (not accepted by RunTask): it runs the enabled per-item
	// generators (chapter stills, trickplay) after a scan adds or changes media files.
	taskNewMedia = "new_media"
)

// Run triggers recorded in RunReport.Trigger.
const (
	TriggerScheduled = "scheduled"
	TriggerManual    = "manual"
	TriggerResume    = "resume"
	TriggerNewMedia  = "new_media"
)

// isBackgroundTrigger reports runs started by the server itself outside the schedule.
// They must not move a rolling schedule, or frequent additions would postpone it forever.
func isBackgroundTrigger(trigger string) bool {
	return trigger == TriggerResume || trigger == TriggerNewMedia
}

func isValidTask(task string) bool {
	switch task {
	case "", TaskArtwork, TaskTranscodeCache, TaskAudit, TaskChapterThumbnails, TaskTrickplay, TaskOptimizeDB:
		return true
	default:
		return false
	}
}

// Config controls periodic background housekeeping tasks and retention windows.
type Config struct {
	Enabled                   bool   `json:"enabled"`
	IntervalHours             int    `json:"interval_hours"`              // 1..720 (default 24)
	ScheduleTime              string `json:"schedule_time"`               // Optional 24h local time "HH:MM" (default "03:00"; "" = rolling)
	CleanupOrphanedArtwork    bool   `json:"cleanup_orphaned_artwork"`    // default true
	ArtworkGraceHours         int    `json:"artwork_grace_hours"`         // 1..168 (default 24)
	CleanupTranscodeCache     bool   `json:"cleanup_transcode_cache"`     // default true
	SubtitleCacheDays         int    `json:"subtitle_cache_days"`         // 1..365 (default 30)
	PruneAuditLogs            bool   `json:"prune_audit_logs"`            // default true
	AuditRetentionDays        int    `json:"audit_retention_days"`        // 7..3650 (default 90)
	OptimizeDatabase          bool   `json:"optimize_database"`           // default true
	GenerateChapterThumbnails bool   `json:"generate_chapter_thumbnails"` // default true
	// Trickplay scrubbing previews (keyframe-only sprite sheets). Optional in requests so older
	// clients keep working; zero interval/width fall back to defaults.
	GenerateTrickplay        bool `json:"generate_trickplay" required:"false"`         // default true (omitted = false)
	TrickplayIntervalSeconds int  `json:"trickplay_interval_seconds" required:"false"` // 2..60 (default 10)
	TrickplayWidth           int  `json:"trickplay_width" required:"false"`            // 160..480 (default 320)
}

const (
	DefaultTrickplayIntervalSeconds = 10
	DefaultTrickplayWidth           = 320
)

// normalize fills defaults for optional fields omitted by older clients or persisted configs.
func (c Config) normalize() Config {
	if c.TrickplayIntervalSeconds == 0 {
		c.TrickplayIntervalSeconds = DefaultTrickplayIntervalSeconds
	}
	if c.TrickplayWidth == 0 {
		c.TrickplayWidth = DefaultTrickplayWidth
	}
	return c
}

// DefaultConfig returns safe, conservative defaults for daily housekeeping at 03:00 local time.
func DefaultConfig() Config {
	return Config{
		Enabled:                   true,
		IntervalHours:             24,
		ScheduleTime:              "03:00",
		CleanupOrphanedArtwork:    true,
		ArtworkGraceHours:         24,
		CleanupTranscodeCache:     true,
		SubtitleCacheDays:         30,
		PruneAuditLogs:            true,
		AuditRetentionDays:        90,
		OptimizeDatabase:          true,
		GenerateChapterThumbnails: true,
		GenerateTrickplay:         true,
		TrickplayIntervalSeconds:  DefaultTrickplayIntervalSeconds,
		TrickplayWidth:            DefaultTrickplayWidth,
	}
}

func parseScheduleTime(raw string) (hour, min int, ok bool) {
	s := strings.TrimSpace(raw)
	if len(s) != 5 || s[2] != ':' {
		return 0, 0, false
	}
	h1, h2 := s[0], s[1]
	m1, m2 := s[3], s[4]
	if h1 < '0' || h1 > '9' || h2 < '0' || h2 > '9' || m1 < '0' || m1 > '9' || m2 < '0' || m2 > '9' {
		return 0, 0, false
	}
	h := int(h1-'0')*10 + int(h2-'0')
	m := int(m1-'0')*10 + int(m2-'0')
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, false
	}
	return h, m, true
}

// Validate checks bounds for all configurable maintenance settings.
func (c Config) Validate() error {
	if c.IntervalHours < 1 || c.IntervalHours > 720 {
		return fmt.Errorf("%w: interval_hours must be between 1 and 720", ErrInvalidConfig)
	}
	if strings.TrimSpace(c.ScheduleTime) != "" {
		if _, _, ok := parseScheduleTime(c.ScheduleTime); !ok {
			return fmt.Errorf("%w: schedule_time must be in 24-hour HH:MM format (00:00–23:59)", ErrInvalidConfig)
		}
	}
	if c.ArtworkGraceHours < 1 || c.ArtworkGraceHours > 168 {
		return fmt.Errorf("%w: artwork_grace_hours must be between 1 and 168", ErrInvalidConfig)
	}
	if c.SubtitleCacheDays < 1 || c.SubtitleCacheDays > 365 {
		return fmt.Errorf("%w: subtitle_cache_days must be between 1 and 365", ErrInvalidConfig)
	}
	if c.AuditRetentionDays < 7 || c.AuditRetentionDays > 3650 {
		return fmt.Errorf("%w: audit_retention_days must be between 7 and 3650", ErrInvalidConfig)
	}
	if c.TrickplayIntervalSeconds < 2 || c.TrickplayIntervalSeconds > 60 {
		return fmt.Errorf("%w: trickplay_interval_seconds must be between 2 and 60", ErrInvalidConfig)
	}
	if c.TrickplayWidth < 160 || c.TrickplayWidth > 480 || c.TrickplayWidth%2 != 0 {
		return fmt.Errorf("%w: trickplay_width must be an even number between 160 and 480", ErrInvalidConfig)
	}
	return nil
}

// RunProgress reports live progress for an in-flight housekeeping pass.
type RunProgress struct {
	Stage      string `json:"stage"`
	Label      string `json:"label"`
	Detail     string `json:"detail,omitempty"`
	Completed  int    `json:"completed"`
	Total      int    `json:"total"`
	Percent    int    `json:"percent"`
	Step       int    `json:"step"`
	TotalSteps int    `json:"total_steps"`
}

// RunReport summarizes the outcome of a single housekeeping pass.
type RunReport struct {
	StartedAt                     time.Time `json:"started_at"`
	FinishedAt                    time.Time `json:"finished_at"`
	DurationMS                    int64     `json:"duration_ms"`
	Trigger                       string    `json:"trigger"` // "scheduled" | "manual" | "resume" | "new_media"
	Task                          string    `json:"task,omitempty"`
	Canceled                      bool      `json:"canceled,omitempty"`
	SkippedArtworkScanBusy        bool      `json:"skipped_artwork_scan_busy,omitempty"`
	SkippedChaptersBusy           bool      `json:"skipped_chapters_busy,omitempty"`
	ArtworkFilesRemoved           int       `json:"artwork_files_removed"`
	ArtworkBytesFreed             int64     `json:"artwork_bytes_freed"`
	TranscodeItemsRemoved         int       `json:"transcode_items_removed"`
	TranscodeBytesFreed           int64     `json:"transcode_bytes_freed"`
	AuditEventsPruned             int64     `json:"audit_events_pruned"`
	DatabaseOptimized             bool      `json:"database_optimized"`
	ChapterThumbnailsGenerated    int       `json:"chapter_thumbnails_generated"`
	ChapterThumbnailsExisting     int       `json:"chapter_thumbnails_existing,omitempty"`
	ChapterThumbnailsTotal        int       `json:"chapter_thumbnails_total,omitempty"`
	ChapterThumbnailsMissingFiles int       `json:"chapter_thumbnails_missing_files,omitempty"`
	ChapterThumbnailsFailed       int       `json:"chapter_thumbnails_failed,omitempty"`
	SkippedTrickplayBusy          bool      `json:"skipped_trickplay_busy,omitempty"`
	TrickplayGenerated            int       `json:"trickplay_generated,omitempty"`
	TrickplayExisting             int       `json:"trickplay_existing,omitempty"`
	TrickplayTotal                int       `json:"trickplay_total,omitempty"`
	TrickplayFailed               int       `json:"trickplay_failed,omitempty"`
	TrickplayBytes                int64     `json:"trickplay_bytes,omitempty"`
	Errors                        []string  `json:"errors,omitempty"`
	// Outcome summarizes the pass: "completed", "partial" (some tasks skipped or errored),
	// "skipped" (nothing requested could run) or "canceled".
	Outcome string `json:"outcome,omitempty"`
	// Skipped lists tasks that did not run (or stopped early) and why.
	Skipped []SkippedTask `json:"skipped,omitempty"`
}

// SkippedTask records a housekeeping task that could not run or yielded part-way, with a
// human-readable reason (e.g. "Library scan in progress").
type SkippedTask struct {
	Task   string `json:"task"`
	Reason string `json:"reason"`
}

const (
	OutcomeCompleted = "completed"
	OutcomePartial   = "partial"
	OutcomeSkipped   = "skipped"
	OutcomeCanceled  = "canceled"
)

// Status is the full maintenance state exposed to the Admin Console.
type Status struct {
	Config         Config       `json:"config"`
	Running        bool         `json:"running"`
	Progress       *RunProgress `json:"progress,omitempty"`
	LastRun        *RunReport   `json:"last_run,omitempty"`
	NextRunAt      *time.Time   `json:"next_run_at,omitempty"`
	ServerTimezone string       `json:"server_timezone,omitempty"`
	// ChaptersResumePending is true while chapter still generation is paused for active
	// scans/playback and will resume automatically once the server has been idle.
	ChaptersResumePending bool `json:"chapters_resume_pending,omitempty"`
	// NewMediaPending is true while a pass for newly added media waits to start (scan
	// finished, no FFmpeg playback session, no other pass running; direct play is fine).
	NewMediaPending bool `json:"new_media_pending,omitempty"`
}

const (
	// chapterResumePoll is how often a paused chapter pass checks whether playback/scans ended.
	chapterResumePoll = 15 * time.Second
	// chapterResumeQuiet is how long the server must stay idle before resuming, so the
	// short gap between one episode ending and the next auto-playing does not trigger a run.
	chapterResumeQuiet = 90 * time.Second
	// chapterResumeMaxWait abandons the automatic resume; the next scheduled pass picks up.
	chapterResumeMaxWait = 24 * time.Hour

	// newMediaPoll is how often a queued new-media pass checks whether the server is idle.
	newMediaPoll = 5 * time.Second
	// newMediaQuiet is how long the server must stay idle before the new-media pass starts,
	// so back-to-back library scans (or watcher bursts) coalesce into one pass.
	newMediaQuiet = 30 * time.Second
	// newMediaMaxWait abandons a queued new-media pass; the next scheduled pass picks up.
	newMediaMaxWait = 24 * time.Hour
)

// Options injects runtime hooks used to guard housekeeping against active scans or streams.
type Options struct {
	DataDir             string
	IsScanRunning       func() bool
	ActiveTranscodeDirs func() map[string]struct{}
	// IsPlaybackActive reports any client playback (direct play included) seen recently.
	IsPlaybackActive func() bool
	Logger           *slog.Logger
}

// Service manages periodic and on-demand housekeeping with live-editable configuration.
type Service struct {
	store               *store.Store
	dataDir             string
	ffmpegBin           string
	isScanRunning       func() bool
	activeTranscodeDirs func() map[string]struct{}
	isPlaybackActive    func() bool
	logger              *slog.Logger

	reconfigCh chan struct{}

	// baseCtx outlives individual runs so a paused chapter pass can resume on its own.
	baseCtx        context.Context
	resumeArmed    bool
	resumeCancel   context.CancelFunc
	resumeGen      uint64
	resumePoll     time.Duration
	resumeQuiet    time.Duration
	resumeMaxWait  time.Duration
	newMediaArmed  bool
	newMediaCancel context.CancelFunc
	// newMediaIDs collects the items the queued new-media pass will process.
	newMediaIDs     map[string]struct{}
	newMediaGen     uint64
	newMediaPoll    time.Duration
	newMediaQuiet   time.Duration
	newMediaMaxWait time.Duration
	tpEncOnce       sync.Once
	tpEnc           trickplayEncoder

	updateMu        sync.Mutex
	mu              sync.Mutex
	cfg             Config
	running         bool
	cancelRun       context.CancelFunc
	progress        *RunProgress
	lastRun         *RunReport
	lastScheduledAt time.Time
	nextRunAt       *time.Time
	// lastBusyReason is the most recent reason a yielding task found the server busy.
	lastBusyReason string
}

// NewService constructs a maintenance service and hydrates saved config/last-run state from SQLite.
func NewService(ctx context.Context, s *store.Store, opts Options) *Service {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	ffmpegBin, _ := exec.LookPath("ffmpeg")
	svc := &Service{
		store:               s,
		dataDir:             opts.DataDir,
		ffmpegBin:           ffmpegBin,
		isScanRunning:       opts.IsScanRunning,
		activeTranscodeDirs: opts.ActiveTranscodeDirs,
		isPlaybackActive:    opts.IsPlaybackActive,
		logger:              logger,
		reconfigCh:          make(chan struct{}, 1),
		cfg:                 DefaultConfig(),
		baseCtx:             context.WithoutCancel(ctx),
		resumePoll:          chapterResumePoll,
		resumeQuiet:         chapterResumeQuiet,
		resumeMaxWait:       chapterResumeMaxWait,
		newMediaPoll:        newMediaPoll,
		newMediaQuiet:       newMediaQuiet,
		newMediaMaxWait:     newMediaMaxWait,
	}
	svc.loadPersisted(ctx)
	return svc
}

func (s *Service) loadPersisted(ctx context.Context) {
	if s.store == nil {
		return
	}
	if raw, err := s.store.GetSetting(ctx, ConfigSettingKey); err == nil && strings.TrimSpace(raw) != "" {
		cfg := DefaultConfig()
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil && cfg.normalize().Validate() == nil {
			cfg = cfg.normalize()
			s.cfg = cfg
		}
	}
	if raw, err := s.store.GetSetting(ctx, LastRunSettingKey); err == nil && strings.TrimSpace(raw) != "" {
		var rep RunReport
		if err := json.Unmarshal([]byte(raw), &rep); err == nil && !rep.StartedAt.IsZero() {
			s.lastRun = &rep
		}
	}
	if raw, err := s.store.GetSetting(ctx, LastScheduledSettingKey); err == nil && strings.TrimSpace(raw) != "" {
		if ts, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(raw)); err == nil && !ts.IsZero() {
			s.lastScheduledAt = ts.UTC()
		}
	}
	if s.lastRun != nil && s.lastRun.Trigger == TriggerScheduled && !s.lastRun.FinishedAt.IsZero() {
		if s.lastScheduledAt.IsZero() || s.lastRun.FinishedAt.After(s.lastScheduledAt) {
			s.lastScheduledAt = s.lastRun.FinishedAt.UTC()
		}
	}
}

// Status returns a snapshot of current configuration, running state, last run report, and next run time.
func (s *Service) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	tzName, _ := time.Now().In(time.Local).Zone()
	out := Status{
		Config:                s.cfg,
		Running:               s.running,
		ServerTimezone:        tzName,
		ChaptersResumePending: s.resumeArmed,
		NewMediaPending:       s.newMediaArmed,
	}
	if s.progress != nil {
		p := *s.progress
		out.Progress = &p
	}
	if s.lastRun != nil {
		cp := *s.lastRun
		if len(s.lastRun.Errors) > 0 {
			cp.Errors = append([]string(nil), s.lastRun.Errors...)
		}
		out.LastRun = &cp
	}
	if s.nextRunAt != nil && s.cfg.Enabled {
		t := *s.nextRunAt
		out.NextRunAt = &t
	}
	return out
}

// UpdateConfig validates, persists, and hot-reloads the maintenance schedule.
func (s *Service) UpdateConfig(ctx context.Context, cfg Config) (Status, error) {
	cfg = cfg.normalize()
	if err := cfg.Validate(); err != nil {
		return Status{}, err
	}
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	if s.store != nil {
		b, err := json.Marshal(cfg)
		if err != nil {
			return Status{}, err
		}
		if err := s.store.SetSetting(ctx, ConfigSettingKey, string(b)); err != nil {
			return Status{}, err
		}
	}
	s.mu.Lock()
	s.cfg = cfg
	if !cfg.Enabled {
		s.nextRunAt = nil
	} else {
		next := s.computeNextRunLocked(time.Now().UTC())
		s.nextRunAt = &next
	}
	s.mu.Unlock()

	select {
	case s.reconfigCh <- struct{}{}:
	default:
	}
	return s.Status(), nil
}

// Run executes the background maintenance scheduler until ctx is canceled.
func (s *Service) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		enabled := s.cfg.Enabled
		var wait time.Duration
		if enabled {
			now := time.Now().UTC()
			next := s.computeNextRunLocked(now)
			s.nextRunAt = &next
			wait = time.Until(next)
			if wait < 0 {
				wait = 0
			}
		} else {
			s.nextRunAt = nil
		}
		s.mu.Unlock()

		if !enabled {
			select {
			case <-ctx.Done():
				return
			case <-s.reconfigCh:
				continue
			}
		}

		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-s.reconfigCh:
			timer.Stop()
			continue
		case <-timer.C:
			_, err := s.runOnce(ctx, TriggerScheduled, "")
			if err != nil && !errors.Is(err, ErrAlreadyRunning) && ctx.Err() == nil {
				s.logger.Warn("scheduled maintenance run failed", "error", err)
			}
		}
	}
}

// RunNow triggers an immediate housekeeping pass synchronously and returns the updated Status.
// It detaches from the caller's HTTP request context so browser navigation or refresh does not
// abort an in-flight pass; callers stop a pass explicitly via CancelRun.
func (s *Service) RunNow(ctx context.Context) (Status, error) {
	return s.RunTask(ctx, "")
}

// RunTask runs either a full housekeeping pass (task == "") or a single named task immediately.
func (s *Service) RunTask(ctx context.Context, task string) (Status, error) {
	task = strings.TrimSpace(task)
	if !isValidTask(task) {
		return Status{}, fmt.Errorf("%w: unknown task %q", ErrInvalidTask, task)
	}
	if _, err := s.runOnce(context.WithoutCancel(ctx), TriggerManual, task); err != nil {
		return Status{}, err
	}
	return s.Status(), nil
}

// CancelRun requests cancellation of any currently running maintenance pass and drops any
// pending automatic chapter-still resume or new-media pass. Returns true if a pass or pending
// automatic pass was stopped.
func (s *Service) CancelRun() bool {
	s.mu.Lock()
	cancel := s.cancelRun
	running := s.running
	resumeCancel := s.resumeCancel
	resumeArmed := s.resumeArmed
	s.resumeArmed = false
	s.resumeCancel = nil
	newMediaCancel := s.newMediaCancel
	newMediaArmed := s.newMediaArmed
	s.newMediaArmed = false
	s.newMediaCancel = nil
	s.newMediaIDs = nil
	if running && s.progress != nil {
		p := *s.progress
		p.Label = "Canceling housekeeping pass…"
		s.progress = &p
	}
	s.mu.Unlock()
	if resumeCancel != nil {
		resumeCancel()
	}
	if newMediaCancel != nil {
		newMediaCancel()
	}
	if running && cancel != nil {
		cancel()
		return true
	}
	return resumeArmed || newMediaArmed
}

func (s *Service) computeNextRunLocked(now time.Time) time.Time {
	return computeNextRun(now, s.cfg, s.lastRun, s.lastScheduledAt, time.Local)
}

func computeNextRun(now time.Time, cfg Config, lastRun *RunReport, lastScheduledAt time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	interval := time.Duration(cfg.IntervalHours) * time.Hour

	scheduledAnchor := lastScheduledAt
	if scheduledAnchor.IsZero() && lastRun != nil && lastRun.Trigger == TriggerScheduled && !lastRun.FinishedAt.IsZero() {
		scheduledAnchor = lastRun.FinishedAt
	}

	if h, m, ok := parseScheduleTime(cfg.ScheduleTime); ok {
		nowLocal := now.In(loc)
		anchorToday := time.Date(nowLocal.Year(), nowLocal.Month(), nowLocal.Day(), h, m, 0, 0, loc)

		if cfg.IntervalHours < 24 {
			// Sub-day interval anchored at HH:MM (e.g., 12h anchored at 03:00 -> 03:00, 15:00).
			candidate := anchorToday
			for candidate.After(nowLocal) {
				prev := candidate.Add(-interval)
				if !prev.After(nowLocal) {
					break
				}
				candidate = prev
			}
			for !candidate.After(nowLocal) {
				candidate = candidate.Add(interval)
			}
			if !scheduledAnchor.IsZero() {
				minNext := scheduledAnchor.In(loc).Add(interval / 2)
				for candidate.Before(minNext) {
					candidate = candidate.Add(interval)
				}
			}
			return candidate.UTC()
		}

		// Daily or multi-day interval at HH:MM local time.
		days := cfg.IntervalHours / 24
		if days < 1 {
			days = 1
		}
		if !scheduledAnchor.IsZero() {
			lastLocal := scheduledAnchor.In(loc)
			candidate := time.Date(lastLocal.Year(), lastLocal.Month(), lastLocal.Day(), h, m, 0, 0, loc).AddDate(0, 0, days)
			if candidate.After(nowLocal) {
				return candidate.UTC()
			}
		}
		candidate := anchorToday
		if !candidate.After(nowLocal) {
			candidate = candidate.AddDate(0, 0, 1)
		}
		return candidate.UTC()
	}

	rollingAnchor := scheduledAnchor
	if lastRun != nil && !lastRun.FinishedAt.IsZero() && !isBackgroundTrigger(lastRun.Trigger) {
		rollingAnchor = lastRun.FinishedAt
	}
	if !rollingAnchor.IsZero() {
		next := rollingAnchor.Add(interval)
		if next.After(now) {
			return next
		}
		// Overdue on startup: wait 2 minutes so server boot & initial scans settle first.
		return now.Add(2 * time.Minute)
	}
	return now.Add(interval)
}

func (s *Service) runOnce(ctx context.Context, trigger, task string) (RunReport, error) {
	runCtx, cancel := context.WithCancel(ctx)

	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		cancel()
		return RunReport{}, ErrAlreadyRunning
	}
	s.running = true
	s.cancelRun = cancel
	s.lastBusyReason = ""
	cfg := s.cfg
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		s.running = false
		s.cancelRun = nil
		s.progress = nil
		if s.cfg.Enabled {
			next := s.computeNextRunLocked(time.Now().UTC())
			s.nextRunAt = &next
		} else {
			s.nextRunAt = nil
		}
		s.mu.Unlock()

		// Wake scheduler so its active timer stays aligned with the recalculated NextRunAt.
		select {
		case s.reconfigCh <- struct{}{}:
		default:
		}
	}()

	perItem := task == "" || task == taskNewMedia
	wantArtwork := task == "" && cfg.CleanupOrphanedArtwork || task == TaskArtwork
	wantTranscode := task == "" && cfg.CleanupTranscodeCache || task == TaskTranscodeCache
	wantAudit := task == "" && cfg.PruneAuditLogs || task == TaskAudit
	wantChapters := perItem && cfg.GenerateChapterThumbnails || task == TaskChapterThumbnails
	wantTrickplay := perItem && cfg.GenerateTrickplay || task == TaskTrickplay
	wantOptimize := task == "" && cfg.OptimizeDatabase || task == TaskOptimizeDB

	runArtwork := wantArtwork && s.dataDir != "" && s.store != nil
	runTranscode := wantTranscode && s.dataDir != ""
	runAudit := wantAudit && s.store != nil
	runChapters := wantChapters && s.dataDir != "" && s.store != nil && s.ffmpegBin != ""
	runTrickplay := wantTrickplay && s.dataDir != "" && s.store != nil && s.ffmpegBin != ""
	runOptimize := wantOptimize && s.store != nil

	var skipped []SkippedTask
	skip := func(name, reason string) {
		skipped = append(skipped, SkippedTask{Task: name, Reason: reason})
	}
	unavailable := func(name string, needsFFmpeg bool) {
		if needsFFmpeg && s.ffmpegBin == "" {
			skip(name, "FFmpeg not available")
			return
		}
		skip(name, "Not available on this server")
	}
	for _, t := range []struct {
		name        string
		want, run   bool
		needsFFmpeg bool
	}{
		{TaskArtwork, wantArtwork, runArtwork, false},
		{TaskTranscodeCache, wantTranscode, runTranscode, false},
		{TaskAudit, wantAudit, runAudit, false},
		{TaskChapterThumbnails, wantChapters, runChapters, true},
		{TaskTrickplay, wantTrickplay, runTrickplay, true},
		{TaskOptimizeDB, wantOptimize, runOptimize, false},
	} {
		if t.want && !t.run {
			unavailable(t.name, t.needsFFmpeg)
		}
	}
	// ran counts tasks that did at least part of their work.
	ran := 0
	busyReason := func() string {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.lastBusyReason != "" {
			return s.lastBusyReason
		}
		return "Server busy"
	}

	totalSteps := 0
	if runArtwork {
		totalSteps++
	}
	if runTranscode {
		totalSteps++
	}
	if runAudit {
		totalSteps++
	}
	if runChapters {
		totalSteps++
	}
	if runTrickplay {
		totalSteps++
	}
	if runOptimize {
		totalSteps++
	}
	if totalSteps == 0 {
		totalSteps = 1
	}

	stepNum := 0
	setProgress := func(stage, label, detail string, completed, total int, subFrac float64) {
		if subFrac < 0 {
			subFrac = 0
		}
		if subFrac > 1 {
			subFrac = 1
		}
		pct := int(((float64(stepNum-1) + subFrac) / float64(totalSteps)) * 100.0)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		s.mu.Lock()
		s.progress = &RunProgress{
			Stage:      stage,
			Label:      label,
			Detail:     detail,
			Completed:  completed,
			Total:      total,
			Percent:    pct,
			Step:       stepNum,
			TotalSteps: totalSteps,
		}
		s.mu.Unlock()
	}

	started := time.Now().UTC()
	report := RunReport{
		StartedAt: started,
		Trigger:   trigger,
		Task:      task,
	}
	if task == taskNewMedia {
		// Reported like a (partial) housekeeping pass: the trigger says why it ran.
		report.Task = ""
	}

	// 1. Orphaned artwork cleanup (guarded against active scans & grace window).
	if runCtx.Err() == nil && runArtwork {
		stepNum++
		setProgress("artwork", "Cleaning orphaned artwork", "", 0, 0, 0)
		if s.isScanRunning != nil && s.isScanRunning() {
			report.SkippedArtworkScanBusy = true
			skip(TaskArtwork, BusyReasonScan)
		} else {
			ran++
			removed, freed, err := s.cleanOrphanedArtwork(runCtx, started.Add(-time.Duration(cfg.ArtworkGraceHours)*time.Hour))
			report.ArtworkFilesRemoved = removed
			report.ArtworkBytesFreed = freed
			if err != nil && runCtx.Err() == nil {
				report.Errors = append(report.Errors, "artwork cleanup: "+err.Error())
			}
		}
		setProgress("artwork", "Cleaning orphaned artwork", "", 1, 1, 1)
	}

	// 2. Stale transcode session dirs & aged extracted subtitle cache cleanup.
	if runCtx.Err() == nil && runTranscode {
		stepNum++
		setProgress("transcode_cache", "Cleaning transcode & subtitle cache", "", 0, 0, 0)
		subCutoff := started.Add(-time.Duration(cfg.SubtitleCacheDays) * 24 * time.Hour)
		sessionCutoff := started.Add(-2 * time.Hour)
		ran++
		removed, freed, err := s.cleanTranscodeCache(sessionCutoff, subCutoff)
		report.TranscodeItemsRemoved = removed
		report.TranscodeBytesFreed = freed
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "transcode cache cleanup: "+err.Error())
		}
		setProgress("transcode_cache", "Cleaning transcode & subtitle cache", "", 1, 1, 1)
	}

	// 3. Audit log retention pruning.
	if runCtx.Err() == nil && runAudit {
		stepNum++
		setProgress("audit", "Pruning old audit logs", "", 0, 0, 0)
		cutoff := started.Add(-time.Duration(cfg.AuditRetentionDays) * 24 * time.Hour)
		ran++
		pruned, err := s.store.PruneAuditEventsBefore(runCtx, cutoff)
		report.AuditEventsPruned = pruned
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "audit log pruning: "+err.Error())
		}
		setProgress("audit", "Pruning old audit logs", "", 1, 1, 1)
	}

	// 4. Chapter still thumbnail generation (sequential, whole backlog per run, yields to active scans/playback).
	if runCtx.Err() == nil && runChapters {
		stepNum++
		setProgress("chapter_thumbnails", "Generating chapter stills", "Scanning chapter index…", 0, 0, 0)
		res, err := s.generateChapterThumbnails(runCtx, func(done, total int, detail string) {
			frac := 0.0
			if total > 0 {
				frac = float64(done) / float64(total)
			}
			setProgress("chapter_thumbnails", "Generating chapter stills", detail, done, total, frac)
		})
		report.ChapterThumbnailsGenerated = res.Generated
		report.ChapterThumbnailsExisting = res.Existing
		report.ChapterThumbnailsTotal = res.Total
		report.ChapterThumbnailsMissingFiles = res.MissingFiles
		report.ChapterThumbnailsFailed = res.Unextractable
		report.SkippedChaptersBusy = res.SkippedBusy && runCtx.Err() == nil
		if report.SkippedChaptersBusy {
			skip(TaskChapterThumbnails, busyReason())
		}
		if !report.SkippedChaptersBusy || res.Generated > 0 {
			ran++
		}
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "chapter thumbnails: "+err.Error())
		}
		setProgress("chapter_thumbnails", "Generating chapter stills", "", res.Total, res.Total, 1)
	}

	// 5. Trickplay scrubbing previews (keyframe-only, paused in place while streams/scans run).
	if runCtx.Err() == nil && runTrickplay {
		stepNum++
		setProgress("trickplay", "Generating scrubbing previews", "Checking items…", 0, 0, 0)
		res, err := s.generateTrickplay(runCtx, cfg, func(done, total int, detail string) {
			frac := 0.0
			if total > 0 {
				frac = float64(done) / float64(total)
			}
			setProgress("trickplay", "Generating scrubbing previews", detail, done, total, frac)
		})
		report.TrickplayGenerated = res.Generated
		report.TrickplayExisting = res.Existing
		report.TrickplayTotal = res.Total
		report.TrickplayFailed = res.Unextractable
		report.TrickplayBytes = res.Bytes
		report.SkippedTrickplayBusy = res.SkippedBusy && runCtx.Err() == nil
		if report.SkippedTrickplayBusy {
			skip(TaskTrickplay, busyReason())
		}
		if !report.SkippedTrickplayBusy || res.Generated > 0 {
			ran++
		}
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "trickplay: "+err.Error())
		}
		setProgress("trickplay", "Generating scrubbing previews", "", res.Total, res.Total, 1)
	}

	// 6. Non-blocking SQLite query-planner optimization + passive WAL checkpoint.
	if runCtx.Err() == nil && runOptimize {
		stepNum++
		setProgress("optimize_db", "Optimizing SQLite database", "", 0, 0, 0)
		ran++
		optErr := s.store.Optimize(runCtx)
		cpErr := s.store.CheckpointPassive(runCtx)
		if optErr != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "db optimize: "+optErr.Error())
		}
		if cpErr != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "wal checkpoint: "+cpErr.Error())
		}
		if optErr == nil && cpErr == nil {
			report.DatabaseOptimized = true
		}
		setProgress("optimize_db", "Optimizing SQLite database", "", 1, 1, 1)
	}

	if runCtx.Err() != nil {
		report.Canceled = true
	}
	report.Skipped = skipped
	switch {
	case report.Canceled:
		report.Outcome = OutcomeCanceled
	case len(skipped) > 0 && ran == 0:
		report.Outcome = OutcomeSkipped
	case len(skipped) > 0:
		report.Outcome = OutcomePartial
	default:
		report.Outcome = OutcomeCompleted
	}

	report.FinishedAt = time.Now().UTC()
	report.DurationMS = report.FinishedAt.Sub(started).Milliseconds()

	s.mu.Lock()
	cp := report
	s.lastRun = &cp
	if trigger == TriggerScheduled {
		s.lastScheduledAt = report.FinishedAt
	}
	s.mu.Unlock()

	if report.SkippedChaptersBusy && !report.Canceled {
		s.armChapterResume()
	}

	if s.store != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if b, err := json.Marshal(report); err == nil {
			if err := s.store.SetSetting(persistCtx, LastRunSettingKey, string(b)); err != nil {
				s.logger.Warn("failed to persist maintenance run report", "error", err)
			}
		}
		if trigger == TriggerScheduled {
			if err := s.store.SetSetting(persistCtx, LastScheduledSettingKey, report.FinishedAt.UTC().Format(time.RFC3339Nano)); err != nil {
				s.logger.Warn("failed to persist last scheduled maintenance timestamp", "error", err)
			}
		}
	}

	s.logger.Info("maintenance housekeeping completed",
		"trigger", trigger,
		"task", task,
		"outcome", report.Outcome,
		"skipped", len(report.Skipped),
		"duration_ms", report.DurationMS,
		"artwork_removed", report.ArtworkFilesRemoved,
		"transcode_removed", report.TranscodeItemsRemoved,
		"audit_pruned", report.AuditEventsPruned,
		"chapter_thumbnails_generated", report.ChapterThumbnailsGenerated,
		"db_optimized", report.DatabaseOptimized,
		"errors", len(report.Errors),
	)
	return report, nil
}

func (s *Service) cleanOrphanedArtwork(ctx context.Context, graceCutoff time.Time) (int, int64, error) {
	artRoot := filepath.Clean(filepath.Join(s.dataDir, "artwork"))
	if _, err := os.Stat(artRoot); os.IsNotExist(err) {
		return 0, 0, nil
	}

	refFiles, refPeople, err := s.store.ListReferencedArtworkPaths(ctx)
	if err != nil {
		return 0, 0, err
	}
	liveItemIDs, err := s.store.ListAllMediaItemIDs(ctx)
	if err != nil {
		return 0, 0, err
	}

	peopleRoot := filepath.Join(artRoot, "people")
	chaptersRoot := filepath.Join(artRoot, "chapters")
	trickplayRoot := filepath.Join(artRoot, "trickplay")
	var removed int
	var bytesFreed int64
	var dirs []string

	walkErr := filepath.WalkDir(artRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		cleanPath := filepath.Clean(path)
		if d.IsDir() {
			if parent := filepath.Dir(cleanPath); parent == chaptersRoot || parent == trickplayRoot {
				if _, ok := liveItemIDs[d.Name()]; ok {
					return filepath.SkipDir
				}
			}
			if cleanPath != artRoot && cleanPath != peopleRoot && cleanPath != chaptersRoot && cleanPath != trickplayRoot {
				dirs = append(dirs, cleanPath)
			}
			return nil
		}
		// Never touch in-flight temp files.
		if strings.HasSuffix(d.Name(), ".tmp") || strings.HasSuffix(d.Name(), ".tmp.jpg") {
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			return nil
		}
		// Grace period guardrail: never delete recently created artwork.
		if info.ModTime().After(graceCutoff) {
			return nil
		}

		if filepath.Dir(cleanPath) == peopleRoot {
			if _, ok := refPeople[d.Name()]; ok {
				return nil
			}
		} else {
			if _, ok := refFiles[cleanPath]; ok {
				return nil
			}
		}

		if rmErr := os.Remove(cleanPath); rmErr == nil {
			removed++
			bytesFreed += info.Size()
		}
		return nil
	})

	// Prune empty subdirectories deepest-first.
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // only succeeds when empty
	}

	return removed, bytesFreed, walkErr
}

func (s *Service) cleanTranscodeCache(sessionCutoff, subCutoff time.Time) (int, int64, error) {
	baseDir := filepath.Clean(filepath.Join(s.dataDir, "transcode"))
	entries, err := os.ReadDir(baseDir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, 0, nil
		}
		return 0, 0, err
	}

	var activeDirs map[string]struct{}
	if s.activeTranscodeDirs != nil {
		activeDirs = s.activeTranscodeDirs()
	}

	var removed int
	var bytesFreed int64

	for _, entry := range entries {
		fullPath := filepath.Clean(filepath.Join(baseDir, entry.Name()))
		if entry.Name() == "subs" && entry.IsDir() {
			subEntries, subErr := os.ReadDir(fullPath)
			if subErr != nil {
				continue
			}
			for _, se := range subEntries {
				if se.IsDir() || strings.HasSuffix(se.Name(), ".tmp") {
					continue
				}
				info, infoErr := se.Info()
				if infoErr != nil || info.ModTime().After(subCutoff) {
					continue
				}
				if os.Remove(filepath.Join(fullPath, se.Name())) == nil {
					removed++
					bytesFreed += info.Size()
				}
			}
			continue
		}

		if _, active := activeDirs[fullPath]; active {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil || info.ModTime().After(sessionCutoff) {
			continue
		}
		sz := dirSize(fullPath)
		if os.RemoveAll(fullPath) == nil {
			removed++
			bytesFreed += sz
		}
	}

	return removed, bytesFreed, nil
}

func dirSize(path string) int64 {
	var total int64
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}

// armChapterResume starts (at most one) background watcher that re-runs chapter still
// generation once active scans and transcodes have ended and the server has stayed idle
// for resumeQuiet. If that pass is interrupted again, it re-arms itself.
func (s *Service) armChapterResume() {
	s.mu.Lock()
	if s.resumeArmed || s.baseCtx == nil {
		s.mu.Unlock()
		return
	}
	s.resumeArmed = true
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.resumeCancel = cancel
	s.resumeGen++
	gen := s.resumeGen
	poll, quiet, maxWait := s.resumePoll, s.resumeQuiet, s.resumeMaxWait
	s.mu.Unlock()

	go func() {
		defer cancel()
		disarm := func() {
			s.mu.Lock()
			if s.resumeGen == gen {
				s.resumeArmed = false
				s.resumeCancel = nil
			}
			s.mu.Unlock()
		}
		deadline := time.Now().Add(maxWait)
		var idleSince time.Time
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				disarm()
				return
			case <-ticker.C:
			}
			if time.Now().After(deadline) {
				s.logger.Info("chapter still generation resume abandoned; next scheduled pass will continue")
				disarm()
				return
			}
			s.mu.Lock()
			running := s.running
			s.mu.Unlock()
			if running || s.isBusyForChapterThumbnails(ctx) {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			if time.Since(idleSince) < quiet {
				continue
			}
			// Disarm before running so a run that pauses again can re-arm a fresh watcher.
			disarm()
			s.logger.Info("resuming paused chapter still generation")
			if _, err := s.runOnce(s.baseCtx, TriggerResume, TaskChapterThumbnails); err != nil {
				if errors.Is(err, ErrAlreadyRunning) {
					s.armChapterResume()
				} else if ctx.Err() == nil {
					s.logger.Warn("resumed chapter still generation failed", "error", err)
				}
			}
			return
		}
	}()
}

// NotifyMediaAdded queues a pass that generates chapter stills and trickplay for itemIDs, the
// media a library scan just added or changed, so they are ready now instead of at the next
// scheduled pass. It does nothing unless automatic housekeeping and at least one of those
// tasks are enabled.
//
// The pass processes only those items (the backlog stays with scheduled passes) and runs
// through direct play: it starts once no scan, FFmpeg playback session or other pass has been
// active for newMediaQuiet, and while it runs it pauses only for scans and FFmpeg sessions
// (see newMediaScope). A pass that is already running lists its items when its chapter or
// trickplay step starts, so it can miss media added after that; waiting for it to finish and
// then running again covers those. Notifications coalesce: at most one pass is queued (later
// IDs join it), and a notification arriving while the queued pass runs queues one follow-up.
func (s *Service) NotifyMediaAdded(itemIDs []string) {
	if len(itemIDs) == 0 {
		return
	}
	s.mu.Lock()
	cfg := s.cfg
	if !cfg.Enabled || (!cfg.GenerateChapterThumbnails && !cfg.GenerateTrickplay) ||
		s.ffmpegBin == "" || s.dataDir == "" || s.store == nil || s.baseCtx == nil {
		s.mu.Unlock()
		return
	}
	if s.newMediaIDs == nil {
		s.newMediaIDs = make(map[string]struct{}, len(itemIDs))
	}
	for _, id := range itemIDs {
		s.newMediaIDs[id] = struct{}{}
	}
	if s.newMediaArmed {
		s.mu.Unlock()
		return
	}
	s.newMediaArmed = true
	ctx, cancel := context.WithCancel(s.baseCtx)
	s.newMediaCancel = cancel
	s.newMediaGen++
	gen := s.newMediaGen
	poll, quiet, maxWait := s.newMediaPoll, s.newMediaQuiet, s.newMediaMaxWait
	s.mu.Unlock()

	// idleCtx only selects the new-media busy policy for the wait below.
	idleCtx := withNewMediaScope(ctx, nil)
	go func() {
		defer cancel()
		// disarm clears the queued pass and hands back its items; ok is false if it was
		// already dropped (canceled, or superseded by a newer queued pass).
		disarm := func() (ids map[string]struct{}, ok bool) {
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.newMediaGen != gen || !s.newMediaArmed {
				return nil, false
			}
			ids = s.newMediaIDs
			s.newMediaArmed = false
			s.newMediaCancel = nil
			s.newMediaIDs = nil
			return ids, true
		}
		deadline := time.Now().Add(maxWait)
		var idleSince time.Time
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				disarm()
				return
			case <-ticker.C:
			}
			if time.Now().After(deadline) {
				s.logger.Info("new media pass abandoned; next scheduled pass will continue")
				disarm()
				return
			}
			s.mu.Lock()
			running := s.running
			s.mu.Unlock()
			if running || s.busyReasonFor(idleCtx) != "" {
				idleSince = time.Time{}
				continue
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
			if time.Since(idleSince) < quiet {
				continue
			}
			// Disarm before running so media added during this pass queues a follow-up.
			ids, ok := disarm()
			if !ok {
				return // canceled in the meantime
			}
			s.logger.Info("generating chapter stills and trickplay for new media", "items", len(ids))
			runCtx := withNewMediaScope(s.baseCtx, ids)
			if _, err := s.runOnce(runCtx, TriggerNewMedia, taskNewMedia); err != nil {
				if errors.Is(err, ErrAlreadyRunning) {
					s.NotifyMediaAdded(mapKeys(ids))
				} else {
					s.logger.Warn("new media pass failed", "error", err)
				}
			}
			return
		}
	}()
}

func mapKeys(m map[string]struct{}) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
