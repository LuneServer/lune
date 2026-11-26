package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5"

	"github.com/LuneServer/lune/internal/auth"
	"github.com/LuneServer/lune/internal/store"
)

type UserProfileDTO struct {
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
	LibraryIDs          []string              `json:"library_ids"`
	Preferences         store.UserPreferences `json:"preferences"`
	ActiveSessions      int                   `json:"active_sessions"`
	LastLoginAt         *time.Time            `json:"last_login_at,omitempty"`
	CreatedAt           time.Time             `json:"created_at"`
	UpdatedAt           time.Time             `json:"updated_at"`
}

type UserProfileResponse struct {
	Body UserProfileDTO
}

type UpdateSelfProfileInput struct {
	Body struct {
		DisplayName *string                `json:"display_name,omitempty" minLength:"1" maxLength:"64"`
		Preferences *store.UserPreferences `json:"preferences,omitempty"`
	}
}

type ChangeOwnPasswordInput struct {
	Body struct {
		CurrentPassword string `json:"current_password" minLength:"1" maxLength:"128"`
		NewPassword     string `json:"new_password" minLength:"8" maxLength:"128"`
	}
}

type ChangeOwnPasswordResponse struct {
	Body struct {
		Changed bool `json:"changed"`
	}
}

type TOTPSetupResponse struct {
	Body struct {
		Secret     string `json:"secret"`
		OTPAuthURL string `json:"otpauth_url"`
		Issuer     string `json:"issuer"`
		Account    string `json:"account"`
	}
}

type TOTPEnableInput struct {
	Body struct {
		Password string `json:"password" minLength:"1" maxLength:"128"`
		Code     string `json:"code" minLength:"6" maxLength:"16"`
	}
}

type TOTPEnableResponse struct {
	Body struct {
		Enabled       bool     `json:"enabled"`
		RecoveryCodes []string `json:"recovery_codes"`
	}
}

type TOTPDisableInput struct {
	Body struct {
		Password string `json:"password" minLength:"1" maxLength:"128"`
		Code     string `json:"code" minLength:"6" maxLength:"32"`
	}
}

type TOTPDisableResponse struct {
	Body struct {
		Enabled bool `json:"enabled"`
	}
}

type UserMarksListInput struct {
	Limit  int `query:"limit" default:"50" minimum:"1" maximum:"200"`
	Offset int `query:"offset" default:"0" minimum:"0"`
}

type UserMarkResponse struct {
	Body struct {
		ItemID      string `json:"item_id"`
		InWatchlist *bool  `json:"in_watchlist,omitempty"`
		IsFavorite  *bool  `json:"is_favorite,omitempty"`
	}
}

