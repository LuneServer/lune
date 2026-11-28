package library

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

var (
	ErrNotFound      = errors.New("library or item not found")
	ErrAlreadyExists = errors.New("library path already registered")
	ErrInvalidPath   = errors.New("invalid library root path")
	ErrScanRunning   = errors.New("library scan already in progress")
)

// ScanStatus tracks live or last-completed scan metrics for a library.
type ScanStatus struct {
	LibraryID string `json:"library_id"`
	Running   bool   `json:"running"`
	// Scanned counts candidate files fully handled so far (unchanged files skipped, plus
	// new/modified files once probed or failed). Scanned/Total is the scan progress.
	Scanned int `json:"scanned"`
	// Total is the number of candidate media files found for this scan. It is 0 until the
	// directory walk finishes, so clients should treat 0 on a running scan as indeterminate.
	Total     int    `json:"total"`
	Added     int    `json:"added"`
	Updated   int    `json:"updated"`
	Removed   int    `json:"removed"`
	Skipped   int    `json:"skipped"`
	Errors    int    `json:"errors"`
	LastError string `json:"last_error,omitempty"`
	// ErrorSamples holds the first few distinct failures (path + reason) so admins can see why a scan reported errors.
	ErrorSamples []ScanError `json:"error_samples,omitempty"`
	StartedAt    *time.Time  `json:"started_at,omitempty"`
	FinishedAt   *time.Time  `json:"finished_at,omitempty"`
}

// ScanError describes a single file (or batch) that failed to probe or index.
type ScanError struct {
	Path    string `json:"path,omitempty"`
	Message string `json:"message"`
}

const (
	maxScanErrorSamples = 10
	// CurrentMetadataVersion is bumped whenever parser, scanner, or enricher logic changes
	// (e.g. credits, cast, content rating, release date, IMDb IDs, or Sonarr/TV year & group parsing)
	// so incremental scans and startup backfill automatically upgrade previously indexed items.
	// v4: TMDB episode names replace filename-derived episode titles, and fallback titles drop
	// release junk and leading episode numbers.
	CurrentMetadataVersion = 4
)

// Service coordinates media library configuration, incremental scanning, and catalog queries.
type Service struct {
	store    *store.Store
	prober   metadata.Prober
	enricher metadata.Enricher
	watcher  *Watcher
	logger   *slog.Logger

	mu          sync.Mutex
	scans       map[string]*ScanStatus
	warnedNoFFP bool
}

// NewService creates a library & scanning service.
func NewService(s *store.Store, prober metadata.Prober, logger *slog.Logger) *Service {
	if prober == nil {
		prober = metadata.NewFFProbe()
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:  s,
		prober: prober,
		logger: logger,
		scans:  make(map[string]*ScanStatus),
	}
}

// SetEnricher attaches an optional remote metadata enricher (e.g. TMDB).
func (s *Service) SetEnricher(e metadata.Enricher) {
	s.enricher = e
}

// HasEnricher reports whether a remote metadata enricher is configured.
func (s *Service) HasEnricher() bool {
	return s.enricher != nil
}

// BackfillStaleLibraries scans enrichable libraries that contain items indexed under an older
// metadata schema version (or before an enricher was configured).
func (s *Service) BackfillStaleLibraries(ctx context.Context) error {
	if s.enricher == nil {
		return nil
	}
	libIDs, err := s.store.ListLibrariesNeedingMetadataBackfill(ctx, CurrentMetadataVersion)
	if err != nil {
		return err
	}
	for _, libID := range libIDs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		s.logger.Info("backfilling rich metadata for library", "library_id", libID, "target_version", CurrentMetadataVersion)
		if _, err := s.ScanSync(ctx, libID, false); err != nil && !errors.Is(err, ErrScanRunning) {
			s.logger.Warn("metadata backfill scan failed", "library_id", libID, "error", err)
		}
	}
	return nil
}

// SetWatcher binds the background filesystem watcher so library changes refresh watched paths.
func (s *Service) SetWatcher(w *Watcher) {
	s.watcher = w
}

func (s *Service) CreateLibrary(ctx context.Context, name, kind, rootPath string) (store.Library, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return store.Library{}, fmt.Errorf("%w: name is required", ErrInvalidPath)
	}
	absPath, err := filepath.Abs(strings.TrimSpace(rootPath))
	if err != nil {
		return store.Library{}, fmt.Errorf("%w: %v", ErrInvalidPath, err)
	}
	info, err := os.Stat(absPath)
	if err != nil || !info.IsDir() {
		return store.Library{}, fmt.Errorf("%w: directory does not exist: %s", ErrInvalidPath, absPath)
	}

	lib := store.Library{
		ID:       uuid.NewString(),
		Name:     name,
		Kind:     kind,
		RootPath: absPath,
	}
	if err := s.store.CreateLibrary(ctx, lib); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return store.Library{}, ErrAlreadyExists
		}
		return store.Library{}, err
	}
	s.watcher.NotifyLibrariesChanged()
	return s.store.GetLibrary(ctx, lib.ID)
}

