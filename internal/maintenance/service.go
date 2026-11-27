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
	TaskOptimizeDB        = "optimize_db"
)

func isValidTask(task string) bool {
	switch task {
	case "", TaskArtwork, TaskTranscodeCache, TaskAudit, TaskChapterThumbnails, TaskOptimizeDB:
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
	Trigger                       string    `json:"trigger"` // "scheduled" | "manual"
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
	Errors                        []string  `json:"errors,omitempty"`
}

// Status is the full maintenance state exposed to the Admin Console.
type Status struct {
	Config         Config       `json:"config"`
	Running        bool         `json:"running"`
	Progress       *RunProgress `json:"progress,omitempty"`
	LastRun        *RunReport   `json:"last_run,omitempty"`
	NextRunAt      *time.Time   `json:"next_run_at,omitempty"`
	ServerTimezone string       `json:"server_timezone,omitempty"`
}

// Options injects runtime hooks used to guard housekeeping against active scans or streams.
type Options struct {
	DataDir             string
	IsScanRunning       func() bool
	ActiveTranscodeDirs func() map[string]struct{}
	Logger              *slog.Logger
}

// Service manages periodic and on-demand housekeeping with live-editable configuration.
type Service struct {
	store               *store.Store
	dataDir             string
	ffmpegBin           string
	isScanRunning       func() bool
	activeTranscodeDirs func() map[string]struct{}
	logger              *slog.Logger

	reconfigCh chan struct{}

	updateMu        sync.Mutex
	mu              sync.Mutex
	cfg             Config
	running         bool
	cancelRun       context.CancelFunc
	progress        *RunProgress
	lastRun         *RunReport
	lastScheduledAt time.Time
	nextRunAt       *time.Time
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
		logger:              logger,
		reconfigCh:          make(chan struct{}, 1),
		cfg:                 DefaultConfig(),
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
		if err := json.Unmarshal([]byte(raw), &cfg); err == nil && cfg.Validate() == nil {
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
	if s.lastRun != nil && s.lastRun.Trigger == "scheduled" && !s.lastRun.FinishedAt.IsZero() {
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
		Config:         s.cfg,
		Running:        s.running,
		ServerTimezone: tzName,
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
			_, err := s.runOnce(ctx, "scheduled", "")
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
	if _, err := s.runOnce(context.WithoutCancel(ctx), "manual", task); err != nil {
		return Status{}, err
	}
	return s.Status(), nil
}

// CancelRun requests cancellation of any currently running maintenance pass.
// Returns true if a running pass was signaled to stop.
func (s *Service) CancelRun() bool {
	s.mu.Lock()
	cancel := s.cancelRun
	running := s.running
	if running && s.progress != nil {
		p := *s.progress
		p.Label = "Canceling housekeeping pass…"
		s.progress = &p
	}
	s.mu.Unlock()
	if running && cancel != nil {
		cancel()
		return true
	}
	return false
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
	if scheduledAnchor.IsZero() && lastRun != nil && lastRun.Trigger == "scheduled" && !lastRun.FinishedAt.IsZero() {
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

	if lastRun != nil && !lastRun.FinishedAt.IsZero() {
		next := lastRun.FinishedAt.Add(interval)
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

	runArtwork := (task == "" && cfg.CleanupOrphanedArtwork || task == TaskArtwork) && s.dataDir != "" && s.store != nil
	runTranscode := (task == "" && cfg.CleanupTranscodeCache || task == TaskTranscodeCache) && s.dataDir != ""
	runAudit := (task == "" && cfg.PruneAuditLogs || task == TaskAudit) && s.store != nil
	runChapters := (task == "" && cfg.GenerateChapterThumbnails || task == TaskChapterThumbnails) && s.dataDir != "" && s.store != nil && s.ffmpegBin != ""
	runOptimize := (task == "" && cfg.OptimizeDatabase || task == TaskOptimizeDB) && s.store != nil

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

	// 1. Orphaned artwork cleanup (guarded against active scans & grace window).
	if runCtx.Err() == nil && runArtwork {
		stepNum++
		setProgress("artwork", "Cleaning orphaned artwork", "", 0, 0, 0)
		if s.isScanRunning != nil && s.isScanRunning() {
			report.SkippedArtworkScanBusy = true
		} else {
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
		pruned, err := s.store.PruneAuditEventsBefore(runCtx, cutoff)
		report.AuditEventsPruned = pruned
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "audit log pruning: "+err.Error())
		}
		setProgress("audit", "Pruning old audit logs", "", 1, 1, 1)
	}

	// 4. Chapter still thumbnail generation (sequential, capped, yields to active scans/transcodes).
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
		report.SkippedChaptersBusy = res.SkippedBusy && runCtx.Err() == nil
		if err != nil && runCtx.Err() == nil {
			report.Errors = append(report.Errors, "chapter thumbnails: "+err.Error())
		}
		setProgress("chapter_thumbnails", "Generating chapter stills", "", res.Total, res.Total, 1)
	}

	// 5. Non-blocking SQLite query-planner optimization + passive WAL checkpoint.
	if runCtx.Err() == nil && runOptimize {
		stepNum++
		setProgress("optimize_db", "Optimizing SQLite database", "", 0, 0, 0)
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

	report.FinishedAt = time.Now().UTC()
	report.DurationMS = report.FinishedAt.Sub(started).Milliseconds()

	s.mu.Lock()
	cp := report
	s.lastRun = &cp
	if trigger == "scheduled" {
		s.lastScheduledAt = report.FinishedAt
	}
	s.mu.Unlock()

	if s.store != nil {
		persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if b, err := json.Marshal(report); err == nil {
			if err := s.store.SetSetting(persistCtx, LastRunSettingKey, string(b)); err != nil {
				s.logger.Warn("failed to persist maintenance run report", "error", err)
			}
		}
		if trigger == "scheduled" {
			if err := s.store.SetSetting(persistCtx, LastScheduledSettingKey, report.FinishedAt.UTC().Format(time.RFC3339Nano)); err != nil {
				s.logger.Warn("failed to persist last scheduled maintenance timestamp", "error", err)
			}
		}
	}

	s.logger.Info("maintenance housekeeping completed",
		"trigger", trigger,
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
			if filepath.Dir(cleanPath) == chaptersRoot {
				if _, ok := liveItemIDs[d.Name()]; ok {
					return filepath.SkipDir
				}
			}
			if cleanPath != artRoot && cleanPath != peopleRoot && cleanPath != chaptersRoot {
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
