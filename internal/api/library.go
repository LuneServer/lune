package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
)

type LibraryDTO struct {
	ID        string             `json:"id"`
	Name      string             `json:"name"`
	Kind      string             `json:"kind"`
	RootPath  string             `json:"root_path,omitempty"`
	ItemCount int                `json:"item_count"`
	Scan      library.ScanStatus `json:"scan"`
	CreatedAt time.Time          `json:"created_at"`
}

type MediaItemSummaryDTO struct {
	ID                    string    `json:"id"`
	LibraryID             string    `json:"library_id"`
	Kind                  string    `json:"kind"`
	Title                 string    `json:"title"`
	OriginalTitle         string    `json:"original_title,omitempty"`
	Year                  *int      `json:"year,omitempty"`
	SeriesTitle           string    `json:"series_title,omitempty"`
	SeasonNumber          *int      `json:"season_number,omitempty"`
	EpisodeNumber         *int      `json:"episode_number,omitempty"`
	AbsoluteEpisodeNumber *int      `json:"absolute_episode_number,omitempty"`
	ReleaseGroup          string    `json:"release_group,omitempty"`
	Container             string    `json:"container,omitempty"`
	DurationMS            int64     `json:"duration_ms"`
	VideoCodec            string    `json:"video_codec,omitempty"`
	VideoWidth            int       `json:"video_width,omitempty"`
	VideoHeight           int       `json:"video_height,omitempty"`
	VideoHDR              string    `json:"video_hdr,omitempty"`
	AudioCodec            string    `json:"audio_codec,omitempty"`
	AudioChannels         int       `json:"audio_channels,omitempty"`
	AudioLanguages        []string  `json:"audio_languages,omitempty"`
	DualAudio             bool      `json:"dual_audio,omitempty"`
	PosterURL             string    `json:"poster_url,omitempty"`
	BackdropURL           string    `json:"backdrop_url,omitempty"`
	Overview              string    `json:"overview,omitempty"`
	Rating                float64   `json:"rating,omitempty"`
	ContentRating         string    `json:"content_rating,omitempty"`
	Genres                []string  `json:"genres,omitempty"`
	CreatedAt             time.Time `json:"created_at"`
}

type CreditPersonDTO struct {
	Name       string `json:"name"`
	Role       string `json:"role,omitempty"`
	Department string `json:"department,omitempty"`
	PhotoURL   string `json:"photo_url,omitempty"`
}

type MediaItemDetailDTO struct {
	MediaItemSummaryDTO
	Tagline               string                     `json:"tagline,omitempty"`
	ContentRating         string                     `json:"content_rating,omitempty"`
	ReleaseDate           string                     `json:"release_date,omitempty"`
	Genres                []string                   `json:"genres"`
	Directors             []string                   `json:"directors"`
	Writers               []string                   `json:"writers"`
	Studios               []string                   `json:"studios"`
	Cast                  []CreditPersonDTO          `json:"cast"`
	TMDBID                string                     `json:"tmdb_id,omitempty"`
	IMDBID                string                     `json:"imdb_id,omitempty"`
	FilePath              string                     `json:"file_path,omitempty"`
	FileSize              int64                      `json:"file_size"`
	Bitrate               int64                      `json:"bitrate"`
	Streams               []metadata.StreamInfo      `json:"streams"`
	Chapters              []metadata.ChapterInfo     `json:"chapters"`
	AudioTracks           []stream.AudioTrackDTO     `json:"audio_tracks"`
	Subtitles             []stream.SubtitleStreamDTO `json:"subtitles"`
	SelectedAudioIndex    *int                       `json:"selected_audio_index,omitempty"`
	SelectedSubtitleIndex *int                       `json:"selected_subtitle_index,omitempty"`
	Progress              *ProgressDTO               `json:"progress,omitempty"`
	InWatchlist           bool                       `json:"in_watchlist"`
	IsFavorite            bool                       `json:"is_favorite"`
}