func (s *Service) ListLibraries(ctx context.Context) ([]store.Library, error) {
	return s.store.ListLibraries(ctx)
}

func (s *Service) ListLibrariesFiltered(ctx context.Context, allowedIDs []string, restrict bool) ([]store.Library, error) {
	return s.store.ListLibrariesFiltered(ctx, allowedIDs, restrict)
}

func (s *Service) GetLibrary(ctx context.Context, id string) (store.Library, error) {
	lib, err := s.store.GetLibrary(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.Library{}, ErrNotFound
	}
	return lib, err
}

func (s *Service) DeleteLibrary(ctx context.Context, id string) error {
	err := s.store.DeleteLibrary(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return ErrNotFound
	}
	if err == nil {
		s.mu.Lock()
		if st, ok := s.scans[id]; ok && !st.Running {
			delete(s.scans, id)
		}
		s.mu.Unlock()
	}
	s.watcher.NotifyLibrariesChanged()
	return err
}

func (s *Service) ListItems(ctx context.Context, f store.MediaListFilter) ([]store.MediaItem, int, error) {
	return s.store.ListMediaItems(ctx, f)
}

func (s *Service) GetLibraryFacets(ctx context.Context, libraryID string) (store.LibraryFacets, error) {
	return s.store.GetLibraryFacets(ctx, libraryID)
}

func (s *Service) GetItem(ctx context.Context, id string) (store.MediaItem, error) {
	item, err := s.store.GetMediaItem(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return store.MediaItem{}, ErrNotFound
	}
	return item, err
}

