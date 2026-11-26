package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/store"
)

type SeriesSummaryDTO struct {
	LibraryID     string    `json:"library_id"`
	LibraryKind   string    `json:"library_kind,omitempty"`
	Title         string    `json:"title"`
	OriginalTitle string    `json:"original_title,omitempty"`
	Year          *int      `json:"year,omitempty"`
	Rating        float64   `json:"rating,omitempty"`
	ContentRating string    `json:"content_rating,omitempty"`
	Genres        []string  `json:"genres,omitempty"`
	Overview      string    `json:"overview,omitempty"`
	VideoHeight   int       `json:"video_height,omitempty"`
	VideoHDR      string    `json:"video_hdr,omitempty"`
	SeasonCount   int       `json:"season_count"`
	EpisodeCount  int       `json:"episode_count"`
	DualAudio     bool      `json:"dual_audio,omitempty"`
	PosterURL     string    `json:"poster_url,omitempty"`
	BackdropURL   string    `json:"backdrop_url,omitempty"`
	LatestAddedAt time.Time `json:"latest_added_at"`
}

type SeriesEpisodeDTO struct {
	Item     MediaItemSummaryDTO `json:"item"`
	Progress *ProgressDTO        `json:"progress,omitempty"`
}

type SeriesSeasonDTO struct {
	SeasonNumber int                `json:"season_number"`
	Episodes     []SeriesEpisodeDTO `json:"episodes"`
}

type ListSeriesInput struct {
	LibraryID     string  `query:"library_id"`
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
}

type ListSeriesResponse struct {
	Body struct {
		Series []SeriesSummaryDTO `json:"series"`
	}
}

type SeriesDetailInput struct {
	LibraryID string `query:"library_id"`
	Title     string `query:"title" required:"true"`
}

type SeriesDetailResponse struct {
	Body struct {
		LibraryID            string               `json:"library_id"`
		LibraryKind          string               `json:"library_kind,omitempty"`
		RepresentativeItemID string               `json:"representative_item_id,omitempty"`
		Title                string               `json:"title"`
		OriginalTitle        string               `json:"original_title,omitempty"`
		Overview             string               `json:"overview,omitempty"`
		Tagline              string               `json:"tagline,omitempty"`
		Year                 *int                 `json:"year,omitempty"`
		ContentRating        string               `json:"content_rating,omitempty"`
		Genres               []string             `json:"genres"`
		Studios              []string             `json:"studios"`
		Cast                 []CreditPersonDTO    `json:"cast"`
		TMDBID               string               `json:"tmdb_id,omitempty"`
		IMDBID               string               `json:"imdb_id,omitempty"`
		DualAudio            bool                 `json:"dual_audio,omitempty"`
		PosterURL            string               `json:"poster_url,omitempty"`
		BackdropURL          string               `json:"backdrop_url,omitempty"`
		SeasonCount          int                  `json:"season_count"`
		EpisodeCount         int                  `json:"episode_count"`
		WatchedCount         int                  `json:"watched_count"`
		InWatchlist          bool                 `json:"in_watchlist"`
		IsFavorite           bool                 `json:"is_favorite"`
		NextUp               *MediaItemSummaryDTO `json:"next_up,omitempty"`
		Seasons              []SeriesSeasonDTO    `json:"seasons"`
	}
}

type SeriesWatchedInput struct {
	Body struct {
		LibraryID    string `json:"library_id" minLength:"1"`
		Title        string `json:"title" minLength:"1"`
		SeasonNumber *int   `json:"season_number,omitempty"`
		Completed    bool   `json:"completed"`
	}
}

type SeriesRefreshInput struct {
	Body struct {
		LibraryID     string `json:"library_id" minLength:"1"`
		Title         string `json:"title" minLength:"1"`
		OverrideTitle string `json:"override_title,omitempty" maxLength:"200"`
		OverrideYear  *int   `json:"override_year,omitempty" minimum:"1880" maximum:"2200"`
		TMDBID        string `json:"tmdb_id,omitempty" maxLength:"32"`
	}
}

type SeriesBulkResponse struct {
	Body struct {
		Updated int    `json:"updated"`
		Title   string `json:"title,omitempty"`
	}
}

type NextEpisodeResponse struct {
	Body struct {
		NextItem *MediaItemSummaryDTO `json:"next_item,omitempty"`
	}
}

