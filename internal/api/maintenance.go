package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/maintenance"
	"github.com/LuneServer/lune/internal/store"
)

type MaintenanceStatusResponse struct {
	Body maintenance.Status
}

type UpdateMaintenanceConfigInput struct {
	Body maintenance.Config
}

func registerMaintenanceAdminRoutes(api huma.API, maintSvc *maintenance.Service, db *store.Store) {
	if maintSvc == nil {
		return
	}

	huma.Register(api, huma.Operation{
		OperationID: "admin-get-maintenance",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/maintenance",
		Summary:     "Get periodic housekeeping configuration, schedule, and last run report",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*MaintenanceStatusResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("admin privileges required")
		}
		return &MaintenanceStatusResponse{Body: maintSvc.Status()}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-update-maintenance-config",
		Method:      http.MethodPut,
		Path:        "/api/v1/admin/maintenance/config",
		Summary:     "Update periodic housekeeping settings and reschedule live",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UpdateMaintenanceConfigInput) (*MaintenanceStatusResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("admin privileges required")
		}
		st, err := maintSvc.UpdateConfig(ctx, in.Body)
		if err != nil {
			if errors.Is(err, maintenance.ErrInvalidConfig) {
				return nil, huma.Error400BadRequest(err.Error())
			}
			return nil, huma.Error500InternalServerError("failed to save maintenance settings", err)
		}
		if db != nil {
			_ = db.InsertAuditEvent(ctx, store.AuditEvent{
				EventType: "admin.maintenance_config_updated",
				ActorID:   user.ID,
				ClientIP:  clientIPFromContext(ctx),
				Detail: fmt.Sprintf("enabled=%t interval=%dh time=%s artwork=%t(%dh) transcode=%t(%dd) audit=%t(%dd) db_opt=%t",
					in.Body.Enabled, in.Body.IntervalHours, in.Body.ScheduleTime,
					in.Body.CleanupOrphanedArtwork, in.Body.ArtworkGraceHours,
					in.Body.CleanupTranscodeCache, in.Body.SubtitleCacheDays,
					in.Body.PruneAuditLogs, in.Body.AuditRetentionDays,
					in.Body.OptimizeDatabase),
			})
		}
		return &MaintenanceStatusResponse{Body: st}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-run-maintenance",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/maintenance/run",
		Summary:     "Run a housekeeping maintenance pass immediately",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*MaintenanceStatusResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("admin privileges required")
		}
		st, err := maintSvc.RunNow(ctx)
		if err != nil {
			if errors.Is(err, maintenance.ErrAlreadyRunning) {
				return nil, huma.Error409Conflict("maintenance run already in progress")
			}
			return nil, huma.Error500InternalServerError("maintenance run failed", err)
		}
		if db != nil {
			detail := "manual housekeeping completed"
			if st.LastRun != nil {
				detail = fmt.Sprintf("artwork=%d transcode=%d audit_pruned=%d db_opt=%t",
					st.LastRun.ArtworkFilesRemoved,
					st.LastRun.TranscodeItemsRemoved,
					st.LastRun.AuditEventsPruned,
					st.LastRun.DatabaseOptimized)
			}
			_ = db.InsertAuditEvent(ctx, store.AuditEvent{
				EventType: "admin.maintenance_run",
				ActorID:   user.ID,
				ClientIP:  clientIPFromContext(ctx),
				Detail:    detail,
			})
		}
		return &MaintenanceStatusResponse{Body: st}, nil
	})
}