// GetPersonPhoto lazily fetches and caches a cast/crew headshot via the configured enricher.
func (s *Service) GetPersonPhoto(ctx context.Context, filename string) (string, error) {
	fetcher, ok := s.enricher.(metadata.PersonArtFetcher)
	if !ok || fetcher == nil {
		return "", ErrNotFound
	}
	clean := strings.TrimPrefix(strings.TrimSpace(filename), "/")
	if clean == "" || strings.Contains(clean, "/") || strings.Contains(clean, "\\") || strings.Contains(clean, "..") {
		return "", ErrNotFound
	}
	indexed, err := s.store.HasIndexedPersonPhoto(ctx, clean)
	if err != nil {
		return "", err
	}
	if !indexed {
		return "", ErrNotFound
	}
	p, err := fetcher.FetchPersonPhoto(ctx, "/"+clean)
	if err != nil {
		if errors.Is(err, metadata.ErrArtworkNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return p, nil
}

// ResolveArtwork ensures a media item's poster or backdrop file exists on disk,
// lazily fetching sharded TMDB artwork (such as episode stills or backdrops) on first access.
func (s *Service) ResolveArtwork(ctx context.Context, artPath string) (string, error) {
	if artPath == "" {
		return "", ErrNotFound
	}
	if info, err := os.Stat(artPath); err == nil && !info.IsDir() && info.Size() > 0 {
		return artPath, nil
	}
	fetcher, ok := s.enricher.(metadata.LazyArtFetcher)
	if !ok || fetcher == nil {
		return "", ErrNotFound
	}
	p, err := fetcher.EnsureArtwork(ctx, artPath)
	if err != nil {
		if errors.Is(err, metadata.ErrArtworkNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	return p, nil
}

// GetScanStatus returns the current or most recent scan state for a library.
func (s *Service) GetScanStatus(libraryID string) ScanStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st, ok := s.scans[libraryID]; ok {
		cp := *st
		if len(st.ErrorSamples) > 0 {
			cp.ErrorSamples = append([]ScanError(nil), st.ErrorSamples...)
		}
		return cp
	}
	return ScanStatus{LibraryID: libraryID}
}

// HasRunningScan reports whether any library currently has an active scan in progress.
func (s *Service) HasRunningScan() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.scans {
		if st.Running {
			return true
		}
	}
	return false
}

// StartScanAsync launches a background library scan.
func (s *Service) StartScanAsync(ctx context.Context, libraryID string, force bool) (ScanStatus, error) {
	lib, err := s.GetLibrary(ctx, libraryID)
	if err != nil {
		return ScanStatus{}, err
	}
	st, err := s.beginScan(lib.ID)
	if err != nil {
		return ScanStatus{}, err
	}
	go func() {
		bgCtx := context.WithoutCancel(ctx)
		_ = s.runScan(bgCtx, lib, force)
	}()
	return st, nil
}

// ScanSync executes a library scan synchronously and returns the final metrics.
func (s *Service) ScanSync(ctx context.Context, libraryID string, force bool) (ScanStatus, error) {
	// Detach from the request context: a browser navigating away (or a proxy timeout) must not
	// cancel the scan midway and fail in-flight DB batches.
	ctx = context.WithoutCancel(ctx)
	lib, err := s.GetLibrary(ctx, libraryID)
	if err != nil {
		return ScanStatus{}, err
	}
	if _, err := s.beginScan(lib.ID); err != nil {
		return ScanStatus{}, err
	}
	err = s.runScan(ctx, lib, force)
	return s.GetScanStatus(lib.ID), err
}

func (s *Service) beginScan(libraryID string) (ScanStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.scans[libraryID]; ok && cur.Running {
		return ScanStatus{}, ErrScanRunning
	}
	now := time.Now().UTC()
	st := &ScanStatus{
		LibraryID: libraryID,
		Running:   true,
		StartedAt: &now,
	}
	s.scans[libraryID] = st
	return *st, nil
}

type discoveredFile struct {
	path           string
	size           int64
	mtime          int64
	isNew          bool
	id             string
	existingTMDBID string
}

func (s *Service) runScan(ctx context.Context, lib store.Library, force bool) error {
	defer func() {
		s.mu.Lock()
		if st, ok := s.scans[lib.ID]; ok {
			now := time.Now().UTC()
			st.Running = false
			st.FinishedAt = &now
		}
		s.mu.Unlock()
	}()

	existing, err := s.store.ListLibraryFingerprints(ctx, lib.ID)
	if err != nil {
		s.recordScanErr(lib.ID, err)
		return err
	}

	seen := make(map[string]bool, len(existing))
	var toProcess []discoveredFile

	walkErr := filepath.WalkDir(lib.RootPath, func(path string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			s.recordItemErr(lib.ID, path, fmt.Errorf("walk: %w", err), 1)
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && (strings.HasPrefix(name, ".") || name == "@eaDir" || name == "#recycle") {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsSupportedMedia(path, lib.Kind) {
			return nil
		}

		info, statErr := d.Info()
		if statErr != nil {
			s.recordItemErr(lib.ID, path, fmt.Errorf("stat: %w", statErr), 1)
			return nil
		}

		seen[path] = true
		mtime := info.ModTime().UnixNano()
		if fp, ok := existing[path]; ok {
			needsBackfill := s.enricher != nil && fp.MetadataVersion < CurrentMetadataVersion && isEnrichableLibraryKind(lib.Kind)
			if !force && fp.FileSize == info.Size() && fp.FileMtime == mtime && !needsBackfill {
				s.incScan(lib.ID, func(st *ScanStatus) { st.Skipped++; st.Scanned++ })
				return nil
			}
			toProcess = append(toProcess, discoveredFile{
				path: path, size: info.Size(), mtime: mtime, isNew: false, id: fp.ID, existingTMDBID: fp.TMDBID,
			})
			return nil
		}

		toProcess = append(toProcess, discoveredFile{
			path: path, size: info.Size(), mtime: mtime, isNew: true, id: uuid.NewString(),
		})
		return nil
	})
	if walkErr != nil {
		s.recordScanErr(lib.ID, walkErr)
		return walkErr
	}

	// The walk is done: every candidate is now known, so progress (Scanned/Total) is computable.
	// Unchanged files are already counted in Scanned; changed ones advance it as they finish.
	total := len(seen)
	s.incScan(lib.ID, func(st *ScanStatus) { st.Total = total })

	// Probe new or modified files with bounded concurrency and flush DB writes in batches
	// so large scans (10k–100k files) minimize SQLite write lock hold times and fsyncs.
	const (
		workers   = 4
		batchSize = 100
	)
	type scannedItem struct {
		item  store.MediaItem
		isNew bool
	}

	jobs := make(chan discoveredFile)
	results := make(chan scannedItem, batchSize)

	var writerWG sync.WaitGroup
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		batch := make([]store.MediaItem, 0, batchSize)
		addedInBatch := 0
		updatedInBatch := 0
		flush := func() {
			if len(batch) == 0 {
				return
			}
			if err := s.store.UpsertMediaItemsBatch(ctx, batch); err != nil {
				s.logger.Warn("failed to save media item batch", "count", len(batch), "error", err)
				first := ""
				if len(batch) > 0 {
					first = batch[0].FilePath
				}
				s.recordItemErr(lib.ID, first, fmt.Errorf("failed to save batch of %d item(s): %w", len(batch), err), len(batch))
			} else {
				s.markRequestsAvailableForItems(ctx, batch)
				a, u := addedInBatch, updatedInBatch
				s.incScan(lib.ID, func(st *ScanStatus) {
					st.Added += a
					st.Updated += u
				})
			}
			batch = batch[:0]
			addedInBatch = 0
			updatedInBatch = 0
		}

		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case res, ok := <-results:
				if !ok {
					flush()
					return
				}
				batch = append(batch, res.item)
				if res.isNew {
					addedInBatch++
				} else {
					updatedInBatch++
				}
				if len(batch) >= batchSize {
					flush()
				}
			case <-ticker.C:
				flush()
			}
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range jobs {
				if ctx.Err() != nil {
					return
				}
				item, ok := s.buildScannedItem(ctx, lib, job)
				s.incScan(lib.ID, func(st *ScanStatus) { st.Scanned++ })
				if ok {
					results <- scannedItem{item: item, isNew: job.isNew}
				}
			}
		}()
	}
	for _, job := range toProcess {
		jobs <- job
	}
	close(jobs)
	wg.Wait()
	close(results)
	writerWG.Wait()

	// Prune files removed from disk since the last scan.
	var staleIDs []string
	for path, fp := range existing {
		if !seen[path] {
			staleIDs = append(staleIDs, fp.ID)
		}
	}
	if len(staleIDs) > 0 {
		removed, err := s.store.DeleteMediaItemsByIDs(ctx, staleIDs)
		if err != nil {
			s.recordScanErr(lib.ID, err)
			return err
		}
		s.incScan(lib.ID, func(st *ScanStatus) { st.Removed += int(removed) })
	}

	// Merge differently spelled series titles of one show (same TMDB series, or the same
	// normalised title) so episodes from mixed releases group into a single series.
	if renamed, err := s.store.CanonicalizeSeriesTitles(ctx, lib.ID); err != nil {
		s.logger.Warn("failed to merge series titles", "library", lib.Name, "error", err)
	} else if renamed > 0 {
		s.logger.Info("merged series title variants", "library", lib.Name, "episodes", renamed)
	}

	st := s.GetScanStatus(lib.ID)
	if st.Added+st.Updated+st.Removed >= 50 {
		_ = s.store.Optimize(ctx)
	}
	s.logger.Info("library scan completed",
		"library", lib.Name,
		"scanned", st.Scanned,
		"added", st.Added,
		"updated", st.Updated,
		"skipped", st.Skipped,
		"removed", st.Removed,
		"errors", st.Errors,
	)
	return nil
}