func toSeriesSummaryDTO(ss store.SeriesSummary) SeriesSummaryDTO {
	dto := SeriesSummaryDTO{
		LibraryID:     ss.LibraryID,
		LibraryKind:   ss.LibraryKind,
		Title:         ss.Title,
		OriginalTitle: ss.OriginalTitle,
		Year:          ss.Year,
		Rating:        ss.Rating,
		ContentRating: ss.ContentRating,
		Genres:        decodeStringList(ss.GenresJSON),
		Overview:      ss.Overview,
		VideoHeight:   ss.VideoHeight,
		VideoHDR:      ss.VideoHDR,
		SeasonCount:   ss.SeasonCount,
		EpisodeCount:  ss.EpisodeCount,
		DualAudio:     ss.DualAudio,
		LatestAddedAt: ss.LatestAddedAt,
	}
	if ss.HasPoster && ss.RepresentativeItemID != "" {
		dto.PosterURL = "/api/v1/items/" + ss.RepresentativeItemID + "/artwork/poster"
	}
	if ss.HasBackdrop && ss.RepresentativeItemID != "" {
		dto.BackdropURL = "/api/v1/items/" + ss.RepresentativeItemID + "/artwork/backdrop"
	}
	return dto
}

func registerSeriesRoutes(api huma.API, libSvc *library.Service, authSvc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "list-series",
		Method:      http.MethodGet,
		Path:        "/api/v1/series",
		Summary:     "List TV series grouped from indexed episodes",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *ListSeriesInput) (*ListSeriesResponse, error) {
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
		list, err := libSvc.ListSeriesFiltered(ctx, store.SeriesListFilter{
			LibraryID:         in.LibraryID,
			AllowedLibraryIDs: allowedIDs,
			RestrictLibraries: !unrestricted,
			UserID:            user.ID,
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
		})
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list series", err)
		}
		out := &ListSeriesResponse{}
		out.Body.Series = make([]SeriesSummaryDTO, 0, len(list))
		for _, ss := range list {
			out.Body.Series = append(out.Body.Series, toSeriesSummaryDTO(ss))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-series-detail",
		Method:      http.MethodGet,
		Path:        "/api/v1/series/detail",
		Summary:     "Get seasons, episodes, watch progress, and Next Up for a TV series",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *SeriesDetailInput) (*SeriesDetailResponse, error) {
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
		eps, err := libSvc.ListSeriesEpisodes(ctx, in.LibraryID, in.Title, user.ID, allowedIDs, !unrestricted)
		if err != nil {
			return nil, mapLibraryError(err)
		}

		out := &SeriesDetailResponse{}
		out.Body.Genres = []string{}
		out.Body.Studios = []string{}
		out.Body.Cast = []CreditPersonDTO{}
		out.Body.LibraryID = eps[0].Item.LibraryID
		out.Body.RepresentativeItemID = eps[0].Item.ID
		if lib, libErr := libSvc.GetLibrary(ctx, eps[0].Item.LibraryID); libErr == nil {
			out.Body.LibraryKind = lib.Kind
		}
		out.Body.Title = eps[0].Item.SeriesTitle
		out.Body.EpisodeCount = len(eps)

		if inWL, isFav, markErr := authSvc.GetItemMarks(ctx, user.ID, eps[0].Item.ID); markErr == nil {
			out.Body.InWatchlist = inWL
			out.Body.IsFavorite = isFav
		}

		seasonIdx := make(map[int]int)
		var nextUp *MediaItemSummaryDTO
		watchedCount := 0

		for _, ep := range eps {
			summary := toMediaSummaryDTO(ep.Item)
			if out.Body.OriginalTitle == "" && ep.Item.OriginalTitle != "" {
				out.Body.OriginalTitle = ep.Item.OriginalTitle
			}
			if out.Body.Tagline == "" && ep.Item.Tagline != "" {
				out.Body.Tagline = ep.Item.Tagline
			}
			if out.Body.Overview == "" && ep.Item.Overview != "" {
				out.Body.Overview = ep.Item.Overview
			}
			if out.Body.ContentRating == "" && ep.Item.ContentRating != "" {
				out.Body.ContentRating = ep.Item.ContentRating
			}
			if len(out.Body.Genres) == 0 {
				if g := decodeStringList(ep.Item.GenresJSON); len(g) > 0 {
					out.Body.Genres = g
				}
			}
			if len(out.Body.Studios) == 0 {
				if st := decodeStringList(ep.Item.StudiosJSON); len(st) > 0 {
					out.Body.Studios = st
				}
			}
			if len(out.Body.Cast) == 0 {
				if c := decodeCreditPeople(ep.Item.CastJSON); len(c) > 0 {
					out.Body.Cast = c
				}
			}
			if out.Body.TMDBID == "" && ep.Item.TMDBID != "" {
				out.Body.TMDBID = ep.Item.TMDBID
			}
			if out.Body.IMDBID == "" && ep.Item.IMDBID != "" {
				out.Body.IMDBID = ep.Item.IMDBID
			}
			if summary.DualAudio {
				out.Body.DualAudio = true
			}
			if out.Body.Year == nil && ep.Item.Year != nil {
				out.Body.Year = ep.Item.Year
			}
			if out.Body.PosterURL == "" && summary.PosterURL != "" {
				out.Body.PosterURL = summary.PosterURL
			}
			if out.Body.BackdropURL == "" && summary.BackdropURL != "" {
				out.Body.BackdropURL = summary.BackdropURL
			}

			var progDTO *ProgressDTO
			completed := false
			if ep.Progress != nil {
				completed = ep.Progress.Completed
				if completed {
					watchedCount++
				}
				progDTO = &ProgressDTO{
					ItemID:       ep.Progress.ItemID,
					PositionMS:   ep.Progress.PositionMS,
					DurationMS:   ep.Progress.DurationMS,
					Completed:    ep.Progress.Completed,
					PlayCount:    ep.Progress.PlayCount,
					LastPlayedAt: ep.Progress.LastPlayedAt,
				}
			}

			// First unwatched episode in season/episode order is Next Up.
			if nextUp == nil && !completed {
				cp := summary
				nextUp = &cp
			}

			sNum := 0
			if ep.Item.SeasonNumber != nil {
				sNum = *ep.Item.SeasonNumber
			}
			idx, exists := seasonIdx[sNum]
			if !exists {
				idx = len(out.Body.Seasons)
				seasonIdx[sNum] = idx
				out.Body.Seasons = append(out.Body.Seasons, SeriesSeasonDTO{SeasonNumber: sNum})
			}
			out.Body.Seasons[idx].Episodes = append(out.Body.Seasons[idx].Episodes, SeriesEpisodeDTO{
				Item:     summary,
				Progress: progDTO,
			})
		}

		// If every episode is completed, default Next Up to the pilot so Play works immediately.
		if nextUp == nil && len(eps) > 0 {
			cp := toMediaSummaryDTO(eps[0].Item)
			nextUp = &cp
		}

		out.Body.SeasonCount = len(out.Body.Seasons)
		out.Body.WatchedCount = watchedCount
		out.Body.NextUp = nextUp
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "set-series-watched",
		Method:      http.MethodPost,
		Path:        "/api/v1/series/watched",
		Summary:     "Mark an entire series or season as watched or unwatched for the current user",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *SeriesWatchedInput) (*SeriesBulkResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, in.Body.LibraryID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		n, err := libSvc.SetSeriesWatched(ctx, user.ID, in.Body.LibraryID, in.Body.Title, in.Body.SeasonNumber, in.Body.Completed)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		out := &SeriesBulkResponse{}
		out.Body.Updated = n
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "refresh-series-metadata",
		Method:      http.MethodPost,
		Path:        "/api/v1/series/refresh",
		Summary:     "Re-probe and re-enrich all episodes in a series, with optional title/year re-match (admin only)",
		Tags:        []string{"Media"},
	}, func(ctx context.Context, in *SeriesRefreshInput) (*SeriesBulkResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		n, resolvedTitle, err := libSvc.RefreshSeries(ctx, in.Body.LibraryID, in.Body.Title, in.Body.OverrideTitle, in.Body.OverrideYear, in.Body.TMDBID)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		out := &SeriesBulkResponse{}
		out.Body.Updated = n
		out.Body.Title = resolvedTitle
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-next-episode",
		Method:      http.MethodGet,
		Path:        "/api/v1/items/{id}/next",
		Summary:     "Get the next sequential episode for Up Next auto-advance",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *ItemIDInput) (*NextEpisodeResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		cur, err := libSvc.GetItem(ctx, in.ID)
		if err != nil {
			return nil, mapLibraryError(err)
		}
		allowed, err := authSvc.CanAccessLibrary(ctx, user, cur.LibraryID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to check library access", err)
		}
		if !allowed {
			return nil, huma.Error403Forbidden("access to this library is restricted")
		}
		next, err := libSvc.GetNextEpisode(ctx, in.ID)
		if err != nil {
			if errors.Is(err, library.ErrNotFound) {
				return &NextEpisodeResponse{}, nil
			}
			return nil, huma.Error500InternalServerError("failed to resolve next episode", err)
		}
		dto := toMediaSummaryDTO(next)
		out := &NextEpisodeResponse{}
		out.Body.NextItem = &dto
		return out, nil
	})
}
