package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/store"
)

type UserDTO struct {
	ID                  string                `json:"id"`
	Username            string                `json:"username"`
	DisplayName         string                `json:"display_name"`
	Role                string                `json:"role"`
	AvatarURL           string                `json:"avatar_url,omitempty"`
	AllLibraries        bool                  `json:"all_libraries"`
	CanRequest          bool                  `json:"can_request"`
	AutoApproveRequests bool                  `json:"auto_approve_requests"`
	TOTPEnabled         bool                  `json:"totp_enabled"`
	TOTPRequired        bool                  `json:"totp_required"`
	Preferences         store.UserPreferences `json:"preferences"`
}

type SessionDTO struct {
	ID         string    `json:"id"`
	ClientName string    `json:"client_name"`
	ClientKind string    `json:"client_kind"`
	IPAddress  string    `json:"ip_address"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastSeenAt time.Time `json:"last_seen_at"`
	Current    bool      `json:"current"`
}

type AuditEventDTO struct {
	ID        int64     `json:"id"`
	EventType string    `json:"event_type"`
	ActorID   string    `json:"actor_id,omitempty"`
	ClientIP  string    `json:"client_ip"`
	Detail    string    `json:"detail"`
	CreatedAt time.Time `json:"created_at"`
}

type SetupInput struct {
	Body struct {
		Username    string `json:"username" minLength:"2" maxLength:"64"`
		DisplayName string `json:"display_name,omitempty" maxLength:"64"`
		Password    string `json:"password" minLength:"8" maxLength:"128"`
	}
}

type LoginInput struct {
	Body struct {
		Username   string `json:"username" minLength:"1"`
		Password   string `json:"password" minLength:"1" maxLength:"256"`
		TOTPCode   string `json:"totp_code,omitempty" maxLength:"32"`
		ClientKind string `json:"client_kind,omitempty" enum:"web,app" default:"web"`
		ClientName string `json:"client_name,omitempty" maxLength:"64"`
	}
}

type AuthResponse struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      struct {
		User         UserDTO   `json:"user"`
		SessionID    string    `json:"session_id"`
		AccessToken  string    `json:"access_token,omitempty"`
		RefreshToken string    `json:"refresh_token,omitempty"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
}

type RefreshInput struct {
	Body struct {
		RefreshToken string `json:"refresh_token" minLength:"1"`
	}
}

type TokenResponse struct {
	Body struct {
		User         UserDTO   `json:"user"`
		SessionID    string    `json:"session_id"`
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token"`
		ExpiresAt    time.Time `json:"expires_at"`
	}
}

type LogoutResponse struct {
	SetCookie http.Cookie `header:"Set-Cookie"`
	Body      struct {
		Revoked bool `json:"revoked"`
	}
}

type MeResponse struct {
	Body struct {
		User    UserDTO    `json:"user"`
		Session SessionDTO `json:"session"`
	}
}

type SessionsResponse struct {
	Body struct {
		Sessions []SessionDTO `json:"sessions"`
	}
}

type RevokeSessionInput struct {
	ID string `path:"id"`
}

type RevokeSessionsResponse struct {
	Body struct {
		RevokedCount int64 `json:"revoked_count"`
	}
}

type DeviceStartInput struct {
	Body struct {
		ClientName string `json:"client_name,omitempty" maxLength:"64"`
	}
}

type DeviceStartResponse struct {
	Body struct {
		DeviceCode string `json:"device_code"`
		UserCode   string `json:"user_code" example:"K7MP-4WQX"`
		ExpiresIn  int    `json:"expires_in"`
		Interval   int    `json:"interval"`
	}
}

type DeviceApproveInput struct {
	Body struct {
		UserCode string `json:"user_code" minLength:"8" maxLength:"12"`
	}
}

type DeviceApproveResponse struct {
	Body struct {
		Approved bool `json:"approved"`
	}
}

type DeviceTokenInput struct {
	Body struct {
		DeviceCode string `json:"device_code" minLength:"1"`
	}
}