func (s *Service) buildScannedItem(ctx context.Context, lib store.Library, job discoveredFile) (store.MediaItem, bool) {
	parsed := ParseMediaPath(lib.RootPath, job.path, lib.Kind)

	probeRes, probeErr := s.prober.Probe(ctx, job.path)
	var probedAt *time.Time
	if probeErr == nil {
		now := time.Now().UTC()
		probedAt = &now
	} else if errors.Is(probeErr, metadata.ErrProberUnavailable) {
		s.mu.Lock()
		if !s.warnedNoFFP {
			s.warnedNoFFP = true
			s.logger.Warn("ffprobe not found on PATH; indexing media files without codec inspection")
		}
		s.mu.Unlock()
	} else {
		s.logger.Warn("failed to probe media file", "path", job.path, "error", probeErr)
		s.recordItemErr(lib.ID, job.path, fmt.Errorf("probe: %w", probeErr), 1)
	}

	streamsJSON := "[]"
	if len(probeRes.Streams) > 0 {
		if b, err := json.Marshal(probeRes.Streams); err == nil {
			streamsJSON = string(b)
		}
	}
	chaptersJSON := "[]"
	if len(probeRes.Chapters) > 0 {
		if b, err := json.Marshal(probeRes.Chapters); err == nil {
			chaptersJSON = string(b)
		}
	}
	audioLanguages := strings.Join(metadata.ExtractAudioLanguages(probeRes.Streams), ",")
	subtitlesJSON := "[]"
	if subs := DiscoverSubtitles(job.path); len(subs) > 0 {
		if b, err := json.Marshal(subs); err == nil {
			subtitlesJSON = string(b)
		}
	}
	container := probeRes.Container
	if container == "" {
		container = strings.TrimPrefix(strings.ToLower(filepath.Ext(job.path)), ".")
	}

	item := store.MediaItem{
		ID:                    job.id,
		LibraryID:             lib.ID,
		Kind:                  parsed.Kind,
		Title:                 parsed.Title,
		SortTitle:             parsed.SortTitle,
		Year:                  parsed.Year,
		SeriesTitle:           parsed.SeriesTitle,
		SeasonNumber:          parsed.SeasonNumber,
		EpisodeNumber:         parsed.EpisodeNumber,
		AbsoluteEpisodeNumber: parsed.AbsoluteEpisodeNumber,
		ReleaseGroup:          parsed.ReleaseGroup,
		FilePath:              job.path,
		FileSize:              job.size,
		FileMtime:             job.mtime,
		Container:             container,
		DurationMS:            probeRes.DurationMS,
		Bitrate:               probeRes.Bitrate,
		VideoCodec:            probeRes.VideoCodec,
		VideoWidth:            probeRes.VideoWidth,
		VideoHeight:           probeRes.VideoHeight,
		VideoHDR:              probeRes.VideoHDR,
		AudioCodec:            probeRes.AudioCodec,
		AudioChannels:         probeRes.AudioChannels,
		AudioLanguages:        audioLanguages,
		StreamsJSON:           streamsJSON,
		SubtitlesJSON:         subtitlesJSON,
		ChaptersJSON:          chaptersJSON,
		PosterPath:            parsed.PosterPath,
		BackdropPath:          parsed.BackdropPath,
		TMDBID:                parsed.TMDBID,
		IMDBID:                parsed.IMDBID,
		GenresJSON:            "[]",
		DirectorsJSON:         "[]",
		WritersJSON:           "[]",
		StudiosJSON:           "[]",
		CastJSON:              "[]",
		ProbedAt:              probedAt,
	}

	// 1. Offline local NFO sidecar enrichment (Kodi/Radarr/Sonarr XML, including parent tvshow.nfo for episodes).
	titleFromNFO := false
	if nfo, ok := metadata.ParseLocalNFOForKind(lib.RootPath, job.path, item.Kind); ok {
		if nfo.Title != "" {
			titleFromNFO = true
			item.Title = nfo.Title
			item.SortTitle = makeSortTitle(nfo.Title)
		}
		if nfo.SeriesTitle != "" && item.Kind == "episode" && item.SeriesTitle == "" {
			item.SeriesTitle = nfo.SeriesTitle
		}
		if nfo.OriginalTitle != "" {
			item.OriginalTitle = nfo.OriginalTitle
		}
		if nfo.Year != nil {
			item.Year = nfo.Year
		}
		item.Overview = nfo.Overview
		item.Tagline = nfo.Tagline
		item.Rating = nfo.Rating
		item.ContentRating = nfo.ContentRating
		item.ReleaseDate = nfo.ReleaseDate
		if item.Kind == "episode" {
			item.EpisodeTMDBID, item.EpisodeIMDBID = nfo.EpisodeTMDBID, nfo.EpisodeIMDBID
		}
		if item.TMDBID == "" && nfo.TMDBID != "" {
			item.TMDBID = nfo.TMDBID
		}
		if item.IMDBID == "" && nfo.IMDBID != "" {
			item.IMDBID = nfo.IMDBID
		}
		if len(nfo.Genres) > 0 {
			if gb, err := json.Marshal(nfo.Genres); err == nil {
				item.GenresJSON = string(gb)
			}
		}
		if len(nfo.Directors) > 0 {
			if b, err := json.Marshal(nfo.Directors); err == nil {
				item.DirectorsJSON = string(b)
			}
		}
		if len(nfo.Writers) > 0 {
			if b, err := json.Marshal(nfo.Writers); err == nil {
				item.WritersJSON = string(b)
			}
		}
		if len(nfo.Studios) > 0 {
			if b, err := json.Marshal(nfo.Studios); err == nil {
				item.StudiosJSON = string(b)
			}
		}
		if len(nfo.Cast) > 0 {
			if b, err := json.Marshal(nfo.Cast); err == nil {
				item.CastJSON = string(b)
			}
		}
	}

	// Preserve an existing TMDB binding unless the path or NFO supplies a TMDB ID.
	// An IMDb ID alone cannot select a match in the lookups below.
	if item.TMDBID == "" {
		item.TMDBID = job.existingTMDBID
	}

	// 2. Optional remote enricher (TMDB) when configured.
	if item.Kind != "movie" && item.Kind != "episode" {
		item.MetadataVersion = CurrentMetadataVersion
	} else if s.enricher != nil {
		if item.Overview == "" || item.PosterPath == "" || item.TMDBID == "" || item.CastJSON == "[]" {
			var (
				remote metadata.EnrichedInfo
				err    error
			)
			animeEnricher, hasAnime := s.enricher.(metadata.AnimeEnricher)
			searchEnricher, hasSearch := s.enricher.(metadata.SearchEnricher)
			switch item.Kind {
			case "movie":
				if item.TMDBID != "" && hasSearch {
					remote, err = searchEnricher.EnrichMovieByTMDBID(ctx, item.ID, item.TMDBID)
				} else if lib.Kind == "anime" && hasAnime {
					remote, err = animeEnricher.EnrichAnimeMovie(ctx, item.ID, item.Title, item.Year)
				} else {
					remote, err = s.enricher.EnrichMovie(ctx, item.ID, item.Title, item.Year)
				}
			case "episode":
				season, episode, absEp := 0, 0, 0
				if item.SeasonNumber != nil {
					season = *item.SeasonNumber
				}
				if item.EpisodeNumber != nil {
					episode = *item.EpisodeNumber
				}
				if item.AbsoluteEpisodeNumber != nil {
					absEp = *item.AbsoluteEpisodeNumber
				}
				// For episodes, the path, series NFO, and retained binding supply the series ID.
				if item.TMDBID != "" && hasSearch {
					remote, err = searchEnricher.EnrichEpisodeByTMDBID(ctx, item.ID, item.TMDBID, season, episode, absEp, lib.Kind == "anime")
				} else if lib.Kind == "anime" && hasAnime {
					remote, err = animeEnricher.EnrichAnimeEpisode(ctx, item.ID, item.SeriesTitle, season, episode, absEp, item.Year)
				} else {
					remote, err = s.enricher.EnrichEpisode(ctx, item.ID, item.SeriesTitle, season, episode, item.Year)
				}
			}
			if err == nil {
				item.MetadataVersion = CurrentMetadataVersion
				if item.Kind == "episode" && preferRemoteEpisodeTitle(item.Title, remote.Title, titleFromNFO) {
					item.Title = remote.Title
				}
				if item.OriginalTitle == "" && remote.OriginalTitle != "" {
					item.OriginalTitle = remote.OriginalTitle
				}
				if remote.MappedSeason != nil && remote.MappedEpisode != nil {
					item.SeasonNumber = remote.MappedSeason
					item.EpisodeNumber = remote.MappedEpisode
				}
				if item.Overview == "" && remote.Overview != "" {
					item.Overview = remote.Overview
				}
				if item.Tagline == "" && remote.Tagline != "" {
					item.Tagline = remote.Tagline
				}
				if item.Rating == 0 && remote.Rating > 0 {
					item.Rating = remote.Rating
				}
				if item.DurationMS == 0 && remote.RuntimeMS > 0 {
					item.DurationMS = remote.RuntimeMS
				}
				if item.TMDBID == "" && remote.TMDBID != "" {
					item.TMDBID = remote.TMDBID
				}
				if item.IMDBID == "" && remote.IMDBID != "" {
					item.IMDBID = remote.IMDBID
				}
				if item.GenresJSON == "[]" && len(remote.Genres) > 0 {
					if gb, mErr := json.Marshal(remote.Genres); mErr == nil {
						item.GenresJSON = string(gb)
					}
				}
				if item.ContentRating == "" && remote.ContentRating != "" {
					item.ContentRating = remote.ContentRating
				}
				if item.ReleaseDate == "" && remote.ReleaseDate != "" {
					item.ReleaseDate = remote.ReleaseDate
				}
				if item.DirectorsJSON == "[]" && len(remote.Directors) > 0 {
					if b, mErr := json.Marshal(remote.Directors); mErr == nil {
						item.DirectorsJSON = string(b)
					}
				}
				if item.WritersJSON == "[]" && len(remote.Writers) > 0 {
					if b, mErr := json.Marshal(remote.Writers); mErr == nil {
						item.WritersJSON = string(b)
					}
				}
				if item.StudiosJSON == "[]" && len(remote.Studios) > 0 {
					if b, mErr := json.Marshal(remote.Studios); mErr == nil {
						item.StudiosJSON = string(b)
					}
				}
				if item.CastJSON == "[]" && len(remote.Cast) > 0 {
					if b, mErr := json.Marshal(remote.Cast); mErr == nil {
						item.CastJSON = string(b)
					}
				}
				if item.PosterPath == "" && remote.PosterPath != "" {
					item.PosterPath = remote.PosterPath
				}
				if item.BackdropPath == "" && remote.BackdropPath != "" {
					item.BackdropPath = remote.BackdropPath
				}
			} else {
				s.logger.Warn("remote metadata enrichment failed", "title", item.Title, "error", err)
			}
		} else {
			item.MetadataVersion = CurrentMetadataVersion
		}
	}

	return item, true
}