type CreateLibraryInput struct {
	Body struct {
		Name     string `json:"name" minLength:"1" maxLength:"64"`
		Kind     string `json:"kind" enum:"movies,shows,anime,music,other"`
		RootPath string `json:"root_path" minLength:"1"`
	}
}

type LibraryResponse struct {
	Body LibraryDTO
}

type LibrariesListResponse struct {
	Body struct {
		Libraries []LibraryDTO `json:"libraries"`
	}
}

type LibraryIDInput struct {
	ID string `path:"id"`
}

type ScanLibraryInput struct {
	ID    string `path:"id"`
	Sync  bool   `query:"sync" default:"false" doc:"Run scan synchronously and wait for completion"`
	Force bool   `query:"force" default:"false" doc:"Re-probe and re-enrich unchanged files"`
}

type ScanStatusResponse struct {
	Body library.ScanStatus
}

type LibraryFacetsResponse struct {
	Body store.LibraryFacets
}

type ListItemsInput struct {
	LibraryID     string  `query:"library_id"`
	Kind          string  `query:"kind" enum:",movie,episode,track,other"`
	Query         string  `query:"q"`
	Genre         string  `query:"genre"`
	Person        string  `query:"person"`
	Studio        string  `query:"studio"`
	ContentRating string  `query:"content_rating"`
	MinRating     float64 `query:"min_rating" minimum:"0" maximum:"10"`
	Year          int     `query:"year" minimum:"0" maximum:"2200"`
	Decade        int     `query:"decade" minimum:"0" maximum:"2200"`
	Resolution    string  `query:"resolution" enum:",4k,1080p,720p"`
	HDR           bool    `query:"hdr"`
	DualAudio     bool    `query:"dual_audio"`
	Watched       string  `query:"watched" enum:",watched,unwatched"`
	Sort          string  `query:"sort" enum:"title,recent,year,rating" default:"title"`
	Limit         int     `query:"limit" default:"50" minimum:"1" maximum:"200"`
	Offset        int     `query:"offset" default:"0" minimum:"0"`
}

type SimilarItemsInput struct {
	ID    string `path:"id"`
	Limit int    `query:"limit" default:"12" minimum:"1" maximum:"24"`
}

type SimilarItemsResponse struct {
	Body struct {
		Items  []MediaItemSummaryDTO `json:"items"`
		Series []SeriesSummaryDTO    `json:"series"`
	}
}

type ListItemsResponse struct {
	Body struct {
		Items  []MediaItemSummaryDTO `json:"items"`
		Total  int                   `json:"total"`
		Limit  int                   `json:"limit"`
		Offset int                   `json:"offset"`
	}
}

type ItemIDInput struct {
	ID string `path:"id"`
}

type ItemDetailResponse struct {
	Body MediaItemDetailDTO
}

type RefreshItemInput struct {
	ID   string `path:"id"`
	Body struct {
		Title  string `json:"title,omitempty" maxLength:"200"`
		Year   *int   `json:"year,omitempty" minimum:"1880" maximum:"2200"`
		TMDBID string `json:"tmdb_id,omitempty" maxLength:"32"`
	}
}

type SearchMetadataInput struct {
	Kind  string `query:"kind" enum:"movie,tv" default:"movie"`
	Query string `query:"q" required:"true" minLength:"1" maxLength:"200"`
	Year  int    `query:"year" minimum:"0" maximum:"2200"`
}

type SearchMetadataResponse struct {
	Body struct {
		Configured bool                            `json:"configured"`
		Results    []metadata.MetadataSearchResult `json:"results"`
	}
}

