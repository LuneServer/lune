package api

import (
	"context"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/store"
)

type InviteDTO struct {
	ID           string     `json:"id"`
	TokenPrefix  string     `json:"token_prefix"`
	Label        string     `json:"label,omitempty"`
	Role         string     `json:"role"`
	AllLibraries bool       `json:"all_libraries"`
	RequireTOTP  bool       `json:"require_totp"`
	LibraryIDs   []string   `json:"library_ids"`
	MaxUses      int        `json:"max_uses"`
	UseCount     int        `json:"use_count"`
	Status       string     `json:"status"`
	CreatedBy    string     `json:"created_by,omitempty"`
	ExpiresAt    time.Time  `json:"expires_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type CreateInviteInput struct {
	Body struct {
		Label          string   `json:"label,omitempty" maxLength:"120"`
		Role           string   `json:"role,omitempty" enum:"admin,member" default:"member"`
		AllLibraries   *bool    `json:"all_libraries,omitempty"`
		LibraryIDs     []string `json:"library_ids,omitempty"`
		RequireTOTP    bool     `json:"require_totp,omitempty"`
		MaxUses        int      `json:"max_uses,omitempty" default:"1" minimum:"1" maximum:"100"`
		ExpiresInHours int      `json:"expires_in_hours,omitempty" default:"72" minimum:"1" maximum:"720"`
	}
}

type CreateInviteResponse struct {
	Body struct {
		Invite     InviteDTO `json:"invite"`
		Token      string    `json:"token"`
		InvitePath string    `json:"invite_path"`
	}
}

type InvitesListResponse struct {
	Body struct {
		Invites []InviteDTO `json:"invites"`
	}
}

type InviteIDInput struct {
	ID string `path:"id"`
}

type InviteTokenInput struct {
	Token string `path:"token"`
}

type InviteInspectResponse struct {
	Body struct {
		Valid         bool      `json:"valid"`
		Label         string    `json:"label,omitempty"`
		Role          string    `json:"role"`
		AllLibraries  bool      `json:"all_libraries"`
		RequireTOTP   bool      `json:"require_totp"`
		RemainingUses int       `json:"remaining_uses"`
		ExpiresAt     time.Time `json:"expires_at"`
	}
}

type AcceptInviteInput struct {
	Token string `path:"token"`
	Body  struct {
		Username    string `json:"username" minLength:"2" maxLength:"64"`
		DisplayName string `json:"display_name,omitempty" maxLength:"64"`
		Password    string `json:"password" minLength:"8" maxLength:"128"`
		ClientKind  string `json:"client_kind,omitempty" enum:"web,app" default:"web"`
		ClientName  string `json:"client_name,omitempty" maxLength:"64"`
	}
}

func registerInviteRoutes(api huma.API, runtimeNet *runtimeNetwork, authSvc *auth.Service) {

	huma.Register(api, huma.Operation{
		OperationID:   "admin-create-invite",
		Method:        http.MethodPost,
		Path:          "/api/v1/admin/invites",
		Summary:       "Create a time-limited invite token with role and library permissions (admin only)",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, in *CreateInviteInput) (*CreateInviteResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		created, err := authSvc.CreateInvite(ctx, actor, auth.CreateInviteRequest{
			Label:          in.Body.Label,
			Role:           in.Body.Role,
			AllLibraries:   in.Body.AllLibraries,
			LibraryIDs:     in.Body.LibraryIDs,
			RequireTOTP:    in.Body.RequireTOTP,
			MaxUses:        in.Body.MaxUses,
			ExpiresInHours: in.Body.ExpiresInHours,
		}, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &CreateInviteResponse{}
		out.Body.Invite = toInviteDTO(created.Invite, authSvc.Now())
		out.Body.Token = created.Token
		out.Body.InvitePath = "/invite/" + created.Token
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-invites",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/invites",
		Summary:     "List all user invitations (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*InvitesListResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		invites, err := authSvc.ListInvites(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list invites", err)
		}
		now := authSvc.Now()
		out := &InvitesListResponse{}
		out.Body.Invites = make([]InviteDTO, 0, len(invites))
		for _, inv := range invites {
			out.Body.Invites = append(out.Body.Invites, toInviteDTO(inv, now))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-clear-inactive-invites",
		Method:      http.MethodDelete,
		Path:        "/api/v1/admin/invites",
		Summary:     "Clear revoked, expired, and used invitations (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*struct {
		Body struct {
			Deleted int64 `json:"deleted"`
		}
	}, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		n, err := authSvc.ClearInactiveInvites(ctx, actor, clientIPFromContext(ctx))
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to clear inactive invites")
		}
		out := &struct {
			Body struct {
				Deleted int64 `json:"deleted"`
			}
		}{}
		out.Body.Deleted = n
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "admin-revoke-invite",
		Method:        http.MethodDelete,
		Path:          "/api/v1/admin/invites/{id}",
		Summary:       "Revoke an active invitation (admin only)",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, in *InviteIDInput) (*struct{}, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if err := authSvc.RevokeInvite(ctx, actor, in.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "invite-inspect",
		Method:      http.MethodGet,
		Path:        "/api/v1/invites/{token}",
		Summary:     "Inspect an invitation token before accepting it",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *InviteTokenInput) (*InviteInspectResponse, error) {
		inv, err := authSvc.InspectInvite(ctx, in.Token, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		rem := inv.MaxUses - inv.UseCount
		if rem < 0 {
			rem = 0
		}
		out := &InviteInspectResponse{}
		out.Body.Valid = true
		out.Body.Label = inv.Label
		out.Body.Role = inv.Role
		out.Body.AllLibraries = inv.Role == "admin" || inv.AllLibraries
		out.Body.RequireTOTP = inv.RequireTOTP
		out.Body.RemainingUses = rem
		out.Body.ExpiresAt = inv.ExpiresAt
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "invite-accept",
		Method:        http.MethodPost,
		Path:          "/api/v1/invites/{token}/accept",
		Summary:       "Accept an invitation token, create a user account, and open a session",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Auth"},
	}, func(ctx context.Context, in *AcceptInviteInput) (*AuthResponse, error) {
		kind := in.Body.ClientKind
		if kind == "" {
			kind = "web"
		}
		res, err := authSvc.AcceptInvite(
			ctx,
			in.Token,
			in.Body.Username,
			in.Body.DisplayName,
			in.Body.Password,
			kind,
			in.Body.ClientName,
			clientIPFromContext(ctx),
			userAgentFromContext(ctx),
		)
		if err != nil {
			return nil, mapAuthError(err)
		}
		return buildAuthResponse(res, kind == "web", useSecureCookie(ctx, runtimeNet)), nil
	})
}

func toInviteDTO(inv store.UserInvite, now time.Time) InviteDTO {
	libIDs := inv.LibraryIDs
	if libIDs == nil || inv.Role == "admin" || inv.AllLibraries {
		libIDs = []string{}
	}
	return InviteDTO{
		ID:           inv.ID,
		TokenPrefix:  inv.TokenPrefix,
		Label:        inv.Label,
		Role:         inv.Role,
		AllLibraries: inv.Role == "admin" || inv.AllLibraries,
		RequireTOTP:  inv.RequireTOTP,
		LibraryIDs:   libIDs,
		MaxUses:      inv.MaxUses,
		UseCount:     inv.UseCount,
		Status:       auth.InviteStatus(inv, now),
		CreatedBy:    inv.CreatedBy,
		ExpiresAt:    inv.ExpiresAt,
		RevokedAt:    inv.RevokedAt,
		CreatedAt:    inv.CreatedAt,
	}
}