func (s *Service) incScan(libraryID string, fn func(*ScanStatus)) {
	s.mu.Lock()
	if st, ok := s.scans[libraryID]; ok {
		fn(st)
	}
	s.mu.Unlock()
}

func (s *Service) recordScanErr(libraryID string, err error) {
	s.recordItemErr(libraryID, "", err, 1)
}

// recordItemErr increments the error counter by n, remembers the latest reason, and keeps
// a bounded list of samples (path + message) that the admin UI surfaces.
func (s *Service) recordItemErr(libraryID, path string, err error, n int) {
	if n < 1 {
		n = 1
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.scans[libraryID]
	if !ok {
		return
	}
	st.Errors += n
	st.LastError = err.Error()
	if len(st.ErrorSamples) < maxScanErrorSamples {
		st.ErrorSamples = append(st.ErrorSamples, ScanError{Path: path, Message: err.Error()})
	}
}

// SearchMetadata queries the configured remote enricher (TMDB) for selectable movie or TV candidates.
func (s *Service) SearchMetadata(ctx context.Context, kind, query string, year *int) ([]metadata.MetadataSearchResult, bool, error) {
	se, ok := s.enricher.(metadata.SearchEnricher)
	if !ok || se == nil {
		return []metadata.MetadataSearchResult{}, false, nil
	}
	results, err := se.SearchMetadata(ctx, kind, query, year)
	if err != nil {
		return nil, true, err
	}
	return results, true, nil
}

// RefreshItem re-probes, re-discovers subtitles/artwork, and re-enriches a single item.
// Optional overrideTitle / overrideYear / overrideTMDBID let admins manually re-match misidentified titles.
func (s *Service) RefreshItem(ctx context.Context, itemID, overrideTitle string, overrideYear *int, overrideTMDBID ...string) (store.MediaItem, error) {
	tmdbOverride := ""
	if len(overrideTMDBID) > 0 {
		tmdbOverride = strings.TrimSpace(overrideTMDBID[0])
	}
	item, err := s.GetItem(ctx, itemID)
	if err != nil {
		return store.MediaItem{}, err
	}
	lib, err := s.GetLibrary(ctx, item.LibraryID)
	if err != nil {
		return store.MediaItem{}, err
	}

	info, statErr := os.Stat(item.FilePath)
	if statErr == nil {
		item.FileSize = info.Size()
		item.FileMtime = info.ModTime().UnixNano()
	}

	if subs := DiscoverSubtitles(item.FilePath); len(subs) > 0 {
		if b, err := json.Marshal(subs); err == nil {
			item.SubtitlesJSON = string(b)
		}
	} else {
		item.SubtitlesJSON = "[]"
	}

	parsed := ParseMediaPath(lib.RootPath, item.FilePath, lib.Kind)
	if parsed.PosterPath != "" {
		item.PosterPath = parsed.PosterPath
	}
	if parsed.BackdropPath != "" {
		item.BackdropPath = parsed.BackdropPath
	}

	if probeRes, pErr := s.prober.Probe(ctx, item.FilePath); pErr == nil {
		now := time.Now().UTC()
		item.ProbedAt = &now
		item.DurationMS = probeRes.DurationMS
		item.Bitrate = probeRes.Bitrate
		item.VideoCodec = probeRes.VideoCodec
		item.VideoWidth = probeRes.VideoWidth
		item.VideoHeight = probeRes.VideoHeight
		item.VideoHDR = probeRes.VideoHDR
		item.AudioCodec = probeRes.AudioCodec
		item.AudioChannels = probeRes.AudioChannels
		item.AudioLanguages = strings.Join(metadata.ExtractAudioLanguages(probeRes.Streams), ",")
		if b, err := json.Marshal(probeRes.Streams); err == nil {
			item.StreamsJSON = string(b)
		}
		if b, err := json.Marshal(probeRes.Chapters); err == nil {
			item.ChaptersJSON = string(b)
		}
	}

	queryTitle := item.Title
	if strings.TrimSpace(overrideTitle) != "" {
		queryTitle = strings.TrimSpace(overrideTitle)
		item.Title = queryTitle
		item.SortTitle = makeSortTitle(queryTitle)
	}
	queryYear := item.Year
	if overrideYear != nil && *overrideYear > 0 {
		queryYear = overrideYear
		item.Year = overrideYear
	}

	if s.enricher != nil {
		var (
			remote metadata.EnrichedInfo
			eErr   error
		)
		animeEnricher, hasAnime := s.enricher.(metadata.AnimeEnricher)
		searchEnricher, hasSearch := s.enricher.(metadata.SearchEnricher)
		if item.Kind == "episode" {
			seriesQuery := item.SeriesTitle
			if strings.TrimSpace(overrideTitle) != "" {
				seriesQuery = strings.TrimSpace(overrideTitle)
				item.SeriesTitle = seriesQuery
			}
			season, episode, absEp := 0, 0, 0
			if item.SeasonNumber != nil {
				season = *item.SeasonNumber
			}
			if item.EpisodeNumber != nil {
				episode = *item.EpisodeNumber
			}
			if item.AbsoluteEpisodeNumber != nil {
				absEp = *item.AbsoluteEpisodeNumber
			}
			if tmdbOverride != "" && hasSearch {
				remote, eErr = searchEnricher.EnrichEpisodeByTMDBID(ctx, item.ID, tmdbOverride, season, episode, absEp, lib.Kind == "anime")
			} else if lib.Kind == "anime" && hasAnime {
				remote, eErr = animeEnricher.EnrichAnimeEpisode(ctx, item.ID, seriesQuery, season, episode, absEp, queryYear)
			} else {
				remote, eErr = s.enricher.EnrichEpisode(ctx, item.ID, seriesQuery, season, episode, queryYear)
			}
		} else {
			if tmdbOverride != "" && hasSearch {
				remote, eErr = searchEnricher.EnrichMovieByTMDBID(ctx, item.ID, tmdbOverride)
			} else if lib.Kind == "anime" && hasAnime {
				remote, eErr = animeEnricher.EnrichAnimeMovie(ctx, item.ID, queryTitle, queryYear)
			} else {
				remote, eErr = s.enricher.EnrichMovie(ctx, item.ID, queryTitle, queryYear)
			}
		}
		if eErr == nil {
			item.MetadataVersion = CurrentMetadataVersion
			if item.Kind == "episode" && tmdbOverride != "" && remote.SeriesTitle != "" {
				item.SeriesTitle = remote.SeriesTitle
			}
			if remote.Title != "" && (tmdbOverride != "" || item.Kind != "episode" || strings.TrimSpace(overrideTitle) == "" || isGenericEpisodeTitle(item.Title)) {
				item.Title = remote.Title
				item.SortTitle = makeSortTitle(remote.Title)
			}
			if remote.OriginalTitle != "" || tmdbOverride != "" {
				item.OriginalTitle = remote.OriginalTitle
			}
			if remote.MappedSeason != nil && remote.MappedEpisode != nil {
				item.SeasonNumber = remote.MappedSeason
				item.EpisodeNumber = remote.MappedEpisode
			}
			if remote.Overview != "" {
				item.Overview = remote.Overview
			}
			if remote.Tagline != "" {
				item.Tagline = remote.Tagline
			}
			if remote.Rating > 0 {
				item.Rating = remote.Rating
			}
			if item.DurationMS == 0 && remote.RuntimeMS > 0 {
				item.DurationMS = remote.RuntimeMS
			}
			if remote.Year != nil {
				item.Year = remote.Year
			}
			if remote.TMDBID != "" {
				item.TMDBID = remote.TMDBID
			}
			if remote.IMDBID != "" {
				item.IMDBID = remote.IMDBID
			}
			if len(remote.Genres) > 0 {
				if gb, err := json.Marshal(remote.Genres); err == nil {
					item.GenresJSON = string(gb)
				}
			}
			if remote.ContentRating != "" {
				item.ContentRating = remote.ContentRating
			}
			if remote.ReleaseDate != "" {
				item.ReleaseDate = remote.ReleaseDate
			}
			if len(remote.Directors) > 0 {
				if b, err := json.Marshal(remote.Directors); err == nil {
					item.DirectorsJSON = string(b)
				}
			}
			if len(remote.Writers) > 0 {
				if b, err := json.Marshal(remote.Writers); err == nil {
					item.WritersJSON = string(b)
				}
			}
			if len(remote.Studios) > 0 {
				if b, err := json.Marshal(remote.Studios); err == nil {
					item.StudiosJSON = string(b)
				}
			}
			if len(remote.Cast) > 0 {
				if b, err := json.Marshal(remote.Cast); err == nil {
					item.CastJSON = string(b)
				}
			}
			if remote.PosterPath != "" {
				item.PosterPath = remote.PosterPath
			}
			if remote.BackdropPath != "" {
				item.BackdropPath = remote.BackdropPath
			}
		}
	}

	if err := s.store.UpsertMediaItem(ctx, item); err != nil {
		return store.MediaItem{}, err
	}
	s.markRequestsAvailableForItems(ctx, []store.MediaItem{item})
	return s.GetItem(ctx, item.ID)
}

func (s *Service) markRequestsAvailableForItems(ctx context.Context, items []store.MediaItem) {
	seen := make(map[string]struct{})
	for _, it := range items {
		if it.TMDBID == "" {
			continue
		}
		tmdbID, err := strconv.Atoi(strings.TrimSpace(it.TMDBID))
		if err != nil || tmdbID <= 0 {
			continue
		}
		mediaType := ""
		switch it.Kind {
		case "movie":
			mediaType = "movie"
		case "episode":
			mediaType = "tv"
		default:
			continue
		}
		key := mediaType + ":" + strconv.Itoa(tmdbID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		_, _ = s.store.MarkRequestsAvailableByTMDB(ctx, tmdbID, mediaType)
	}
}

func isEnrichableLibraryKind(kind string) bool {
	return kind == "movies" || kind == "shows" || kind == "anime"
}

var reGenericEpisodeTitle = regexp.MustCompile(`(?i)^(?:S\d{1,2}E\d{1,4}|(?:episode|ep\.?|e|#)?\s*\d{1,4}|(?:OVA|OAD|ONA|Special|SP|NCOP|NCED|PV)(?:\s*\d{1,3})?)$`)

// isGenericEpisodeTitle reports placeholder titles such as "S01E02", "Episode 02", "001", or
// "OVA 01" that carry no episode name.
func isGenericEpisodeTitle(title string) bool {
	t := strings.TrimSpace(title)
	return t == "" || reGenericEpisodeTitle.MatchString(t)
}

// preferRemoteEpisodeTitle decides whether a matched TMDB episode name replaces the title an
// episode got from its filename. TMDB wins over filename text, which is often release junk or
// a fansub's own translation; an explicit .nfo <title> is kept. A placeholder TMDB name such as
// "Episode 5" never replaces a real local title.
func preferRemoteEpisodeTitle(local, remote string, localFromNFO bool) bool {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		return false
	}
	if isGenericEpisodeTitle(local) {
		return true
	}
	if localFromNFO || isGenericEpisodeTitle(remote) {
		return false
	}
	return true
}
