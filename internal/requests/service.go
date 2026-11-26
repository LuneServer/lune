package requests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
)

var (
	ErrDisabled         = errors.New("media requests are disabled")
	ErrForbidden        = errors.New("insufficient permissions")
	ErrNotFound         = errors.New("request not found")
	ErrValidation       = errors.New("invalid request")
	ErrAlreadyInLibrary = errors.New("title is already in your library")
	ErrAlreadyRequested = errors.New("title already has an active request")
	ErrConflict         = errors.New("request cannot be modified in its current state")
)

const tmdbImageBase = "https://image.tmdb.org/t/p"

// TMDBDiscoverer abstracts TMDB discovery and detail lookups for requests.Service.
type TMDBDiscoverer interface {
	Discover(ctx context.Context, tab string, query string, page int, filter metadata.DiscoverFilter) ([]metadata.DiscoverItem, int, error)
	GetRequestTargetDetails(ctx context.Context, mediaType string, tmdbID int) (metadata.DiscoverItem, int, error)
}

// AnnotatedDiscoverItem enriches a TMDB DiscoverItem with local library & request status.
type AnnotatedDiscoverItem struct {
	MediaType     string  `json:"media_type"`
	TMDBID        int     `json:"tmdb_id"`
	Title         string  `json:"title"`
	OriginalTitle string  `json:"original_title,omitempty"`
	Year          *int    `json:"year,omitempty"`
	Overview      string  `json:"overview,omitempty"`
	PosterURL     string  `json:"poster_url,omitempty"`
	BackdropURL   string  `json:"backdrop_url,omitempty"`
	Rating        float64 `json:"rating,omitempty"`
	IsAnime       bool    `json:"is_anime"`
	InLibrary     bool    `json:"in_library"`
	LibraryItemID string  `json:"library_item_id,omitempty"`
	LibraryID     string  `json:"library_id,omitempty"`
	SeriesTitle   string  `json:"library_series_title,omitempty"`
	RequestID     string  `json:"request_id,omitempty"`
	RequestStatus string  `json:"request_status,omitempty"`
}

// DiscoverResult is the paginated response returned by Service.Discover.
type DiscoverResult struct {
	Items      []AnnotatedDiscoverItem `json:"results"`
	Page       int                     `json:"page"`
	TotalPages int                     `json:"total_pages"`
}

type cachedQueue struct {
	ids       map[int]bool
	fetchedAt time.Time
}

const queueCacheTTL = 15 * time.Second

// Service coordinates media discovery, request lifecycle, approval, and Radarr/Sonarr dispatch.
type Service struct {
	store      *store.Store
	envCfg     config.Config
	tmdb       TMDBDiscoverer
	arr        *ArrClient
	logger     *slog.Logger
	queueMu    sync.Mutex
	queueCache map[string]cachedQueue
}

// NewService constructs a Media Requests service.
func NewService(st *store.Store, envCfg config.Config, tmdb TMDBDiscoverer, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		store:      st,
		envCfg:     envCfg,
		tmdb:       tmdb,
		arr:        NewArrClient(nil),
		logger:     logger,
		queueCache: make(map[string]cachedQueue),
	}
}

// SetArrHTTPClient replaces the HTTP client used for Radarr/Sonarr calls (for testing).
func (s *Service) SetArrHTTPClient(client *http.Client) {
	if s != nil && client != nil {
		s.arr = NewArrClient(client)
	}
}

// IsEnabled reports whether media requests are enabled and TMDB is configured.
func (s *Service) IsEnabled(ctx context.Context) bool {
	if s == nil || s.tmdb == nil {
		return false
	}
	cfg, err := loadStoredConfig(ctx, s.store, s.envCfg, true)
	if err != nil {
		return false
	}
	return cfg.Enabled
}

// GetConfig returns the current RequestsConfig with API keys masked for admin display.
func (s *Service) GetConfig(ctx context.Context) (RequestsConfig, error) {
	cfg, err := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil)
	if err != nil {
		return RequestsConfig{}, err
	}
	return cfg.Masked(), nil
}