type AuditListInput struct {
	Limit int `query:"limit" default:"50" minimum:"1" maximum:"200"`
}

type AuditListResponse struct {
	Body struct {
		Events []AuditEventDTO `json:"events"`
	}
}

type DismissedAlertsResponse struct {
	Body struct {
		DismissedIDs []string `json:"dismissed_ids"`
	}
}

type DismissAlertsInput struct {
	Body struct {
		IDs []string `json:"ids" minItems:"1" maxItems:"200"`
	}
}

func registerAuthRoutes(api huma.API, runtimeNet *runtimeNetwork, authSvc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID:   "setup-admin",
		Method:        http.MethodPost,
		Path:          "/api/v1/setup",
		Summary:       "Create initial administrator account",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Auth"},
	}, func(ctx context.Context, in *SetupInput) (*AuthResponse, error) {
		res, err := authSvc.SetupAdmin(ctx, in.Body.Username, in.Body.DisplayName, in.Body.Password, clientIPFromContext(ctx), userAgentFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return buildAuthResponse(res, true, useSecureCookie(ctx, runtimeNet)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-login",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/login",
		Summary:     "Authenticate with username and password",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *LoginInput) (*AuthResponse, error) {
		kind := in.Body.ClientKind
		if kind == "" {
			kind = "web"
		}
		res, err := authSvc.LoginWithTOTP(ctx, in.Body.Username, in.Body.Password, in.Body.TOTPCode, kind, in.Body.ClientName, clientIPFromContext(ctx), userAgentFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return buildAuthResponse(res, kind == "web", useSecureCookie(ctx, runtimeNet)), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-refresh",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/refresh",
		Summary:     "Rotate native app access and refresh tokens",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *RefreshInput) (*TokenResponse, error) {
		res, err := authSvc.Refresh(ctx, in.Body.RefreshToken, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return buildTokenResponse(res), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-logout",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/logout",
		Summary:     "Revoke current session and clear cookie",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, _ *struct{}) (*LogoutResponse, error) {
		user, sess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.RevokeSession(ctx, user, sess.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		out := &LogoutResponse{
			SetCookie: http.Cookie{
				Name:     sessionCookieName,
				Value:    "",
				Path:     "/",
				HttpOnly: true,
				Secure:   useSecureCookie(ctx, runtimeNet),
				SameSite: http.SameSiteLaxMode,
				MaxAge:   -1,
			},
		}
		out.Body.Revoked = true
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-me",
		Method:      http.MethodGet,
		Path:        "/api/v1/auth/me",
		Summary:     "Get current authenticated user and session",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, _ *struct{}) (*MeResponse, error) {
		user, sess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		out := &MeResponse{}
		out.Body.User = toUserDTO(user)
		out.Body.Session = toSessionDTO(sess, sess.ID)
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-list-sessions",
		Method:      http.MethodGet,
		Path:        "/api/v1/auth/sessions",
		Summary:     "List active sessions for the current user",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, _ *struct{}) (*SessionsResponse, error) {
		user, curSess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		sessions, err := authSvc.ListSessions(ctx, user.ID)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list sessions", err)
		}
		out := &SessionsResponse{}
		out.Body.Sessions = make([]SessionDTO, 0, len(sessions))
		for _, s := range sessions {
			out.Body.Sessions = append(out.Body.Sessions, toSessionDTO(s, curSess.ID))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "auth-revoke-session",
		Method:        http.MethodDelete,
		Path:          "/api/v1/auth/sessions/{id}",
		Summary:       "Revoke a specific session",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Auth"},
	}, func(ctx context.Context, in *RevokeSessionInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.RevokeSession(ctx, user, in.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-revoke-other-sessions",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/sessions/revoke-others",
		Summary:     "Revoke all other active sessions for the current user",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, _ *struct{}) (*RevokeSessionsResponse, error) {
		user, sess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		n, err := authSvc.RevokeOtherSessions(ctx, user, sess.ID, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &RevokeSessionsResponse{}
		out.Body.RevokedCount = n
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-device-start",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/device",
		Summary:     "Start RFC 8628 device login for TV/mobile apps",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *DeviceStartInput) (*DeviceStartResponse, error) {
		res, err := authSvc.StartDeviceFlow(ctx, in.Body.ClientName, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &DeviceStartResponse{}
		out.Body.DeviceCode = res.DeviceCode
		out.Body.UserCode = res.UserCode
		out.Body.ExpiresIn = res.ExpiresIn
		out.Body.Interval = res.Interval
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-device-approve",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/device/approve",
		Summary:     "Approve a pending TV/mobile device login code",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *DeviceApproveInput) (*DeviceApproveResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.ApproveDeviceFlow(ctx, user, in.Body.UserCode, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		out := &DeviceApproveResponse{}
		out.Body.Approved = true
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "auth-device-token",
		Method:      http.MethodPost,
		Path:        "/api/v1/auth/device/token",
		Summary:     "Poll for completion of an RFC 8628 device login",
		Tags:        []string{"Auth"},
	}, func(ctx context.Context, in *DeviceTokenInput) (*TokenResponse, error) {
		res, err := authSvc.PollDeviceFlow(ctx, in.Body.DeviceCode, clientIPFromContext(ctx), userAgentFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return buildTokenResponse(res), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-audit",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/audit",
		Summary:     "List recent security and authentication audit events (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *AuditListInput) (*AuditListResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		events, err := authSvc.ListAuditEvents(ctx, in.Limit)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list audit events", err)
		}
		out := &AuditListResponse{}
		out.Body.Events = make([]AuditEventDTO, 0, len(events))
		for _, ev := range events {
			out.Body.Events = append(out.Body.Events, AuditEventDTO{
				ID:        ev.ID,
				EventType: ev.EventType,
				ActorID:   ev.ActorID,
				ClientIP:  ev.ClientIP,
				Detail:    ev.Detail,
				CreatedAt: ev.CreatedAt,
			})
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-dismissed-alerts",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/alerts/dismissed",
		Summary:     "List dismissed console alert IDs (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*DismissedAlertsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		ids, err := authSvc.GetDismissedAlerts(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to load dismissed alerts", err)
		}
		out := &DismissedAlertsResponse{}
		out.Body.DismissedIDs = ids
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-dismiss-alerts",
		Method:      http.MethodPost,
		Path:        "/api/v1/admin/alerts/dismiss",
		Summary:     "Dismiss one or more console system alerts (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *DismissAlertsInput) (*DismissedAlertsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if user.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		ids, err := authSvc.DismissAlerts(ctx, in.Body.IDs)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to dismiss alerts", err)
		}
		out := &DismissedAlertsResponse{}
		out.Body.DismissedIDs = ids
		return out, nil
	})
}

func buildAuthResponse(res auth.AuthResult, setCookie, secure bool) *AuthResponse {
	out := &AuthResponse{}
	out.Body.User = toUserDTO(res.User)
	out.Body.SessionID = res.Session.ID
	out.Body.ExpiresAt = res.Session.ExpiresAt

	if setCookie {
		out.SetCookie = http.Cookie{
			Name:     sessionCookieName,
			Value:    res.AccessToken,
			Path:     "/",
			HttpOnly: true,
			Secure:   secure,
			SameSite: http.SameSiteLaxMode,
			Expires:  res.Session.ExpiresAt,
		}
	} else {
		out.Body.AccessToken = res.AccessToken
		out.Body.RefreshToken = res.RefreshToken
	}
	return out
}

func buildTokenResponse(res auth.AuthResult) *TokenResponse {
	out := &TokenResponse{}
	out.Body.User = toUserDTO(res.User)
	out.Body.SessionID = res.Session.ID
	out.Body.AccessToken = res.AccessToken
	out.Body.RefreshToken = res.RefreshToken
	out.Body.ExpiresAt = res.Session.ExpiresAt
	return out
}

func avatarURLFor(u store.User) string {
	if u.AvatarPath == "" {
		return ""
	}
	return "/api/v1/users/" + u.ID + "/avatar"
}

func toUserDTO(u store.User) UserDTO {
	return UserDTO{
		ID:                  u.ID,
		Username:            u.Username,
		DisplayName:         u.DisplayName,
		Role:                u.Role,
		AvatarURL:           avatarURLFor(u),
		AllLibraries:        u.HasUnrestrictedLibraryAccess(),
		CanRequest:          u.Role == "admin" || u.CanRequest,
		AutoApproveRequests: u.Role == "admin" || u.AutoApproveRequests,
		TOTPEnabled:         u.TOTPEnabled,
		TOTPRequired:        u.TOTPRequired,
		Preferences:         u.Preferences(),
	}
}

func toSessionDTO(s store.Session, currentID string) SessionDTO {
	return SessionDTO{
		ID:         s.ID,
		ClientName: s.ClientName,
		ClientKind: s.ClientKind,
		IPAddress:  s.IPAddress,
		ExpiresAt:  s.ExpiresAt,
		LastSeenAt: s.LastSeenAt,
		Current:    s.ID == currentID,
	}
}

func mapAuthError(err error) error {
	var rlErr *auth.RateLimitError
	if errors.As(err, &rlErr) {
		secs := int(rlErr.RetryAfter.Seconds())
		if secs < 1 {
			secs = 1
		}
		return huma.ErrorWithHeaders(
			huma.Error429TooManyRequests(rlErr.Error()),
			http.Header{"Retry-After": []string{strconv.Itoa(secs)}},
		)
	}

	switch {
	case errors.Is(err, auth.ErrSetupCompleted):
		return huma.Error409Conflict("initial setup has already been completed")
	case errors.Is(err, auth.ErrInvalidCredentials):
		return huma.Error401Unauthorized("invalid username or password")
	case errors.Is(err, auth.ErrTOTPRequired):
		return huma.ErrorWithHeaders(
			huma.Error401Unauthorized("two-factor authentication code required"),
			http.Header{"X-Lune-TOTP-Required": []string{"true"}},
		)
	case errors.Is(err, auth.ErrInvalidTOTPCode):
		return huma.Error401Unauthorized("invalid two-factor authentication code")
	case errors.Is(err, auth.ErrAccountDisabled):
		return huma.Error403Forbidden("account is disabled")
	case errors.Is(err, auth.ErrUnauthorized):
		return huma.Error401Unauthorized("authentication required")
	case errors.Is(err, auth.ErrForbidden):
		return huma.Error403Forbidden("insufficient permissions")
	case errors.Is(err, auth.ErrNotFound):
		return huma.Error404NotFound("resource not found")
	case errors.Is(err, auth.ErrConflict):
		return huma.Error409Conflict(err.Error())
	case errors.Is(err, auth.ErrDevicePending):
		return huma.Error428PreconditionRequired("authorization_pending")
	case errors.Is(err, auth.ErrDeviceExpired):
		return huma.Error410Gone("expired_token")
	case errors.Is(err, auth.ErrDeviceDenied):
		return huma.Error403Forbidden("access_denied")
	case errors.Is(err, auth.ErrInviteExpired):
		return huma.Error410Gone("invite has expired")
	case errors.Is(err, auth.ErrInviteRevoked):
		return huma.Error410Gone("invite has been revoked")
	case errors.Is(err, auth.ErrInviteExhausted):
		return huma.Error410Gone("invite has already been used")
	case errors.Is(err, auth.ErrPayloadTooLarge):
		return huma.Error413RequestEntityTooLarge(err.Error())
	case errors.Is(err, auth.ErrValidation):
		return huma.Error422UnprocessableEntity(err.Error())
	default:
		return huma.Error500InternalServerError("internal authentication error", err)
	}
}
