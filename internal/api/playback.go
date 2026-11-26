package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/LuneServer/lune/internal/stream"
)

type PlaybackRequestInput struct {
	ID   string `path:"id"`
	Body struct {
		Containers            []string `json:"containers,omitempty" example:"[\"mp4\",\"webm\"]"`
		VideoCodecs           []string `json:"video_codecs,omitempty" example:"[\"h264\",\"hevc\"]"`
		AudioCodecs           []string `json:"audio_codecs,omitempty" example:"[\"aac\",\"opus\"]"`
		Supports10BitH264     *bool    `json:"supports_10bit_h264,omitempty"`
		Supports10BitHEVC     *bool    `json:"supports_10bit_hevc,omitempty"`
		SupportsHDR           *bool    `json:"supports_hdr,omitempty"`
		SupportsDoVi          *bool    `json:"supports_dovi,omitempty"`
		MaxAudioChannels      int      `json:"max_audio_channels,omitempty"`
		MaxBitrate            int64    `json:"max_bitrate,omitempty"`
		MaxWidth              int      `json:"max_width,omitempty"`
		MaxHeight             int      `json:"max_height,omitempty"`
		AudioStreamIndex      *int     `json:"audio_stream_index,omitempty"`
		BurnSubtitleStreamIdx *int     `json:"burn_subtitle_stream_index,omitempty"`
		StartPositionMS       *int64   `json:"start_position_ms,omitempty"`
		ForceTranscode        bool     `json:"force_transcode,omitempty"`
		ClientName            string   `json:"client_name,omitempty"`
	}
}

type PlaybackPlanResponse struct {
	Body stream.PlaybackPlan
}

type ProgressDTO struct {
	ItemID         string    `json:"item_id"`
	PositionMS     int64     `json:"position_ms"`
	DurationMS     int64     `json:"duration_ms"`
	Completed      bool      `json:"completed"`
	PlayCount      int       `json:"play_count"`
	LastPlayedAt   time.Time `json:"last_played_at,omitempty"`
	SessionStopped bool      `json:"session_stopped,omitempty"`
}

type UpdateProgressInput struct {
	ID   string `path:"id"`
	Body struct {
		PositionMS int64  `json:"position_ms" minimum:"0"`
		Completed  *bool  `json:"completed,omitempty"`
		SessionID  string `json:"session_id,omitempty"`
	}
}

type ProgressResponse struct {
	Body ProgressDTO
}

type ContinueWatchingItemDTO struct {
	Item     MediaItemSummaryDTO `json:"item"`
	Progress ProgressDTO         `json:"progress"`
}

type ContinueWatchingInput struct {
	Limit int `query:"limit" default:"20" minimum:"1" maximum:"50"`
}

type ContinueWatchingResponse struct {
	Body struct {
		Items []ContinueWatchingItemDTO `json:"items"`
	}
}

type NextUpItemDTO struct {
	Item         MediaItemSummaryDTO `json:"item"`
	LastPlayedAt time.Time           `json:"last_played_at"`
}

type NextUpInput struct {
	Limit int `query:"limit" default:"20" minimum:"1" maximum:"50"`
}

type NextUpResponse struct {
	Body struct {
		Items []NextUpItemDTO `json:"items"`
	}
}

type ActiveStreamsResponse struct {
	Body struct {
		Streams []stream.ActiveStream `json:"streams"`
	}
}

type StopPlaybackSessionInput struct {
	ID string `path:"id"`
}

type DirectLinkResponse struct {
	Body stream.DirectLink
}

type StreamingConfigResponse struct {
	Body stream.StreamingConfig
}

type UpdateStreamingConfigInput struct {
	Body struct {
		MaxGlobalTranscodes int `json:"max_global_transcodes,omitempty"`
		MaxUserTranscodes   int `json:"max_user_transcodes,omitempty"`
		IdleTimeoutMinutes  int `json:"idle_timeout_minutes,omitempty"`
	}
}