// SaveConfig validates and persists RequestsConfig, preserving existing API keys when omitted or masked.
func (s *Service) SaveConfig(ctx context.Context, actor store.User, incoming RequestsConfig, clientIP string) (RequestsConfig, error) {
	existing, err := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil)
	if err != nil {
		return RequestsConfig{}, err
	}

	next := RequestsConfig{
		Enabled:        incoming.Enabled,
		TMDBConfigured: s.tmdb != nil,
		Radarr:         normalizeArrSlot(incoming.Radarr, existing.Radarr, true, false),
		RadarrAnime:    normalizeArrSlot(incoming.RadarrAnime, existing.RadarrAnime, true, true),
		Sonarr:         normalizeArrSlot(incoming.Sonarr, existing.Sonarr, false, false),
		SonarrAnime:    normalizeArrSlot(incoming.SonarrAnime, existing.SonarrAnime, false, true),
	}

	b, err := json.Marshal(next)
	if err != nil {
		return RequestsConfig{}, err
	}
	if err := s.store.SetSetting(ctx, requestsConfigKey, string(b)); err != nil {
		return RequestsConfig{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.requests_config_updated",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("updated media requests config (enabled=%t)", next.Enabled),
	})

	// Reload through loadStoredConfig so any env overrides are reflected accurately.
	saved, err := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil)
	if err != nil {
		return next.Masked(), nil
	}
	return saved.Masked(), nil
}

// TestArrConnection tests a Radarr or Sonarr instance. If apiKey is empty or masked and slot is given,
// the saved API key for that slot is used only when baseURL matches the saved slot's origin.
func (s *Service) TestArrConnection(ctx context.Context, service, baseURL, apiKey, slot string) ArrTestResult {
	trimmedKey := strings.TrimSpace(apiKey)
	if trimmedKey == "" || trimmedKey == maskedAPIKeyValue {
		trimmedKey = ""
		if cfg, err := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil); err == nil {
			var savedSlot ArrInstanceConfig
			switch strings.ToLower(strings.TrimSpace(slot)) {
			case "radarr":
				savedSlot = cfg.Radarr
			case "radarr_anime":
				savedSlot = cfg.RadarrAnime
			case "sonarr":
				savedSlot = cfg.Sonarr
			case "sonarr_anime":
				savedSlot = cfg.SonarrAnime
			default:
				if strings.EqualFold(service, "radarr") {
					savedSlot = cfg.Radarr
				} else if strings.EqualFold(service, "sonarr") {
					savedSlot = cfg.Sonarr
				}
			}
			if strings.TrimSpace(baseURL) != "" && savedSlot.APIKey != "" {
				if !sameOrigin(baseURL, savedSlot.URL) {
					return ArrTestResult{
						OK:              false,
						Error:           "API key is required when changing the server URL",
						QualityProfiles: []QualityProfile{},
						RootFolders:     []RootFolder{},
					}
				}
				trimmedKey = savedSlot.APIKey
			}
		}
	}
	return s.arr.TestConnection(ctx, service, baseURL, trimmedKey)
}