func registerLibraryRoutes(r chi.Router, api huma.API, libSvc *library.Service, authSvc *auth.Service, streamSvc *stream.Service, dataDir string) {
	huma.Register(api, huma.Operation{
		OperationID: "list-libraries",
		Method:      http.MethodGet,
		Path:        "/api/v1/libraries",
		Summary:     "List configured media libraries accessible to the current user",
		Tags:        []string{"Libraries"},
	}, func(ctx context.Context, _ *struct{}) (*LibrariesListResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		allowedIDs, unrestricted, err := authSvc.LibraryScope(ctx, user)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to resolve library permissions", err)
		}
		libs, err := libSvc.ListLibrariesFiltered(ctx, allowedIDs, !unrestricted)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list libraries", err)
		}
		out := &LibrariesListResponse{}
		out.Body.Libraries = make([]LibraryDTO, 0, len(libs))
		isAdmin := user.Role == "admin"
		for _, l := range libs {
			out.Body.Libraries = append(out.Body.Libraries, toLibraryDTO(l, libSvc.GetScanStatus(l.ID), isAdmin))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "create-library",
		Method:        http.MethodPost,
		Path:          "/api/v1/libraries",
		Summary:       "Register a media library directory (admin only)",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Libraries"},
	}, func(ctx context.Context, in *CreateLibraryInput) (*LibraryResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		lib, err := libSvc.CreateLibrary(ctx, in.Body.Name, in.Body.Kind, in.Body.RootPath)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		return &LibraryResponse{Body: toLibraryDTO(lib, libSvc.GetScanStatus(lib.ID), true)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "delete-library",
		Method:        http.MethodDelete,
		Path:          "/api/v1/libraries/{id}",
		Summary:       "Remove a library and its indexed catalog entries (admin only)",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Libraries"},
	}, func(ctx context.Context, in *LibraryIDInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if err := libSvc.DeleteLibrary(ctx, in.ID); err != nil {
			return nil, mapLibraryError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "scan-library",
		Method:      http.MethodPost,
		Path:        "/api/v1/libraries/{id}/scan",
		Summary:     "Scan a media library for new, modified, or removed files (admin only)",
		Tags:        []string{"Libraries"},
	}, func(ctx context.Context, in *ScanLibraryInput) (*ScanStatusResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		var (
			st  library.ScanStatus
			err error
		)
		if in.Sync {
			st, err = libSvc.ScanSync(ctx, in.ID, in.Force)
		} else {
			st, err = libSvc.StartScanAsync(ctx, in.ID, in.Force)
		}
		if err != nil {
			return nil, mapLibraryError(err)
		}
		return &ScanStatusResponse{Body: st}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-library-scan",
		Method:      http.MethodGet,
		Path:        "/api/v1/libraries/{id}/scan",
		Summary:     "Get current or latest scan status for a library",
		Tags:        []string{"Libraries"},
	}, func(ctx context.Context, in *LibraryIDInput) (*ScanStatusResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if _, err := libSvc.GetLibrary(ctx, in.ID); err != nil {
			return nil, mapLibraryError(err)
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		st := libSvc.GetScanStatus(in.ID)
		if user.Role != "admin" {
			st.LastError = ""
			st.ErrorSamples = nil
		}
		return &ScanStatusResponse{Body: st}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-library-facets",
		Method:      http.MethodGet,
		Path:        "/api/v1/libraries/{id}/facets",
		Summary:     "List distinct filter values (genres, certifications, years, capabilities) available in a library",
		Tags:        []string{"Libraries"},
	}, func(ctx context.Context, in *LibraryIDInput) (*LibraryFacetsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if _, err := libSvc.GetLibrary(ctx, in.ID); err != nil {
			return nil, mapLibraryError(err)
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		facets, err := libSvc.GetLibraryFacets(ctx, in.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to compute library facets", err)
		}
		return &LibraryFacetsResponse{Body: facets}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-items",
		Method:      http.MethodGet,
		Path:        "/api/v1/items",
		Summary:     "Browse and search indexed media items",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *ListItemsInput) (*ListItemsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		allowedIDs, unrestricted, err := authSvc.LibraryScope(ctx, user)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to resolve library permissions", err)
		}
		if !unrestricted && in.LibraryID != "" && !containsString(allowedIDs, in.LibraryID) {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		items, total, err := libSvc.ListItems(ctx, store.MediaListFilter{
			LibraryID:         in.LibraryID,
			AllowedLibraryIDs: allowedIDs,
			RestrictLibraries: !unrestricted,
			UserID:            user.ID,
			Kind:              in.Kind,
			Query:             in.Query,
			Genre:             in.Genre,
			Person:            in.Person,
			Studio:            in.Studio,
			ContentRating:     in.ContentRating,
			MinRating:         in.MinRating,
			Year:              in.Year,
			Decade:            in.Decade,
			Resolution:        in.Resolution,
			HDR:               in.HDR,
			DualAudio:         in.DualAudio,
			Watched:           in.Watched,
			Sort:              in.Sort,
			Limit:             in.Limit,
			Offset:            in.Offset,
		})
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list media items", err)
		}
		return buildItemsListResponse(items, total, in.Limit, in.Offset), nil
	})

	enrichItemDetail := func(ctx context.Context, user store.User, item store.MediaItem) MediaItemDetailDTO {
		detail := toMediaDetailDTO(item)
		metadata.PopulateChapterThumbnails(dataDir, item.ID, detail.Chapters)
		if user.Role == "admin" {
			detail.FilePath = item.FilePath
		}
		if inWL, isFav, err := authSvc.GetItemMarks(ctx, user.ID, item.ID); err == nil {
			detail.InWatchlist = inWL
			detail.IsFavorite = isFav
		}
		if streamSvc != nil {
			if prog, err := streamSvc.GetProgress(ctx, user, item.ID); err == nil && (prog.PositionMS > 0 || prog.Completed || prog.PlayCount > 0) {
				detail.Progress = &ProgressDTO{
					ItemID:       prog.ItemID,
					PositionMS:   prog.PositionMS,
					DurationMS:   prog.DurationMS,
					Completed:    prog.Completed,
					PlayCount:    prog.PlayCount,
					LastPlayedAt: prog.LastPlayedAt,
				}
			}
			tracks := streamSvc.InspectItemTracks(ctx, user, item)
			detail.AudioTracks = tracks.AudioTracks
			detail.Subtitles = tracks.Subtitles
			detail.SelectedAudioIndex = tracks.SelectedAudioIndex
			detail.SelectedSubtitleIndex = tracks.SelectedSubtitleIndex
		}
		return detail
	}

	huma.Register(api, huma.Operation{
		OperationID: "get-item",
		Method:      http.MethodGet,
		Path:        "/api/v1/items/{id}",
		Summary:     "Get full technical details and stream metadata for a media item",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *ItemIDInput) (*ItemDetailResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		item, err := libSvc.GetItem(ctx, in.ID)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, item.LibraryID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		return &ItemDetailResponse{Body: enrichItemDetail(ctx, user, item)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-similar-items",
		Method:      http.MethodGet,
		Path:        "/api/v1/items/{id}/similar",
		Summary:     "Find locally indexed movies or series similar to a media item",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *SimilarItemsInput) (*SimilarItemsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		item, err := libSvc.GetItem(ctx, in.ID)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, item.LibraryID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		allowedIDs, unrestricted, err := authSvc.LibraryScope(ctx, user)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to resolve library permissions", err)
		}
		simItems, simSeries, err := libSvc.FindSimilarMedia(ctx, item, allowedIDs, !unrestricted, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to find similar media", err)
		}
		out := &SimilarItemsResponse{}
		out.Body.Items = make([]MediaItemSummaryDTO, 0, len(simItems))
		for _, it := range simItems {
			out.Body.Items = append(out.Body.Items, toMediaSummaryDTO(it))
		}
		out.Body.Series = make([]SeriesSummaryDTO, 0, len(simSeries))
		for _, ss := range simSeries {
			out.Body.Series = append(out.Body.Series, toSeriesSummaryDTO(ss))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "search-metadata",
		Method:      http.MethodGet,
		Path:        "/api/v1/metadata/search",
		Summary:     "Search TMDB for movie or TV metadata candidates before re-matching (admin only)",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *SearchMetadataInput) (*SearchMetadataResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		var yr *int
		if in.Year > 1880 {
			y := in.Year
			yr = &y
		}
		results, configured, err := libSvc.SearchMetadata(ctx, in.Kind, in.Query, yr)
		if err != nil {
			return nil, huma.Error502BadGateway("metadata provider search failed", err)
		}
		out := &SearchMetadataResponse{}
		out.Body.Configured = configured
		out.Body.Results = results
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "refresh-item-metadata",
		Method:      http.MethodPost,
		Path:        "/api/v1/items/{id}/refresh",
		Summary:     "Re-probe and re-enrich metadata for a single item, with optional manual re-match (admin only)",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *RefreshItemInput) (*ItemDetailResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		item, err := libSvc.RefreshItem(ctx, in.ID, in.Body.Title, in.Body.Year, in.Body.TMDBID)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		return &ItemDetailResponse{Body: enrichItemDetail(ctx, user, item)}, nil
	})

	// Binary chapter still thumbnail endpoint (enforces auth + per-user library ACL).
	// Binary trickplay sprite sheets (scrubbing previews) generated by maintenance.
	r.Get("/api/v1/items/{id}/trickplay/{sheet}", func(w http.ResponseWriter, req *http.Request) {
		user, _, ok := currentPrincipal(req.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		sheet := chi.URLParam(req, "sheet")
		if !metadata.TrickplaySheetPattern.MatchString(sheet) {
			http.NotFound(w, req)
			return
		}
		item, err := libSvc.GetItem(req.Context(), chi.URLParam(req, "id"))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		allowed, err := authSvc.CanAccessLibrary(req.Context(), user, item.LibraryID)
		if err != nil || !allowed {
			http.Error(w, "access to this library is restricted", http.StatusForbidden)
			return
		}
		if dataDir == "" {
			http.NotFound(w, req)
			return
		}
		sheetPath := filepath.Join(metadata.TrickplayDir(dataDir, item.ID), sheet)
		info, err := os.Stat(sheetPath)
		if err != nil || info.IsDir() {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", metadata.TrickplayContentType(sheet))
		// URLs carry ?v=<generated_at>, so regenerated sheets get new URLs.
		w.Header().Set("Cache-Control", "private, max-age=604800, immutable")
		http.ServeFile(w, req, sheetPath)
	})

	r.Get("/api/v1/items/{id}/artwork/chapters/{index}", func(w http.ResponseWriter, req *http.Request) {
		user, _, ok := currentPrincipal(req.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		rawIndex := chi.URLParam(req, "index")
		idx, err := strconv.Atoi(rawIndex)
		if err != nil || idx < 0 || strconv.Itoa(idx) != rawIndex {
			http.NotFound(w, req)
			return
		}
		item, err := libSvc.GetItem(req.Context(), chi.URLParam(req, "id"))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		allowed, err := authSvc.CanAccessLibrary(req.Context(), user, item.LibraryID)
		if err != nil || !allowed {
			http.Error(w, "access to this library is restricted", http.StatusForbidden)
			return
		}
		if dataDir == "" {
			http.NotFound(w, req)
			return
		}
		stillPath := filepath.Join(dataDir, "artwork", "chapters", item.ID, strconv.Itoa(idx)+".jpg")
		info, err := os.Stat(stillPath)
		if err != nil || info.IsDir() {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Header().Set("Cache-Control", "private, max-age=604800")
		http.ServeFile(w, req, stillPath)
	})

	// Binary local artwork endpoint (strictly serves indexed poster/backdrop files).
	r.Get("/api/v1/items/{id}/artwork/{kind}", func(w http.ResponseWriter, req *http.Request) {
		user, _, ok := currentPrincipal(req.Context())
		if !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		item, err := libSvc.GetItem(req.Context(), chi.URLParam(req, "id"))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		allowed, err := authSvc.CanAccessLibrary(req.Context(), user, item.LibraryID)
		if err != nil || !allowed {
			http.Error(w, "access to this library is restricted", http.StatusForbidden)
			return
		}
		var artPath string
		switch chi.URLParam(req, "kind") {
		case "poster":
			artPath = item.PosterPath
		case "backdrop":
			artPath = item.BackdropPath
		default:
			http.NotFound(w, req)
			return
		}
		if artPath == "" {
			http.NotFound(w, req)
			return
		}
		resolvedPath, err := libSvc.ResolveArtwork(req.Context(), artPath)
		if err != nil {
			switch {
			case errors.Is(err, library.ErrNotFound):
				http.NotFound(w, req)
			case errors.Is(err, context.DeadlineExceeded):
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "upstream artwork timeout", http.StatusGatewayTimeout)
			default:
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "artwork temporarily unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=86400")
		http.ServeFile(w, req, resolvedPath)
	})

	// Authenticated lazily-cached cast/crew headshot endpoint.
	r.Get("/api/v1/artwork/people/{file}", func(w http.ResponseWriter, req *http.Request) {
		if _, _, ok := currentPrincipal(req.Context()); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		photoPath, err := libSvc.GetPersonPhoto(req.Context(), chi.URLParam(req, "file"))
		if err != nil {
			switch {
			case errors.Is(err, library.ErrNotFound):
				http.NotFound(w, req)
			case errors.Is(err, context.DeadlineExceeded):
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "upstream artwork timeout", http.StatusGatewayTimeout)
			default:
				w.Header().Set("Cache-Control", "no-store")
				http.Error(w, "artwork temporarily unavailable", http.StatusServiceUnavailable)
			}
			return
		}
		if photoPath == "" {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=604800")
		http.ServeFile(w, req, photoPath)
	})
}

func toLibraryDTO(l store.Library, scan library.ScanStatus, isAdmin bool) LibraryDTO {
	rootPath := l.RootPath
	if !isAdmin {
		rootPath = ""
		scan.LastError = ""
		scan.ErrorSamples = nil
	}
	return LibraryDTO{
		ID:        l.ID,
		Name:      l.Name,
		Kind:      l.Kind,
		RootPath:  rootPath,
		ItemCount: l.ItemCount,
		Scan:      scan,
		CreatedAt: l.CreatedAt,
	}
}

func toMediaSummaryDTO(m store.MediaItem) MediaItemSummaryDTO {
	var audioLangs []string
	if m.AudioLanguages != "" {
		for _, part := range strings.Split(m.AudioLanguages, ",") {
			if p := strings.TrimSpace(part); p != "" {
				audioLangs = append(audioLangs, p)
			}
		}
	}
	dto := MediaItemSummaryDTO{
		ID:                    m.ID,
		LibraryID:             m.LibraryID,
		Kind:                  m.Kind,
		Title:                 m.Title,
		OriginalTitle:         m.OriginalTitle,
		Year:                  m.Year,
		SeriesTitle:           m.SeriesTitle,
		SeasonNumber:          m.SeasonNumber,
		EpisodeNumber:         m.EpisodeNumber,
		AbsoluteEpisodeNumber: m.AbsoluteEpisodeNumber,
		ReleaseGroup:          m.ReleaseGroup,
		Container:             m.Container,
		DurationMS:            m.DurationMS,
		VideoCodec:            m.VideoCodec,
		VideoWidth:            m.VideoWidth,
		VideoHeight:           m.VideoHeight,
		VideoHDR:              m.VideoHDR,
		AudioCodec:            m.AudioCodec,
		AudioChannels:         m.AudioChannels,
		AudioLanguages:        audioLangs,
		DualAudio:             len(audioLangs) >= 2,
		Overview:              m.Overview,
		Rating:                m.Rating,
		ContentRating:         m.ContentRating,
		Genres:                decodeStringList(m.GenresJSON),
		CreatedAt:             m.CreatedAt,
	}
	if m.PosterPath != "" {
		dto.PosterURL = "/api/v1/items/" + m.ID + "/artwork/poster"
	}
	if m.BackdropPath != "" {
		dto.BackdropURL = "/api/v1/items/" + m.ID + "/artwork/backdrop"
	}
	return dto
}

func toMediaDetailDTO(item store.MediaItem) MediaItemDetailDTO {
	var streams []metadata.StreamInfo
	if item.StreamsJSON != "" {
		_ = json.Unmarshal([]byte(item.StreamsJSON), &streams)
	}
	if streams == nil {
		streams = []metadata.StreamInfo{}
	}
	var chapters []metadata.ChapterInfo
	if item.ChaptersJSON != "" {
		_ = json.Unmarshal([]byte(item.ChaptersJSON), &chapters)
	}
	if chapters == nil {
		chapters = []metadata.ChapterInfo{}
	}
	var genres []string
	if item.GenresJSON != "" {
		_ = json.Unmarshal([]byte(item.GenresJSON), &genres)
	}
	if genres == nil {
		genres = []string{}
	}
	directors := decodeStringList(item.DirectorsJSON)
	writers := decodeStringList(item.WritersJSON)
	studios := decodeStringList(item.StudiosJSON)
	cast := decodeCreditPeople(item.CastJSON)
	return MediaItemDetailDTO{
		MediaItemSummaryDTO: toMediaSummaryDTO(item),
		Tagline:             item.Tagline,
		ContentRating:       item.ContentRating,
		ReleaseDate:         item.ReleaseDate,
		Genres:              genres,
		Directors:           directors,
		Writers:             writers,
		Studios:             studios,
		Cast:                cast,
		TMDBID:              item.TMDBID,
		IMDBID:              item.IMDBID,
		FileSize:            item.FileSize,
		Bitrate:             item.Bitrate,
		Streams:             streams,
		Chapters:            chapters,
		AudioTracks:         []stream.AudioTrackDTO{},
		Subtitles:           []stream.SubtitleStreamDTO{},
	}
}

func decodeStringList(raw string) []string {
	var list []string
	if raw != "" && raw != "[]" {
		_ = json.Unmarshal([]byte(raw), &list)
	}
	if list == nil {
		return []string{}
	}
	return list
}

func decodeCreditPeople(raw string) []CreditPersonDTO {
	var people []metadata.CreditPerson
	if raw != "" && raw != "[]" {
		_ = json.Unmarshal([]byte(raw), &people)
	}
	out := make([]CreditPersonDTO, 0, len(people))
	for _, p := range people {
		dto := CreditPersonDTO{
			Name:       p.Name,
			Role:       p.Role,
			Department: p.Department,
		}
		if clean := strings.TrimPrefix(strings.TrimSpace(p.ProfilePath), "/"); clean != "" && !strings.Contains(clean, "/") {
			dto.PhotoURL = "/api/v1/artwork/people/" + clean
		}
		out = append(out, dto)
	}
	return out
}

func containsString(list []string, target string) bool {
	for _, s := range list {
		if s == target {
			return true
		}
	}
	return false
}

func mapLibraryError(err error) error {
	switch {
	case errors.Is(err, library.ErrNotFound):
		return huma.Error404NotFound("library or media item not found")
	case errors.Is(err, library.ErrAlreadyExists):
		return huma.Error409Conflict("a library is already registered for that path")
	case errors.Is(err, library.ErrInvalidPath):
		return huma.Error422UnprocessableEntity(err.Error())
	case errors.Is(err, library.ErrScanRunning):
		return huma.Error409Conflict("library scan is already running")
	default:
		return huma.Error500InternalServerError("internal library error", err)
	}
}