type AdminUserDTO struct {
	ID                  string     `json:"id"`
	Username            string     `json:"username"`
	DisplayName         string     `json:"display_name"`
	Role                string     `json:"role"`
	Disabled            bool       `json:"disabled"`
	AllLibraries        bool       `json:"all_libraries"`
	CanRequest          bool       `json:"can_request"`
	AutoApproveRequests bool       `json:"auto_approve_requests"`
	TOTPEnabled         bool       `json:"totp_enabled"`
	TOTPRequired        bool       `json:"totp_required"`
	LibraryIDs          []string   `json:"library_ids"`
	AvatarURL           string     `json:"avatar_url,omitempty"`
	ActiveSessions      int        `json:"active_sessions"`
	LastLoginAt         *time.Time `json:"last_login_at,omitempty"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

type AdminUsersListResponse struct {
	Body struct {
		Users []AdminUserDTO `json:"users"`
	}
}

type CreateUserInput struct {
	Body struct {
		Username     string   `json:"username" minLength:"2" maxLength:"64"`
		DisplayName  string   `json:"display_name,omitempty" maxLength:"64"`
		Password     string   `json:"password" minLength:"8" maxLength:"128"`
		Role         string   `json:"role,omitempty" enum:"admin,member" default:"member"`
		AllLibraries *bool    `json:"all_libraries,omitempty"`
		LibraryIDs   []string `json:"library_ids,omitempty"`
		TOTPRequired bool     `json:"totp_required,omitempty"`
	}
}

type AdminUserResponse struct {
	Body AdminUserDTO
}

type UpdateUserInput struct {
	ID   string `path:"id"`
	Body struct {
		DisplayName         *string   `json:"display_name,omitempty" minLength:"1" maxLength:"64"`
		Role                *string   `json:"role,omitempty" enum:"admin,member"`
		Disabled            *bool     `json:"disabled,omitempty"`
		Password            *string   `json:"password,omitempty" minLength:"8" maxLength:"128"`
		AllLibraries        *bool     `json:"all_libraries,omitempty"`
		LibraryIDs          *[]string `json:"library_ids,omitempty"`
		CanRequest          *bool     `json:"can_request,omitempty"`
		AutoApproveRequests *bool     `json:"auto_approve_requests,omitempty"`
		TOTPRequired        *bool     `json:"totp_required,omitempty"`
	}
}

type UserIDInput struct {
	ID string `path:"id"`
}

func registerUserRoutes(r chi.Router, api huma.API, authSvc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "user-get-profile",
		Method:      http.MethodGet,
		Path:        "/api/v1/users/me",
		Summary:     "Get full profile, preferences, and library permissions for the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, _ *struct{}) (*UserProfileResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		detail, err := authSvc.GetUserDetail(ctx, user.ID)
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &UserProfileResponse{Body: toUserProfileDTO(detail)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-update-profile",
		Method:      http.MethodPatch,
		Path:        "/api/v1/users/me",
		Summary:     "Update current user's display name and playback preferences",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *UpdateSelfProfileInput) (*UserProfileResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		detail, err := authSvc.UpdateSelfProfile(ctx, user, in.Body.DisplayName, in.Body.Preferences, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &UserProfileResponse{Body: toUserProfileDTO(detail)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-change-password",
		Method:      http.MethodPost,
		Path:        "/api/v1/users/me/password",
		Summary:     "Change current user's password and revoke other active sessions",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *ChangeOwnPasswordInput) (*ChangeOwnPasswordResponse, error) {
		user, sess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.ChangeOwnPassword(ctx, user, sess.ID, in.Body.CurrentPassword, in.Body.NewPassword, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		out := &ChangeOwnPasswordResponse{}
		out.Body.Changed = true
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-totp-setup",
		Method:      http.MethodPost,
		Path:        "/api/v1/users/me/totp/setup",
		Summary:     "Generate a new TOTP two-factor authentication secret for the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, _ *struct{}) (*TOTPSetupResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		res, err := authSvc.BeginTOTPSetup(ctx, user)
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &TOTPSetupResponse{}
		out.Body.Secret = res.Secret
		out.Body.OTPAuthURL = res.OTPAuthURL
		out.Body.Issuer = res.Issuer
		out.Body.Account = res.Account
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-totp-enable",
		Method:      http.MethodPost,
		Path:        "/api/v1/users/me/totp/enable",
		Summary:     "Verify and enable TOTP two-factor authentication for the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *TOTPEnableInput) (*TOTPEnableResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		codes, err := authSvc.EnableTOTP(ctx, user, in.Body.Password, in.Body.Code, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &TOTPEnableResponse{}
		out.Body.Enabled = true
		out.Body.RecoveryCodes = codes
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-totp-disable",
		Method:      http.MethodPost,
		Path:        "/api/v1/users/me/totp/disable",
		Summary:     "Disable TOTP two-factor authentication for the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *TOTPDisableInput) (*TOTPDisableResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.DisableTOTP(ctx, user, in.Body.Password, in.Body.Code, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		out := &TOTPDisableResponse{}
		out.Body.Enabled = false
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-delete-avatar",
		Method:      http.MethodDelete,
		Path:        "/api/v1/users/me/avatar",
		Summary:     "Remove current user's avatar image",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, _ *struct{}) (*UserProfileResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		detail, err := authSvc.DeleteAvatar(ctx, user, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &UserProfileResponse{Body: toUserProfileDTO(detail)}, nil
	})

	// Binary avatar upload (accepts either raw PNG/JPEG/WebP body or multipart/form-data field "avatar"/"file").
	r.Put("/api/v1/users/me/avatar", func(w http.ResponseWriter, req *http.Request) {
		user, _, ok := currentPrincipal(req.Context())
		if !ok {
			writeJSONError(w, http.StatusUnauthorized, "authentication required")
			return
		}

		var data []byte
		var readErr error
		ct := req.Header.Get("Content-Type")
		if strings.HasPrefix(strings.ToLower(ct), "multipart/form-data") {
			req.Body = http.MaxBytesReader(w, req.Body, auth.MaxAvatarBytes+(64<<10))
			if err := req.ParseMultipartForm(auth.MaxAvatarBytes); err != nil {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "avatar upload too large or malformed")
				return
			}
			f, _, err := req.FormFile("avatar")
			if err != nil {
				f, _, err = req.FormFile("file")
			}
			if err != nil {
				writeJSONError(w, http.StatusUnprocessableEntity, "multipart form must include an 'avatar' or 'file' field")
				return
			}
			defer f.Close()
			data, readErr = io.ReadAll(io.LimitReader(f, auth.MaxAvatarBytes+1))
		} else {
			req.Body = http.MaxBytesReader(w, req.Body, auth.MaxAvatarBytes+1024)
			data, readErr = io.ReadAll(io.LimitReader(req.Body, auth.MaxAvatarBytes+1))
		}
		if readErr != nil {
			var maxBytesErr *http.MaxBytesError
			if errors.As(readErr, &maxBytesErr) {
				writeJSONError(w, http.StatusRequestEntityTooLarge, "avatar upload exceeds maximum size")
				return
			}
			writeJSONError(w, http.StatusBadRequest, "failed to read avatar upload")
			return
		}

		detail, err := authSvc.UploadAvatar(req.Context(), user, data, clientIPFromContext(req.Context()))
		if err != nil {
			switch {
			case errors.Is(err, auth.ErrPayloadTooLarge):
				writeJSONError(w, http.StatusRequestEntityTooLarge, err.Error())
			case errors.Is(err, auth.ErrValidation):
				writeJSONError(w, http.StatusUnprocessableEntity, err.Error())
			default:
				writeJSONError(w, http.StatusInternalServerError, "failed to store avatar")
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(toUserProfileDTO(detail))
	})

	// Binary avatar download endpoint (authenticated).
	r.Get("/api/v1/users/{id}/avatar", func(w http.ResponseWriter, req *http.Request) {
		if _, _, ok := currentPrincipal(req.Context()); !ok {
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}
		avatarPath, err := authSvc.GetAvatarPath(req.Context(), chi.URLParam(req, "id"))
		if err != nil {
			http.NotFound(w, req)
			return
		}
		w.Header().Set("Cache-Control", "private, max-age=3600")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "sandbox")
		http.ServeFile(w, req, avatarPath)
	})

	// Per-user watchlist endpoints.
	huma.Register(api, huma.Operation{
		OperationID: "user-list-watchlist",
		Method:      http.MethodGet,
		Path:        "/api/v1/users/me/watchlist",
		Summary:     "List media items in the current user's watchlist",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *UserMarksListInput) (*ListItemsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		items, total, err := authSvc.ListUserMarks(ctx, user, store.MarkKindWatchlist, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list watchlist", err)
		}
		return buildItemsListResponse(items, total, in.Limit, in.Offset), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-add-watchlist",
		Method:      http.MethodPut,
		Path:        "/api/v1/users/me/watchlist/{id}",
		Summary:     "Add a media item to the current user's watchlist",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *ItemIDInput) (*UserMarkResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.AddUserMark(ctx, user, in.ID, store.MarkKindWatchlist); err != nil {
			return nil, mapAuthError(err)
		}
		t := true
		out := &UserMarkResponse{}
		out.Body.ItemID = in.ID
		out.Body.InWatchlist = &t
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "user-remove-watchlist",
		Method:        http.MethodDelete,
		Path:          "/api/v1/users/me/watchlist/{id}",
		Summary:       "Remove a media item from the current user's watchlist",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Users"},
	}, func(ctx context.Context, in *ItemIDInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.RemoveUserMark(ctx, user, in.ID, store.MarkKindWatchlist); err != nil {
			return nil, mapAuthError(err)
		}
		return nil, nil
	})

	// Per-user favorites endpoints.
	huma.Register(api, huma.Operation{
		OperationID: "user-list-favorites",
		Method:      http.MethodGet,
		Path:        "/api/v1/users/me/favorites",
		Summary:     "List media items marked as favorites by the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *UserMarksListInput) (*ListItemsResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		items, total, err := authSvc.ListUserMarks(ctx, user, store.MarkKindFavorite, in.Limit, in.Offset)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list favorites", err)
		}
		return buildItemsListResponse(items, total, in.Limit, in.Offset), nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "user-add-favorite",
		Method:      http.MethodPut,
		Path:        "/api/v1/users/me/favorites/{id}",
		Summary:     "Mark a media item as favorite for the current user",
		Tags:        []string{"Users"},
	}, func(ctx context.Context, in *ItemIDInput) (*UserMarkResponse, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.AddUserMark(ctx, user, in.ID, store.MarkKindFavorite); err != nil {
			return nil, mapAuthError(err)
		}
		t := true
		out := &UserMarkResponse{}
		out.Body.ItemID = in.ID
		out.Body.IsFavorite = &t
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "user-remove-favorite",
		Method:        http.MethodDelete,
		Path:          "/api/v1/users/me/favorites/{id}",
		Summary:       "Remove a media item from the current user's favorites",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Users"},
	}, func(ctx context.Context, in *ItemIDInput) (*struct{}, error) {
		user, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if err := authSvc.RemoveUserMark(ctx, user, in.ID, store.MarkKindFavorite); err != nil {
			return nil, mapAuthError(err)
		}
		return nil, nil
	})
}

func registerUserAdminRoutes(api huma.API, authSvc *auth.Service) {
	huma.Register(api, huma.Operation{
		OperationID: "admin-list-users",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/users",
		Summary:     "List all user accounts with library access and session counts (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, _ *struct{}) (*AdminUsersListResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		details, err := authSvc.ListUsersDetailed(ctx)
		if err != nil {
			return nil, huma.Error500InternalServerError("failed to list users", err)
		}
		out := &AdminUsersListResponse{}
		out.Body.Users = make([]AdminUserDTO, 0, len(details))
		for _, d := range details {
			out.Body.Users = append(out.Body.Users, toAdminUserDTO(d))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-get-user",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/users/{id}",
		Summary:     "Get detailed information for a single user account (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UserIDInput) (*AdminUserResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		detail, err := authSvc.GetUserDetail(ctx, in.ID)
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &AdminUserResponse{Body: toAdminUserDTO(detail)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "admin-create-user",
		Method:        http.MethodPost,
		Path:          "/api/v1/admin/users",
		Summary:       "Create a member or administrator account with library permissions (admin only)",
		DefaultStatus: http.StatusCreated,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, in *CreateUserInput) (*AdminUserResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		detail, err := authSvc.CreateUserWithAccess(ctx, actor, auth.CreateUserRequest{
			Username:     in.Body.Username,
			DisplayName:  in.Body.DisplayName,
			Password:     in.Body.Password,
			Role:         in.Body.Role,
			AllLibraries: in.Body.AllLibraries,
			LibraryIDs:   in.Body.LibraryIDs,
			TOTPRequired: in.Body.TOTPRequired,
		}, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &AdminUserResponse{Body: toAdminUserDTO(detail)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-update-user",
		Method:      http.MethodPatch,
		Path:        "/api/v1/admin/users/{id}",
		Summary:     "Update user display name, role, disabled state, password, or library permissions (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UpdateUserInput) (*AdminUserResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		detail, err := authSvc.UpdateUserDetailed(ctx, actor, in.ID, auth.UpdateUserRequest{
			DisplayName:         in.Body.DisplayName,
			Role:                in.Body.Role,
			Disabled:            in.Body.Disabled,
			Password:            in.Body.Password,
			AllLibraries:        in.Body.AllLibraries,
			LibraryIDs:          in.Body.LibraryIDs,
			CanRequest:          in.Body.CanRequest,
			AutoApproveRequests: in.Body.AutoApproveRequests,
			TOTPRequired:        in.Body.TOTPRequired,
		}, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		return &AdminUserResponse{Body: toAdminUserDTO(detail)}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "admin-delete-user",
		Method:        http.MethodDelete,
		Path:          "/api/v1/admin/users/{id}",
		Summary:       "Delete a user account (admin only)",
		DefaultStatus: http.StatusNoContent,
		Tags:          []string{"Admin"},
	}, func(ctx context.Context, in *UserIDInput) (*struct{}, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if err := authSvc.DeleteUser(ctx, actor, in.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-list-user-sessions",
		Method:      http.MethodGet,
		Path:        "/api/v1/admin/users/{id}/sessions",
		Summary:     "List active sessions for a specific user (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UserIDInput) (*SessionsResponse, error) {
		actor, curSess, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		sessions, err := authSvc.ListUserSessionsAdmin(ctx, in.ID)
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &SessionsResponse{}
		out.Body.Sessions = make([]SessionDTO, 0, len(sessions))
		for _, s := range sessions {
			out.Body.Sessions = append(out.Body.Sessions, toSessionDTO(s, curSess.ID))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-revoke-user-sessions",
		Method:      http.MethodDelete,
		Path:        "/api/v1/admin/users/{id}/sessions",
		Summary:     "Revoke all active sessions for a specific user (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UserIDInput) (*RevokeSessionsResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		n, err := authSvc.RevokeAllUserSessionsAdmin(ctx, actor, in.ID, clientIPFromContext(ctx))
		if err != nil {
			return nil, mapAuthError(err)
		}
		out := &RevokeSessionsResponse{}
		out.Body.RevokedCount = n
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "admin-reset-user-totp",
		Method:      http.MethodDelete,
		Path:        "/api/v1/admin/users/{id}/totp",
		Summary:     "Disable two-factor authentication for a locked-out user (admin only)",
		Tags:        []string{"Admin"},
	}, func(ctx context.Context, in *UserIDInput) (*TOTPDisableResponse, error) {
		actor, _, ok := currentPrincipal(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		if actor.Role != "admin" {
			return nil, huma.Error403Forbidden("administrator privileges required")
		}
		if err := authSvc.AdminResetTOTP(ctx, actor, in.ID, clientIPFromContext(ctx)); err != nil {
			return nil, mapAuthError(err)
		}
		out := &TOTPDisableResponse{}
		out.Body.Enabled = false
		return out, nil
	})
}

func toUserProfileDTO(d auth.UserDetail) UserProfileDTO {
	libIDs := d.LibraryIDs
	if libIDs == nil {
		libIDs = []string{}
	}
	return UserProfileDTO{
		ID:                  d.User.ID,
		Username:            d.User.Username,
		DisplayName:         d.User.DisplayName,
		Role:                d.User.Role,
		AvatarURL:           avatarURLFor(d.User),
		AllLibraries:        d.User.HasUnrestrictedLibraryAccess(),
		CanRequest:          d.User.Role == "admin" || d.User.CanRequest,
		AutoApproveRequests: d.User.Role == "admin" || d.User.AutoApproveRequests,
		TOTPEnabled:         d.User.TOTPEnabled,
		TOTPRequired:        d.User.TOTPRequired,
		LibraryIDs:          libIDs,
		Preferences:         d.User.Preferences(),
		ActiveSessions:      d.ActiveSessions,
		LastLoginAt:         d.User.LastLoginAt,
		CreatedAt:           d.User.CreatedAt,
		UpdatedAt:           d.User.UpdatedAt,
	}
}

func toAdminUserDTO(d auth.UserDetail) AdminUserDTO {
	libIDs := d.LibraryIDs
	if libIDs == nil {
		libIDs = []string{}
	}
	return AdminUserDTO{
		ID:                  d.User.ID,
		Username:            d.User.Username,
		DisplayName:         d.User.DisplayName,
		Role:                d.User.Role,
		Disabled:            d.User.Disabled,
		AllLibraries:        d.User.HasUnrestrictedLibraryAccess(),
		CanRequest:          d.User.Role == "admin" || d.User.CanRequest,
		AutoApproveRequests: d.User.Role == "admin" || d.User.AutoApproveRequests,
		TOTPEnabled:         d.User.TOTPEnabled,
		TOTPRequired:        d.User.TOTPRequired,
		LibraryIDs:          libIDs,
		AvatarURL:           avatarURLFor(d.User),
		ActiveSessions:      d.ActiveSessions,
		LastLoginAt:         d.User.LastLoginAt,
		CreatedAt:           d.User.CreatedAt,
		UpdatedAt:           d.User.UpdatedAt,
	}
}

func buildItemsListResponse(items []store.MediaItem, total, limit, offset int) *ListItemsResponse {
	out := &ListItemsResponse{}
	out.Body.Total = total
	out.Body.Limit = limit
	out.Body.Offset = offset
	out.Body.Items = make([]MediaItemSummaryDTO, 0, len(items))
	for _, m := range items {
		out.Body.Items = append(out.Body.Items, toMediaSummaryDTO(m))
	}
	return out
}

func writeJSONError(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status": status,
		"detail": detail,
	})
}