// Discover fetches TMDB discovery/search results and annotates them with library presence and active requests.
func (s *Service) Discover(ctx context.Context, user store.User, tab, query string, page int, filter metadata.DiscoverFilter) (DiscoverResult, error) {
	if !s.IsEnabled(ctx) {
		return DiscoverResult{}, ErrDisabled
	}
	if user.Role != "admin" && !user.CanRequest {
		return DiscoverResult{}, ErrForbidden
	}
	if page < 1 {
		page = 1
	}

	rawItems, totalPages, err := s.tmdb.Discover(ctx, tab, query, page, filter)
	if err != nil {
		return DiscoverResult{}, err
	}

	var movieIDs, tvIDs []int
	for _, it := range rawItems {
		if it.MediaType == "movie" {
			movieIDs = append(movieIDs, it.TMDBID)
		} else if it.MediaType == "tv" {
			tvIDs = append(tvIDs, it.TMDBID)
		}
	}

	allowedLibs, unrestricted, err := s.store.AccessibleLibraryIDs(ctx, user)
	if err != nil {
		return DiscoverResult{}, err
	}
	restrict := !unrestricted

	localMovies, err := s.store.FindLocalMediaByTMDBAndType(ctx, "movie", movieIDs, allowedLibs, restrict)
	if err != nil {
		return DiscoverResult{}, err
	}
	localTV, err := s.store.FindLocalMediaByTMDBAndType(ctx, "tv", tvIDs, allowedLibs, restrict)
	if err != nil {
		return DiscoverResult{}, err
	}
	activeMovies, err := s.store.FindActiveRequestsByTMDB(ctx, "movie", movieIDs)
	if err != nil {
		return DiscoverResult{}, err
	}
	activeTV, err := s.store.FindActiveRequestsByTMDB(ctx, "tv", tvIDs)
	if err != nil {
		return DiscoverResult{}, err
	}
	var liveCfg *RequestsConfig
	if len(activeMovies) > 0 || len(activeTV) > 0 {
		if cfg, cfgErr := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil); cfgErr == nil {
			liveCfg = &cfg
		}
	}

	items := make([]AnnotatedDiscoverItem, 0, len(rawItems))
	for _, it := range rawItems {
		out := AnnotatedDiscoverItem{
			MediaType:     it.MediaType,
			TMDBID:        it.TMDBID,
			Title:         it.Title,
			OriginalTitle: it.OriginalTitle,
			Year:          it.Year,
			Overview:      it.Overview,
			PosterURL:     tmdbImageURL("w342", it.PosterPath),
			BackdropURL:   tmdbImageURL("w780", it.BackdropPath),
			Rating:        it.Rating,
			IsAnime:       it.IsAnime,
		}
		if it.MediaType == "movie" {
			if m, ok := localMovies[it.TMDBID]; ok {
				out.InLibrary = true
				out.LibraryItemID = m.ItemID
				out.LibraryID = m.LibraryID
			}
			if r, ok := activeMovies[it.TMDBID]; ok {
				out.RequestID = r.ID
				out.RequestStatus = s.resolveLiveStatus(ctx, liveCfg, r)
			}
		} else if it.MediaType == "tv" {
			if m, ok := localTV[it.TMDBID]; ok {
				out.InLibrary = true
				out.LibraryItemID = m.ItemID
				out.LibraryID = m.LibraryID
				out.SeriesTitle = m.SeriesTitle
			}
			if r, ok := activeTV[it.TMDBID]; ok {
				out.RequestID = r.ID
				out.RequestStatus = s.resolveLiveStatus(ctx, liveCfg, r)
			}
		}
		items = append(items, out)
	}

	return DiscoverResult{
		Items:      items,
		Page:       page,
		TotalPages: totalPages,
	}, nil
}

