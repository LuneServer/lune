package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/config"
	"github.com/LuneServer/lune/internal/library"
	"github.com/LuneServer/lune/internal/maintenance"
	"github.com/LuneServer/lune/internal/requests"
	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
	"github.com/LuneServer/lune/internal/stream"
	"github.com/LuneServer/lune/internal/version"
	"github.com/LuneServer/lune/internal/web"
)

// Dependencies wires runtime services into the HTTP API layer.
type Dependencies struct {
	Config      config.Config
	Store       *store.Store
	Auth        *auth.Service
	Library     *library.Service
	Stream      *stream.Service
	Requests    *requests.Service
	Maintenance *maintenance.Service
	Logger      *slog.Logger
	StartedAt   time.Time
}

type HealthOutput struct {
	Body struct {
		Status        string `json:"status" example:"ok"`
		Version       string `json:"version" example:"0.1.0-dev"`
		EdgeMode      string `json:"edge_mode" example:"dev"`
		DatabaseOK    bool   `json:"database_ok"`
		UptimeSeconds int64  `json:"uptime_seconds"`
	}
}

type SystemInfoOutput struct {
	Body struct {
		Brand           string   `json:"brand" example:"Lune"`
		Tagline         string   `json:"tagline" example:"Your universe. Your media."`
		Version         string   `json:"version" example:"0.1.0-dev"`
		EdgeMode        string   `json:"edge_mode" example:"local"`
		PublicURL       string   `json:"public_url,omitempty"`
		SetupRequired   bool     `json:"setup_required"`
		RequestsEnabled bool     `json:"requests_enabled"`
		AuthMethods     []string `json:"auth_methods"`
	}
}

