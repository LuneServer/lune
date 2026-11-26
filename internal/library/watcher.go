package library

import (
	"context"
	"io/fs"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watcher monitors registered library directories with fsnotify + debouncing and runs
// an optional periodic fallback scan (useful on FUSE/network filesystems like Unraid /mnt/user).
type Watcher struct {
	svc      *Service
	debounce time.Duration
	interval time.Duration

	mu      sync.Mutex
	fsw     *fsnotify.Watcher
	dirLib  map[string]string
	timers  map[string]*time.Timer
	syncCh  chan struct{}
}

// NewWatcher constructs a background library watcher.
func NewWatcher(svc *Service, periodicInterval time.Duration) *Watcher {
	return &Watcher{
		svc:      svc,
		debounce: 5 * time.Second,
		interval: periodicInterval,
		dirLib:   make(map[string]string),
		timers:   make(map[string]*time.Timer),
		syncCh:   make(chan struct{}, 1),
	}
}

// NotifyLibrariesChanged triggers a watch-set refresh after a library is added or removed.
func (w *Watcher) NotifyLibrariesChanged() {
	if w == nil {
		return
	}
	select {
	case w.syncCh <- struct{}{}:
	default:
	}
}

// Run blocks until ctx is canceled, watching library roots and triggering debounced incremental scans.
func (w *Watcher) Run(ctx context.Context) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		w.svc.logger.Warn("fsnotify unavailable; falling back to periodic scans only", "error", err)
	} else {
		w.mu.Lock()
		w.fsw = fsw
		w.mu.Unlock()
		defer fsw.Close()
		w.syncWatches(ctx)
	}

	if w.svc.HasEnricher() {
		go func() {
			if err := w.svc.BackfillStaleLibraries(ctx); err != nil && ctx.Err() == nil {
				w.svc.logger.Warn("startup metadata backfill check failed", "error", err)
			}
		}()
	}

	var periodicCh <-chan time.Time
	if w.interval > 0 {
		t := time.NewTicker(w.interval)
		defer t.Stop()
		periodicCh = t.C
	}

	var eventsCh <-chan fsnotify.Event
	var errsCh <-chan error
	if fsw != nil {
		eventsCh = fsw.Events
		errsCh = fsw.Errors
	}

	for {
		select {
		case <-ctx.Done():
			w.stopTimers()
			return

		case <-w.syncCh:
			if fsw != nil {
				w.syncWatches(ctx)
			}

		case <-periodicCh:
			w.scanAll(ctx)

		case ev, ok := <-eventsCh:
			if !ok {
				return
			}
			if ev.Op&(fsnotify.Create|fsnotify.Write|fsnotify.Remove|fsnotify.Rename) == 0 {
				continue
			}
			libID := w.libraryForPath(ev.Name)
			if libID == "" {
				continue
			}
			// If a new subdirectory was created (e.g. a new movie or season folder), watch it too.
			if ev.Op&fsnotify.Create != 0 {
				w.watchSubtree(ev.Name, libID)
			}
			w.scheduleScan(ctx, libID)

		case err, ok := <-errsCh:
			if !ok {
				return
			}
			w.svc.logger.Warn("library watcher error", "error", err)
		}
	}
}

func (w *Watcher) syncWatches(ctx context.Context) {
	libs, err := w.svc.ListLibraries(ctx)
	if err != nil {
		return
	}
	for _, lib := range libs {
		w.watchSubtree(lib.RootPath, lib.ID)
	}
}

func (w *Watcher) watchSubtree(root, libID string) {
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return nil
		}
		name := d.Name()
		if name != "." && (strings.HasPrefix(name, ".") || name == "@eaDir" || name == "#recycle") {
			return filepath.SkipDir
		}
		w.mu.Lock()
		defer w.mu.Unlock()
		if _, exists := w.dirLib[path]; !exists && w.fsw != nil {
			if err := w.fsw.Add(path); err == nil {
				w.dirLib[path] = libID
			}
		}
		return nil
	})
}

func (w *Watcher) libraryForPath(path string) string {
	w.mu.Lock()
	defer w.mu.Unlock()

	dir := path
	for {
		if libID, ok := w.dirLib[dir]; ok {
			return libID
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

func (w *Watcher) scheduleScan(ctx context.Context, libID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if t, ok := w.timers[libID]; ok {
		t.Reset(w.debounce)
		return
	}
	w.timers[libID] = time.AfterFunc(w.debounce, func() {
		w.mu.Lock()
		delete(w.timers, libID)
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		w.svc.logger.Info("auto-scanning library after filesystem change", "library_id", libID)
		_, _ = w.svc.StartScanAsync(ctx, libID, false)
	})
}

func (w *Watcher) scanAll(ctx context.Context) {
	libs, err := w.svc.ListLibraries(ctx)
	if err != nil {
		return
	}
	for _, lib := range libs {
		_, _ = w.svc.StartScanAsync(ctx, lib.ID, false)
	}
}

func (w *Watcher) stopTimers() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for id, t := range w.timers {
		t.Stop()
		delete(w.timers, id)
	}
}