// CreateRequest creates a new media request, auto-approving and dispatching to Radarr/Sonarr when permitted.
func (s *Service) CreateRequest(ctx context.Context, user store.User, mediaType string, tmdbID int, isAnimeOverride *bool, clientIP string) (store.MediaRequestWithUser, error) {
	if !s.IsEnabled(ctx) {
		return store.MediaRequestWithUser{}, ErrDisabled
	}
	if user.Role != "admin" && !user.CanRequest {
		return store.MediaRequestWithUser{}, ErrForbidden
	}

	mt := strings.ToLower(strings.TrimSpace(mediaType))
	if mt != "movie" && mt != "tv" {
		return store.MediaRequestWithUser{}, fmt.Errorf("%w: media_type must be 'movie' or 'tv'", ErrValidation)
	}
	if tmdbID <= 0 {
		return store.MediaRequestWithUser{}, fmt.Errorf("%w: tmdb_id must be positive", ErrValidation)
	}

	// Check if already in user's accessible libraries.
	allowedLibs, unrestricted, err := s.store.AccessibleLibraryIDs(ctx, user)
	if err != nil {
		return store.MediaRequestWithUser{}, err
	}
	localMap, err := s.store.FindLocalMediaByTMDBAndType(ctx, mt, []int{tmdbID}, allowedLibs, !unrestricted)
	if err != nil {
		return store.MediaRequestWithUser{}, err
	}
	if _, exists := localMap[tmdbID]; exists {
		return store.MediaRequestWithUser{}, ErrAlreadyInLibrary
	}

	// Check if already actively requested.
	activeMap, err := s.store.FindActiveRequestsByTMDB(ctx, mt, []int{tmdbID})
	if err != nil {
		return store.MediaRequestWithUser{}, err
	}
	if _, exists := activeMap[tmdbID]; exists {
		return store.MediaRequestWithUser{}, ErrAlreadyRequested
	}

	// Fetch authoritative TMDB metadata.
	details, tvdbID, err := s.tmdb.GetRequestTargetDetails(ctx, mt, tmdbID)
	if err != nil {
		return store.MediaRequestWithUser{}, fmt.Errorf("fetch TMDB metadata: %w", err)
	}
	isAnime := details.IsAnime
	if isAnimeOverride != nil {
		isAnime = *isAnimeOverride
	}

	autoApprove := user.Role == "admin" || user.AutoApproveRequests
	status := store.RequestStatusPending
	var reviewedBy string
	var reviewedAt *time.Time
	if autoApprove {
		status = store.RequestStatusApproved
		reviewedBy = user.ID
		now := time.Now().UTC()
		reviewedAt = &now
	}

	req := store.MediaRequest{
		ID:            uuid.NewString(),
		UserID:        user.ID,
		MediaType:     mt,
		IsAnime:       isAnime,
		TMDBID:        tmdbID,
		TVDBID:        tvdbID,
		Title:         details.Title,
		OriginalTitle: details.OriginalTitle,
		Year:          details.Year,
		Overview:      details.Overview,
		PosterPath:    details.PosterPath,
		BackdropPath:  details.BackdropPath,
		Status:        status,
		ReviewedBy:    reviewedBy,
		ReviewedAt:    reviewedAt,
	}
	if err := s.store.CreateMediaRequest(ctx, req); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return store.MediaRequestWithUser{}, ErrAlreadyRequested
		}
		return store.MediaRequestWithUser{}, err
	}

	if autoApprove {
		s.dispatchAndUpdate(ctx, req, user.ID, reviewedAt, nil)
	}

	evType := "request.created"
	if autoApprove {
		evType = "request.auto_approved"
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: evType,
		ActorID:   user.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("requested %s %q (tmdb:%d)", mt, details.Title, tmdbID),
	})

	return s.store.GetMediaRequest(ctx, req.ID)
}

// ListRequests lists requests for the current user (or all users for admins unless scope=="mine")
// along with the pending request count.
func (s *Service) ListRequests(ctx context.Context, user store.User, scope, statusFilter string, limit int) ([]store.MediaRequestWithUser, int, error) {
	userFilter := user.ID
	if user.Role == "admin" && !strings.EqualFold(strings.TrimSpace(scope), "mine") {
		userFilter = ""
	}
	reqs, err := s.store.ListMediaRequests(ctx, userFilter, "", limit)
	if err != nil {
		return nil, 0, err
	}
	if len(reqs) > 0 {
		if cfg, cfgErr := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil); cfgErr == nil {
			for i := range reqs {
				reqs[i].Status = s.resolveLiveStatus(ctx, &cfg, reqs[i].MediaRequest)
			}
		}
	}
	sf := strings.TrimSpace(statusFilter)
	if sf != "" {
		filtered := make([]store.MediaRequestWithUser, 0, len(reqs))
		for _, r := range reqs {
			if r.Status == sf {
				filtered = append(filtered, r)
			}
		}
		reqs = filtered
	}
	pendingCount := 0
	if user.Role == "admin" {
		if n, err := s.store.CountPendingRequests(ctx); err == nil {
			pendingCount = n
		}
	} else {
		for _, r := range reqs {
			if r.Status == store.RequestStatusPending {
				pendingCount++
			}
		}
	}
	return reqs, pendingCount, nil
}

// ApproveRequest approves a pending/failed/declined request and dispatches it to Radarr/Sonarr if configured.
func (s *Service) ApproveRequest(ctx context.Context, admin store.User, id string, isAnimeOverride *bool, clientIP string) (store.MediaRequestWithUser, error) {
	existing, err := s.store.GetMediaRequest(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MediaRequestWithUser{}, ErrNotFound
		}
		return store.MediaRequestWithUser{}, err
	}
	if existing.Status == store.RequestStatusProcessing || existing.Status == store.RequestStatusAvailable {
		return store.MediaRequestWithUser{}, ErrConflict
	}

	req := existing.MediaRequest
	if isAnimeOverride != nil {
		req.IsAnime = *isAnimeOverride
	}
	now := time.Now().UTC()

	s.dispatchAndUpdate(ctx, req, admin.ID, &now, isAnimeOverride)

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.request_approved",
		ActorID:   admin.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("approved request %s (%s %q)", id, req.MediaType, req.Title),
	})

	return s.store.GetMediaRequest(ctx, id)
}