func registerPlaybackRoutes(r chi.Router, api huma.API, streamSvc *stream.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "negotiate-playback",
		Method:      http.MethodPost,
		Path:        "/api/v1/items/{id}/playback",
		Summary:     "Negotiate playback mode and issue a short-lived signed stream URL",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *PlaybackRequestInput) (*PlaybackPlanResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		plan, err := streamSvc.Negotiate(ctx, user, in.ID, stream.ClientCapabilities{
			Containers:            in.Body.Containers,
			VideoCodecs:           in.Body.VideoCodecs,
			AudioCodecs:           in.Body.AudioCodecs,
			Supports10BitH264:     in.Body.Supports10BitH264,
			Supports10BitHEVC:     in.Body.Supports10BitHEVC,
			SupportsHDR:           in.Body.SupportsHDR,
			SupportsDoVi:          in.Body.SupportsDoVi,
			MaxAudioChannels:      in.Body.MaxAudioChannels,
			MaxBitrate:            in.Body.MaxBitrate,
			MaxWidth:              in.Body.MaxWidth,
			MaxHeight:             in.Body.MaxHeight,
			AudioStreamIndex:      in.Body.AudioStreamIndex,
			BurnSubtitleStreamIdx: in.Body.BurnSubtitleStreamIdx,
			StartPositionMS:       in.Body.StartPositionMS,
			ForceTranscode:        in.Body.ForceTranscode,
			ClientName:            in.Body.ClientName,
		}, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapStreamError(err)
		}
		return &PlaybackPlanResponse{Body: plan}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "create-direct-link",
		Method:      http.MethodPost,
		Path:        "/api/v1/items/{id}/direct-link",
		Summary:     "Generate short-lived signed direct stream and download URLs for an item",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *ItemIDInput) (*DirectLinkResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		link, err := streamSvc.CreateDirectLink(ctx, user, in.ID)
		if err != nil {
			return nil, mapStreamError(err)
		}
		return &DirectLinkResponse{Body: link}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "update-playback-progress",
		Method:      http.MethodPut,
		Path:        "/api/v1/items/{id}/progress",
		Summary:     "Update watch progress and resume position for the current user",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *UpdateProgressInput) (*ProgressResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		prog, err := streamSvc.UpdateProgress(ctx, user, in.ID, in.Body.PositionMS, in.Body.Completed)
		if err != nil {
			return nil, mapStreamError(err)
		}
		return &ProgressResponse{
			Body: ProgressDTO{
				ItemID:         prog.ItemID,
				PositionMS:     prog.PositionMS,
				DurationMS:     prog.DurationMS,
				Completed:      prog.Completed,
				PlayCount:      prog.PlayCount,
				LastPlayedAt:   prog.LastPlayedAt,
				SessionStopped: streamSvc.IsSessionRevoked(in.Body.SessionID),
			},
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-playback-progress",
		Method:      http.MethodGet,
		Path:        "/api/v1/items/{id}/progress",
		Summary:     "Get watch progress for the current user",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *ItemIDInput) (*ProgressResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		prog, err := streamSvc.GetProgress(ctx, user, in.ID)
		if err != nil {
			return nil, mapStreamError(err)
		}
		return &ProgressResponse{
			Body: ProgressDTO{
				ItemID:       prog.ItemID,
				PositionMS:   prog.PositionMS,
				DurationMS:   prog.DurationMS,
				Completed:    prog.Completed,
				PlayCount:    prog.PlayCount,
				LastPlayedAt: prog.LastPlayedAt,
			},
		}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "clear-playback-progress",
		Method:        http.MethodDelete,
		Path:          "/api/v1/items/{id}/progress",
		Summary:       "Clear watch progress and remove an item from Continue Watching for the current user",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Playback"},
	}, func(ctx context.Context, in *ItemIDInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := streamSvc.ClearProgress(ctx, user, in.ID); err != nil {
			return nil, mapStreamError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-continue-watching",
		Method:      http.MethodGet,
		Path:        "/api/v1/playback/continue",
		Summary:     "List in-progress items for the Continue Watching shelf",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *ContinueWatchingInput) (*ContinueWatchingResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		entries, err := streamSvc.ContinueWatching(ctx, user, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load continue watching", err)
		}
		out := &ContinueWatchingResponse{}
		out.Body.Items = make([]ContinueWatchingItemDTO, 0, len(entries))
		for _, e := range entries {
			out.Body.Items = append(out.Body.Items, ContinueWatchingItemDTO{
				Item: toMediaSummaryDTO(e.Item),
				Progress: ProgressDTO{
					ItemID:       e.Progress.ItemID,
					PositionMS:   e.Progress.PositionMS,
					DurationMS:   e.Progress.DurationMS,
					Completed:    e.Progress.Completed,
					PlayCount:    e.Progress.PlayCount,
					LastPlayedAt: e.Progress.LastPlayedAt,
				},
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "list-next-up",
		Method:      http.MethodGet,
		Path:        "/api/v1/playback/next-up",
		Summary:     "List the next unwatched episode for each recently watched series",
		Tags:        []string{"Playback"},
	}, func(ctx context.Context, in *NextUpInput) (*NextUpResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		entries, err := streamSvc.NextUp(ctx, user, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load next up", err)
		}
		out := &NextUpResponse{}
		out.Body.Items = make([]NextUpItemDTO, 0, len(entries))
		for _, e := range entries {
			out.Body.Items = append(out.Body.Items, NextUpItemDTO{
				Item:         toMediaSummaryDTO(e.Item),
				LastPlayedAt: e.LastPlayedAt,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "stop-playback-session",
		Method:        http.MethodDelete,
		Path:          "/api/v1/playback/sessions/{id}",
		Summary:       "Stop an active playback or transcode session",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Playback"},
	}, func(ctx context.Context, in *StopPlaybackSessionInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := streamSvc.StopSessionForUser(in.ID, user); err != nil {
			return nil, mapStreamError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-streams",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/streams",
		Summary:     "List currently active media streams (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*ActiveStreamsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		out := &ActiveStreamsResponse{}
		out.Body.Streams = streamSvc.ListActiveStreams()
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-get-streaming-config",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/streaming/config",
		Summary:     "Get live transcoding concurrency and idle timeout settings (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*StreamingConfigResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		return &StreamingConfigResponse{Body: streamSvc.GetStreamingConfig()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-update-streaming-config",
		Method:      http.MethodPut,
		Path:        "/api/v1/admin/streaming/config",
		Summary:     "Update live transcoding concurrency and idle timeout settings (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UpdateStreamingConfigInput) (*StreamingConfigResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		updated, err := streamSvc.UpdateStreamingConfig(ctx, user, stream.StreamingConfig{
			MaxGlobalTranscodes: in.Body.MaxGlobalTranscodes,
			MaxUserTranscodes:   in.Body.MaxUserTranscodes,
			IdleTimeoutMinutes:  in.Body.IdleTimeoutMinutes,
		}, clientIPFromContext(ctx))
		if err != nil {
			if errors.Is(err, stream.ErrInvalidStreamingConfig) {
				return nil, huma.Error422UnprocessableEntity(err.Error())
			}
			return nil, huma.Error500InternalServerError("failed to update streaming configuration", err)
		}
		return &StreamingConfigResponse{Body: updated}, nil
	})

	// Signed binary media streaming routes (no cookie/header required; HMAC token authorizes stream).
	r.Get("/stream/direct/{token}", func(w http.ResponseWriter, req *http.Request) {
		streamSvc.ServeDirect(w, req, chi.URLParam(req, "token"))
	})
	r.Get("/stream/hls/{token}/{file}", func(w http.ResponseWriter, req *http.Request) {
		streamSvc.ServeHLS(w, req, chi.URLParam(req, "token"), chi.URLParam(req, "file"))
	})
	r.Get("/stream/sub/{token}/{file}", func(w http.ResponseWriter, req *http.Request) {
		streamSvc.ServeSubtitle(w, req, chi.URLParam(req, "token"), chi.URLParam(req, "file"))
	})
}

func mapStreamError(err error) error {
	switch {
	case errors.Is(err, stream.ErrItemNotFound):
		return huma.Error404NotFound("media item not found")
	case errors.Is(err, stream.ErrFileMissing):
		return huma.Error404NotFound("media file is unavailable on disk")
	case errors.Is(err, stream.ErrForbidden):
		return huma.Error403Forbidden("access to this media resource is restricted")
	case errors.Is(err, stream.ErrTooManyTranscodes):
		return huma.Error429TooManyRequests("too many concurrent transcode sessions")
	default:
		return huma.Error500InternalServerError("playback error")
	}
}
