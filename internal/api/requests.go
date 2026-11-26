package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/metadata"
	"github.com/LuneServer/lune/internal/requests"
	"github.com/LuneServer/lune/internal/store"
)

type MediaRequestDTO struct {
	ID              string     `json:"id"`
	UserID          string     `json:"user_id"`
	Username        string     `json:"username"`
	DisplayName     string     `json:"display_name"`
	MediaType       string     `json:"media_type"`
	IsAnime         bool       `json:"is_anime"`
	TMDBID          int        `json:"tmdb_id"`
	TVDBID          int        `json:"tvdb_id,omitempty"`
	Title           string     `json:"title"`
	OriginalTitle   string     `json:"original_title,omitempty"`
	Year            *int       `json:"year,omitempty"`
	Overview        string     `json:"overview,omitempty"`
	PosterURL       string     `json:"poster_url,omitempty"`
	BackdropURL     string     `json:"backdrop_url,omitempty"`
	Status          string     `json:"status"`
	StatusNote      string     `json:"status_note,omitempty"`
	ExternalService string     `json:"external_service,omitempty"`
	ExternalID      int        `json:"external_id,omitempty"`
	ReviewedBy      string     `json:"reviewed_by,omitempty"`
	ReviewedAt      *time.Time `json:"reviewed_at,omitempty"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

type DiscoverInput struct {
	Tab       string  `query:"tab" default:"trending" enum:"trending,movies,tv,anime"`
	Query     string  `query:"q"`
	Page      int     `query:"page" default:"1" minimum:"1" maximum:"500"`
	Sort      string  `query:"sort"`
	Genre     int     `query:"genre" minimum:"0"`
	Year      int     `query:"year" minimum:"0" maximum:"2200"`
	DateFrom  string  `query:"date_from" maxLength:"10"`
	DateTo    string  `query:"date_to" maxLength:"10"`
	MinRating float64 `query:"min_rating" minimum:"0" maximum:"10"`
	Language  string  `query:"language" maxLength:"8"`
}

type DiscoverResponse struct {
	Body requests.DiscoverResult
}

type ListRequestsInput struct {
	Scope  string `query:"scope"`
	Status string `query:"status"`
	Limit  int    `query:"limit" default:"100" minimum:"1" maximum:"200"`
}

type ListRequestsResponse struct {
	Body struct {
		Requests     []MediaRequestDTO `json:"requests"`
		PendingCount int               `json:"pending_count"`
	}
}

type CreateRequestInput struct {
	Body struct {
		MediaType string `json:"media_type" enum:"movie,tv"`
		TMDBID    int    `json:"tmdb_id" minimum:"1"`
		IsAnime   *bool  `json:"is_anime,omitempty"`
	}
}

type MediaRequestResponse struct {
	Body MediaRequestDTO
}

type RequestIDInput struct {
	ID string `path:"id"`
}

type ApproveRequestInput struct {
	ID   string `path:"id"`
	Body struct {
		IsAnime *bool `json:"is_anime,omitempty"`
	}
}

type DeclineRequestInput struct {
	ID   string `path:"id"`
	Body struct {
		Note string `json:"note,omitempty" maxLength:"500"`
	}
}

type RequestsConfigResponse struct {
	Body requests.RequestsConfig
}

type SaveRequestsConfigInput struct {
	Body requests.RequestsConfig
}

type TestArrInput struct {
	Body struct {
		Service string `json:"service" enum:"radarr,sonarr"`
		URL     string `json:"url"`
		APIKey  string `json:"api_key,omitempty"`
		Slot    string `json:"slot,omitempty"`
	}
}

type TestArrResponse struct {
	Body requests.ArrTestResult
}

func registerRequestRoutes(api huma.API, reqSvc *requests.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "requests-discover",
		Method:      http.MethodGet,
		Path:        "/api/v1/requests/discover",
		Summary:     "Browse trending/popular TMDB titles or search for media to request",
		Tags:        []string{"Requests"},
	}, func(ctx context.Context, in *DiscoverInput) (*DiscoverResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if reqSvc == nil {
			return nil, huma.Error404NotFound("media requests are not enabled")
		}
		res, err := reqSvc.Discover(ctx, user, in.Tab, in.Query, in.Page, metadata.DiscoverFilter{
			Sort:      in.Sort,
			Genre:     in.Genre,
			Year:      in.Year,
			DateFrom:  in.DateFrom,
			DateTo:    in.DateTo,
			MinRating: in.MinRating,
			Language:  in.Language,
		})
		if err != nil {
			return nil, mapRequestError(err)
		}
		return &DiscoverResponse{Body: res}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "requests-list",
		Method:      http.MethodGet,
		Path:        "/api/v1/requests",
		Summary:     "List media requests for the current user (or all users for admins)",
		Tags:        []string{"Requests"},
	}, func(ctx context.Context, in *ListRequestsInput) (*ListRequestsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if reqSvc == nil {
			out := &ListRequestsResponse{}
			out.Body.Requests = []MediaRequestDTO{}
			return out, nil
		}
		items, pendingCount, err := reqSvc.ListRequests(ctx, user, in.Scope, in.Status, in.Limit)
		if err != nil {
			return nil, mapRequestError(err)
		}
		out := &ListRequestsResponse{}
		out.Body.Requests = make([]MediaRequestDTO, 0, len(items))
		isAdmin := user.Role == "admin"
		for _, r := range items {
			out.Body.Requests = append(out.Body.Requests, toMediaRequestDTO(r, isAdmin))
		}
		out.Body.PendingCount = pendingCount
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "requests-create",
		Method:        http.MethodPost,
		Path:          "/api/v1/requests",
		Summary:       "Submit a new movie or TV/anime request",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Requests"},
	}, func(ctx context.Context, in *CreateRequestInput) (*MediaRequestResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if reqSvc == nil {
			return nil, huma.Error404NotFound("media requests are not enabled")
		}
		created, err := reqSvc.CreateRequest(ctx, user, in.Body.MediaType, in.Body.TMDBID, in.Body.IsAnime, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapRequestError(err)
		}
		return &MediaRequestResponse{Body: toMediaRequestDTO(created, user.Role == "admin")}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "requests-delete",
		Method:        http.MethodDelete,
		Path:          "/api/v1/requests/{id}",
		Summary:       "Cancel a pending request (member) or delete any request (admin)",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Requests"},
	}, func(ctx context.Context, in *RequestIDInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if reqSvc == nil {
			return nil, huma.Error404NotFound("request not found")
		}
		if err := reqSvc.DeleteRequest(ctx, user, in.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapRequestError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-requests-approve",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/requests/{id}/approve",
		Summary:     "Approve a media request and dispatch to Radarr/Sonarr if configured (admin only)",
		Tags:        []string{"Admin", "Requests"},
	}, func(ctx context.Context, in *ApproveRequestInput) (*MediaRequestResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if reqSvc == nil {
			return nil, huma.Error404NotFound("request not found")
		}
		updated, err := reqSvc.ApproveRequest(ctx, actor, in.ID, in.Body.IsAnime, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapRequestError(err)
		}
		return &MediaRequestResponse{Body: toMediaRequestDTO(updated, true)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-requests-decline",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/requests/{id}/decline",
		Summary:     "Decline a media request with an optional note (admin only)",
		Tags:        []string{"Admin", "Requests"},
	}, func(ctx context.Context, in *DeclineRequestInput) (*MediaRequestResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if reqSvc == nil {
			return nil, huma.Error404NotFound("request not found")
		}
		updated, err := reqSvc.DeclineRequest(ctx, actor, in.ID, in.Body.Note, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapRequestError(err)
		}
		return &MediaRequestResponse{Body: toMediaRequestDTO(updated, true)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-requests-get-config",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/requests/config",
		Summary:     "Get media requests and Radarr/Sonarr integration configuration (admin only)",
		Tags:        []string{"Admin", "Requests"},
	}, func(ctx context.Context, _ *struct{}) (*RequestsConfigResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if reqSvc == nil {
			return &RequestsConfigResponse{Body: requests.RequestsConfig{}}, nil
		}
		cfg, err := reqSvc.GetConfig(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load requests config", err)
		}
		return &RequestsConfigResponse{Body: cfg}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-requests-save-config",
		Method:      http.MethodPut,
		Path:        "/api/v1/admin/requests/config",
		Summary:     "Update media requests and Radarr/Sonarr integration configuration (admin only)",
		Tags:        []string{"Admin", "Requests"},
	}, func(ctx context.Context, in *SaveRequestsConfigInput) (*RequestsConfigResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if reqSvc == nil {
			return nil, huma.Error500InternalServerError("requests service not initialized")
		}
		saved, err := reqSvc.SaveConfig(ctx, actor, in.Body, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapRequestError(err)
		}
		return &RequestsConfigResponse{Body: saved}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-requests-test-arr",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/requests/test",
		Summary:     "Test a Radarr or Sonarr v3 connection and list quality profiles & root folders (admin only)",
		Tags:        []string{"Admin", "Requests"},
	}, func(ctx context.Context, in *TestArrInput) (*TestArrResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if reqSvc == nil {
			return nil, huma.Error500InternalServerError("requests service not initialized")
		}
		res := reqSvc.TestArrConnection(ctx, in.Body.Service, in.Body.URL, in.Body.APIKey, in.Body.Slot)
		return &TestArrResponse{Body: res}, nil
	})
}

func toMediaRequestDTO(r store.MediaRequestWithUser, isAdmin bool) MediaRequestDTO {
	statusNote := r.StatusNote
	externalService := r.ExternalService
	externalID := r.ExternalID
	if !isAdmin {
		externalService = ""
		externalID = 0
		if r.Status == store.RequestStatusFailed {
			statusNote = "Unable to process request — please contact your server administrator"
		}
	}
	return MediaRequestDTO{
		ID:              r.ID,
		UserID:          r.UserID,
		Username:        r.Username,
		DisplayName:     r.DisplayName,
		MediaType:       r.MediaType,
		IsAnime:         r.IsAnime,
		TMDBID:          r.TMDBID,
		TVDBID:          r.TVDBID,
		Title:           r.Title,
		OriginalTitle:   r.OriginalTitle,
		Year:            r.Year,
		Overview:        r.Overview,
		PosterURL:       requests.PosterURL(r.PosterPath),
		BackdropURL:     requests.BackdropURL(r.BackdropPath),
		Status:          r.Status,
		StatusNote:      statusNote,
		ExternalService: externalService,
		ExternalID:      externalID,
		ReviewedBy:      r.ReviewedBy,
		ReviewedAt:      r.ReviewedAt,
		CreatedAt:       r.CreatedAt,
		UpdatedAt:       r.UpdatedAt,
	}
}

func mapRequestError(err error) error {
	switch {
	case errors.Is(err, requests.ErrDisabled):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, requests.ErrForbidden):
		return huma.Error403Forbidden(err.Error())
	case errors.Is(err, requests.ErrNotFound):
		return huma.Error404NotFound(err.Error())
	case errors.Is(err, requests.ErrAlreadyInLibrary),
		errors.Is(err, requests.ErrAlreadyRequested),
		errors.Is(err, requests.ErrConflict):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, requests.ErrValidation):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return huma.Error500InternalServerError("media request operation failed", err)
	}
}