// DeclineRequest marks a request as declined with an optional note.
func (s *Service) DeclineRequest(ctx context.Context, admin store.User, id, note, clientIP string) (store.MediaRequestWithUser, error) {
	existing, err := s.store.GetMediaRequest(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MediaRequestWithUser{}, ErrNotFound
		}
		return store.MediaRequestWithUser{}, err
	}

	now := time.Now().UTC()
	if err := s.store.UpdateMediaRequestStatus(ctx, id, store.MediaRequestStatusUpdate{
		Status:     store.RequestStatusDeclined,
		StatusNote: strings.TrimSpace(note),
		ReviewedBy: admin.ID,
		ReviewedAt: &now,
	}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.MediaRequestWithUser{}, ErrNotFound
		}
		return store.MediaRequestWithUser{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.request_declined",
		ActorID:   admin.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("declined request %s (%s %q)", id, existing.MediaType, existing.Title),
	})

	return s.store.GetMediaRequest(ctx, id)
}

// DeleteRequest removes a request. Admins may delete any request; members may cancel only their own pending requests.
func (s *Service) DeleteRequest(ctx context.Context, user store.User, id, clientIP string) error {
	existing, err := s.store.GetMediaRequest(ctx, id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if user.Role != "admin" {
		if existing.UserID != user.ID || existing.Status != store.RequestStatusPending {
			return ErrForbidden
		}
	}
	if err := s.store.DeleteMediaRequest(ctx, id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "request.deleted",
		ActorID:   user.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("deleted request %s (%s %q)", id, existing.MediaType, existing.Title),
	})
	return nil
}

func (s *Service) dispatchAndUpdate(ctx context.Context, req store.MediaRequest, reviewerID string, reviewedAt *time.Time, isAnimeOverride *bool) {
	cfg, err := loadStoredConfig(ctx, s.store, s.envCfg, s.tmdb != nil)
	if err != nil {
		s.logger.Warn("failed to load requests config during dispatch", "request_id", req.ID, "error", err)
		return
	}

	slotName, slotCfg, ok := selectArrSlot(cfg, req.MediaType, req.IsAnime)
	if !ok {
		// Local-only mode: leave as 'approved' for manual fulfillment.
		_ = s.store.UpdateMediaRequestStatus(ctx, req.ID, store.MediaRequestStatusUpdate{
			Status:     store.RequestStatusApproved,
			StatusNote: "",
			ReviewedBy: reviewerID,
			ReviewedAt: reviewedAt,
			IsAnime:    isAnimeOverride,
		})
		return
	}

	var (
		extID   int
		dispErr error
	)
	if req.MediaType == "movie" {
		extID, dispErr = s.arr.AddMovie(ctx, slotCfg, req.TMDBID, req.Title)
	} else {
		extID, dispErr = s.arr.AddSeries(ctx, slotCfg, req.TMDBID, req.TVDBID, req.Title)
	}

	if dispErr != nil {
		s.logger.Warn("failed to dispatch media request to arr",
			"request_id", req.ID,
			"slot", slotName,
			"title", req.Title,
			"error", dispErr,
		)
		_ = s.store.UpdateMediaRequestStatus(ctx, req.ID, store.MediaRequestStatusUpdate{
			Status:          store.RequestStatusFailed,
			StatusNote:      dispErr.Error(),
			ExternalService: slotName,
			ReviewedBy:      reviewerID,
			ReviewedAt:      reviewedAt,
			IsAnime:         isAnimeOverride,
		})
		return
	}

	_ = s.store.UpdateMediaRequestStatus(ctx, req.ID, store.MediaRequestStatusUpdate{
		Status:          store.RequestStatusApproved,
		StatusNote:      "",
		ExternalService: slotName,
		ExternalID:      extID,
		ReviewedBy:      reviewerID,
		ReviewedAt:      reviewedAt,
		IsAnime:         isAnimeOverride,
	})
}