// NewHandler constructs the chi router with Huma OpenAPI 3.1 endpoints.
func NewHandler(deps Dependencies) http.Handler {
	if deps.Auth == nil {
		deps.Auth = auth.NewService(deps.Store, nil)
	}
	if deps.Config.DataDir != "" {
		deps.Auth.SetDataDir(deps.Config.DataDir)
	}
	if deps.Library == nil {
		deps.Library = library.NewService(deps.Store, nil, nil)
	}
	if deps.Stream == nil {
		deps.Stream, _ = stream.NewService(context.Background(), deps.Store, deps.Config.DataDir, nil)
	}
	if deps.Requests == nil && deps.Store != nil {
		deps.Requests = requests.NewService(deps.Store, deps.Config, nil, nil)
	}
	if deps.Maintenance == nil && deps.Store != nil {
		deps.Maintenance = maintenance.NewService(context.Background(), deps.Store, maintenance.Options{
			DataDir:             deps.Config.DataDir,
			IsScanRunning:       deps.Library.HasRunningScan,
			ActiveTranscodeDirs: deps.Stream.ActiveTranscodeDirs,
			Logger:              deps.Logger,
		})
	}
	ipResolver := security.NewClientIPResolver(deps.Config.TrustedProxies)
	edgeGuard := security.NewEdgeGuard()
	edgeGuard.UpdateLists(deps.Config.IPAllowlist, deps.Config.IPBlocklist)
	if deps.Store != nil {
		if persisted, err := deps.Store.ListActiveIPBans(context.Background()); err == nil && len(persisted) > 0 {
			loaded := make([]security.IPBan, 0, len(persisted))
			for _, b := range persisted {
				loaded = append(loaded, security.IPBan{
					IP:        b.IP,
					Reason:    b.Reason,
					BannedAt:  b.BannedAt,
					ExpiresAt: b.ExpiresAt,
					Permanent: b.Permanent,
				})
			}
			edgeGuard.LoadBans(loaded)
		}
	}
	edgeGuard.SetOnBan(func(ip, reason string, dur time.Duration, permanent bool) {
		if deps.Logger != nil {
			deps.Logger.Warn("security.ip_banned", "ip", ip, "reason", reason, "duration", dur.String(), "permanent", permanent)
		}
		if deps.Store != nil {
			now := time.Now().UTC()
			var exp *time.Time
			if !permanent && dur > 0 {
				t := now.Add(dur)
				exp = &t
			}
			_ = deps.Store.UpsertIPBan(context.Background(), store.StoredIPBan{
				IP:        ip,
				Reason:    reason,
				BannedAt:  now,
				ExpiresAt: exp,
				Permanent: permanent,
			})
			detail := reason
			if permanent {
				detail += " (permanent)"
			} else if dur > 0 {
				detail += fmt.Sprintf(" (for %s)", dur.Round(time.Second))
			}
			_ = deps.Store.InsertAuditEvent(context.Background(), store.AuditEvent{
				EventType: "security.ip_banned",
				ClientIP:  ip,
				Detail:    detail,
			})
		}
	})
	edgeGuard.SetOnUnban(func(ip string) {
		if deps.Store != nil {
			_ = deps.Store.DeleteIPBan(context.Background(), ip)
		}
	})
	deps.Auth.SetOnLockout(func(ip, reason string) {
		edgeGuard.RecordAbuse(ip, reason, 5)
	})
	runtimeNet := newRuntimeNetwork(deps.Config, ipResolver, edgeGuard)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.Recoverer)
	r.Use(security.BodyLimitMiddleware)
	r.Use(security.HeadersMiddleware(ipResolver))
	r.Use(security.EdgeGuardMiddleware(edgeGuard, ipResolver, deps.Logger))
	r.Use(security.CompressMiddleware)
	r.Use(requestMetaMiddleware(ipResolver))
	r.Use(adminLANOnlyMiddleware(runtimeNet, ipResolver, edgeGuard))
	r.Use(csrfMiddleware(runtimeNet))
	r.Use(optionalAuthMiddleware(deps.Auth))
	r.Use(totpEnrollmentGateMiddleware())

	humaCfg := huma.DefaultConfig("Lune Server API", version.Version)
	humaCfg.Info.Description = "Self-hosted media platform API for Lune web and native clients."
	if os.Getenv("LUNE_ENABLE_API_DOCS") != "true" {
		humaCfg.OpenAPIPath = ""
		humaCfg.DocsPath = ""
	}
	api := humachi.New(r, humaCfg)

	huma.Register(api, huma.Operation{
		OperationID: "get-health",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Server health check",
		Tags:        []string{"System"},
	}, func(ctx context.Context, _ *struct{}) (*HealthOutput, error) {
		dbErr := deps.Store.Ping(ctx)
		out := &HealthOutput{}
		out.Body.Status = "ok"
		if dbErr != nil {
			out.Body.Status = "degraded"
		}
		out.Body.Version = version.Version
		out.Body.EdgeMode = string(runtimeNet.EffectiveMode())
		out.Body.DatabaseOK = dbErr == nil
		out.Body.UptimeSeconds = int64(time.Since(deps.StartedAt).Seconds())
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "get-system-info",
		Method:      http.MethodGet,
		Path:        "/api/v1/system/info",
		Summary:     "Public server discovery metadata",
		Tags:        []string{"System"},
	}, func(ctx context.Context, _ *struct{}) (*SystemInfoOutput, error) {
		hasUsers, err := deps.Store.HasUsers(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to inspect server state", err)
		}
		out := &SystemInfoOutput{}
		out.Body.Brand = "Lune"
		out.Body.Tagline = "Your universe. Your media."
		out.Body.Version = version.Version
		out.Body.EdgeMode = string(runtimeNet.EffectiveMode())
		out.Body.PublicURL = runtimeNet.PublicURL()
		out.Body.SetupRequired = !hasUsers
		out.Body.RequestsEnabled = deps.Requests != nil && deps.Requests.IsEnabled(ctx)
		out.Body.AuthMethods = []string{"password", "device_code"}
		return out, nil
	})

	registerAuthRoutes(api, runtimeNet, deps.Auth)
	registerUserRoutes(r, api, deps.Auth)
	registerUserAdminRoutes(api, deps.Auth)
	registerInviteRoutes(api, runtimeNet, deps.Auth)
	registerNetworkAdminRoutes(api, deps.Config, deps.Store, runtimeNet)
	registerMaintenanceAdminRoutes(api, deps.Maintenance, deps.Store)
	registerLibraryRoutes(r, api, deps.Library, deps.Auth, deps.Stream, deps.Config.DataDir)
	registerSeriesRoutes(api, deps.Library, deps.Auth)
	registerRequestRoutes(api, deps.Requests)
	if deps.Stream != nil {
		registerPlaybackRoutes(r, api, deps.Stream)
	}
	if deps.Config.WebDir != "" {
		r.NotFound(web.NewSPAHandler(deps.Config.WebDir).ServeHTTP)
	}

	return r
}