// resolveLiveStatus overlays live Radarr/Sonarr queue state onto approved/processing requests:
// dispatched requests stay calmly on "approved" while waiting for release, and show "processing"
// only while actively present in the *arr instance's /api/v3/queue.
func (s *Service) resolveLiveStatus(ctx context.Context, cfg *RequestsConfig, req store.MediaRequest) string {
	if req.Status != store.RequestStatusApproved && req.Status != store.RequestStatusProcessing {
		return req.Status
	}
	if req.ExternalID <= 0 || req.ExternalService == "" {
		return req.Status
	}
	if cfg == nil {
		return store.RequestStatusApproved
	}
	ids := s.getActiveQueueIDs(ctx, *cfg, req.ExternalService)
	if ids[req.ExternalID] {
		return store.RequestStatusProcessing
	}
	return store.RequestStatusApproved
}

func (s *Service) getActiveQueueIDs(ctx context.Context, cfg RequestsConfig, slotName string) map[int]bool {
	var (
		slotCfg  ArrInstanceConfig
		isRadarr bool
	)
	switch slotName {
	case "radarr":
		slotCfg, isRadarr = cfg.Radarr, true
	case "radarr_anime":
		slotCfg, isRadarr = cfg.RadarrAnime, true
	case "sonarr":
		slotCfg, isRadarr = cfg.Sonarr, false
	case "sonarr_anime":
		slotCfg, isRadarr = cfg.SonarrAnime, false
	default:
		return nil
	}
	if !slotCfg.Enabled || strings.TrimSpace(slotCfg.URL) == "" || strings.TrimSpace(slotCfg.APIKey) == "" {
		return nil
	}

	s.queueMu.Lock()
	if cached, ok := s.queueCache[slotName]; ok && time.Since(cached.fetchedAt) < queueCacheTTL {
		s.queueMu.Unlock()
		return cached.ids
	}
	s.queueMu.Unlock()

	ids, err := s.arr.ActiveQueueIDs(ctx, isRadarr, slotCfg)
	if err != nil {
		// On transient error, cache empty map briefly to avoid stalling repeated list calls.
		ids = nil
	}
	s.queueMu.Lock()
	if s.queueCache == nil {
		s.queueCache = make(map[string]cachedQueue)
	}
	s.queueCache[slotName] = cachedQueue{ids: ids, fetchedAt: time.Now()}
	s.queueMu.Unlock()
	return ids
}

func selectArrSlot(cfg RequestsConfig, mediaType string, isAnime bool) (string, ArrInstanceConfig, bool) {
	switch mediaType {
	case "movie":
		if isAnime && cfg.RadarrAnime.Enabled {
			return "radarr_anime", cfg.RadarrAnime, true
		}
		if cfg.Radarr.Enabled {
			slot := cfg.Radarr
			if isAnime {
				if slot.AnimeQualityProfileID > 0 {
					slot.QualityProfileID = slot.AnimeQualityProfileID
				}
				if slot.AnimeRootFolder != "" {
					slot.RootFolder = slot.AnimeRootFolder
				}
			}
			return "radarr", slot, true
		}
	case "tv":
		if isAnime && cfg.SonarrAnime.Enabled {
			return "sonarr_anime", cfg.SonarrAnime, true
		}
		if cfg.Sonarr.Enabled {
			slot := cfg.Sonarr
			if isAnime {
				if slot.AnimeQualityProfileID > 0 {
					slot.QualityProfileID = slot.AnimeQualityProfileID
				}
				if slot.AnimeRootFolder != "" {
					slot.RootFolder = slot.AnimeRootFolder
				}
				slot.SeriesType = "anime"
			}
			return "sonarr", slot, true
		}
	}
	return "", ArrInstanceConfig{}, false
}

// PosterURL builds the full TMDB image URL for a stored poster_path.
func PosterURL(path string) string {
	return tmdbImageURL("w342", path)
}

// BackdropURL builds the full TMDB image URL for a stored backdrop_path.
func BackdropURL(path string) string {
	return tmdbImageURL("w780", path)
}

func tmdbImageURL(size, path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return tmdbImageBase + "/" + size + path
}
