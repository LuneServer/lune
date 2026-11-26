package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/LuneServer/lune/internal/store"
)

const (
	MaxAvatarBytes           = 2 << 20 // 2 MiB
	dismissedAlertsKey       = "dismissed_console_alerts"
	maxStoredDismissedAlerts = 500
)

// UserDetail combines a user record with its explicit library grants and live session count.
type UserDetail struct {
	User           store.User
	LibraryIDs     []string
	ActiveSessions int
}

// CreateUserRequest defines inputs for admin user creation with library permissions.
type CreateUserRequest struct {
	Username     string
	DisplayName  string
	Password     string
	Role         string
	AllLibraries *bool
	LibraryIDs   []string
	TOTPRequired bool
}

// UpdateUserRequest defines inputs for admin user modification.
type UpdateUserRequest struct {
	DisplayName         *string
	Role                *string
	Disabled            *bool
	Password            *string
	AllLibraries        *bool
	LibraryIDs          *[]string
	CanRequest          *bool
	AutoApproveRequests *bool
	TOTPRequired        *bool
}

// CreateInviteRequest defines inputs for creating a user invitation link.
type CreateInviteRequest struct {
	Label          string
	Role           string
	AllLibraries   *bool
	LibraryIDs     []string
	RequireTOTP    bool
	MaxUses        int
	ExpiresInHours int
}

// CreatedInvite pairs the stored invite record with the raw one-time token.
type CreatedInvite struct {
	Invite store.UserInvite
	Token  string
}

// InviteStatus computes the human-readable lifecycle state of an invite.
func InviteStatus(inv store.UserInvite, now time.Time) string {
	if inv.RevokedAt != nil {
		return "revoked"
	}
	if !inv.ExpiresAt.After(now) {
		return "expired"
	}
	if inv.UseCount >= inv.MaxUses {
		return "exhausted"
	}
	return "active"
}

// Now returns the service clock in UTC.
func (s *Service) Now() time.Time {
	return s.now().UTC()
}

// GetUserDetail fetches a user together with its library grants and active session count.
func (s *Service) GetUserDetail(ctx context.Context, userID string) (UserDetail, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UserDetail{}, ErrNotFound
		}
		return UserDetail{}, err
	}
	libIDs, err := s.store.GetUserLibraryIDs(ctx, userID)
	if err != nil {
		return UserDetail{}, err
	}
	if u.HasUnrestrictedLibraryAccess() {
		libIDs = []string{}
	}
	sessions, err := s.store.CountActiveSessionsForUser(ctx, userID, s.now().UTC())
	if err != nil {
		return UserDetail{}, err
	}
	return UserDetail{
		User:           u,
		LibraryIDs:     libIDs,
		ActiveSessions: sessions,
	}, nil
}

// ListUsersDetailed returns all user accounts enriched with library grants and active session counts.
func (s *Service) ListUsersDetailed(ctx context.Context) ([]UserDetail, error) {
	users, err := s.store.ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	allLibs, err := s.store.GetAllUserLibraryIDs(ctx)
	if err != nil {
		return nil, err
	}
	sessCounts, err := s.store.CountActiveSessionsByUser(ctx, s.now().UTC())
	if err != nil {
		return nil, err
	}

	out := make([]UserDetail, 0, len(users))
	for _, u := range users {
		libIDs := allLibs[u.ID]
		if libIDs == nil || u.HasUnrestrictedLibraryAccess() {
			libIDs = []string{}
		}
		out = append(out, UserDetail{
			User:           u,
			LibraryIDs:     libIDs,
			ActiveSessions: sessCounts[u.ID],
		})
	}
	return out, nil
}

// CreateUserWithAccess creates a new user account with role and library access policy.
func (s *Service) CreateUserWithAccess(ctx context.Context, actor store.User, req CreateUserRequest, clientIP string) (UserDetail, error) {
	username, err := validateUsername(req.Username)
	if err != nil {
		return UserDetail{}, err
	}
	displayName := strings.TrimSpace(req.DisplayName)
	if displayName == "" {
		displayName = username
	}
	if len(displayName) > 64 {
		return UserDetail{}, fmt.Errorf("%w: display_name must be at most 64 characters", ErrValidation)
	}

	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" {
		return UserDetail{}, fmt.Errorf("%w: role must be 'admin' or 'member'", ErrValidation)
	}

	allLibraries := true
	if req.AllLibraries != nil {
		allLibraries = *req.AllLibraries
	}
	if role == "admin" {
		allLibraries = true
	}

	var libIDs []string
	if !allLibraries {
		libIDs, err = s.validateLibraryIDs(ctx, req.LibraryIDs)
		if err != nil {
			return UserDetail{}, err
		}
	}

	hash, err := HashPassword(req.Password)
	if err != nil {
		return UserDetail{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	u := store.User{
		ID:           uuid.NewString(),
		Username:     username,
		DisplayName:  displayName,
		PasswordHash: hash,
		Role:         role,
		AllLibraries: allLibraries,
		TOTPRequired: req.TOTPRequired,
	}
	if err := s.store.CreateUserWithLibraries(ctx, u, libIDs); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return UserDetail{}, fmt.Errorf("%w: username already exists", ErrConflict)
		}
		return UserDetail{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.user_created",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("created %s user %q", role, username),
	})
	return s.GetUserDetail(ctx, u.ID)
}

// UpdateUserDetailed updates display_name, role, disabled state, password, and library permissions.
func (s *Service) UpdateUserDetailed(ctx context.Context, actor store.User, targetID string, req UpdateUserRequest, clientIP string) (UserDetail, error) {
	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UserDetail{}, ErrNotFound
		}
		return UserDetail{}, err
	}

	var upd store.AdminUserUpdate
	var auditEvents []store.AuditEvent

	if req.DisplayName != nil {
		dn := strings.TrimSpace(*req.DisplayName)
		if len(dn) < 1 || len(dn) > 64 {
			return UserDetail{}, fmt.Errorf("%w: display_name must be between 1 and 64 characters", ErrValidation)
		}
		upd.DisplayName = &dn
	}

	effectiveRole := target.Role
	if req.Role != nil {
		role := strings.TrimSpace(*req.Role)
		if role != "admin" && role != "member" {
			return UserDetail{}, fmt.Errorf("%w: role must be 'admin' or 'member'", ErrValidation)
		}
		if role != target.Role {
			if target.ID == actor.ID && role != "admin" {
				return UserDetail{}, fmt.Errorf("%w: cannot demote your own administrator account", ErrValidation)
			}
			if target.Role == "admin" && role != "admin" && !target.Disabled {
				if admins, err := s.store.CountActiveAdmins(ctx); err == nil && admins <= 1 {
					return UserDetail{}, fmt.Errorf("%w: cannot demote the last active administrator", ErrValidation)
				}
			}
			upd.Role = &role
			effectiveRole = role
			auditEvents = append(auditEvents, store.AuditEvent{
				EventType: "admin.user_role_changed",
				ActorID:   actor.ID,
				ClientIP:  clientIP,
				Detail:    fmt.Sprintf("changed role for %q from %s to %s", target.Username, target.Role, role),
			})
		}
	}

	if req.Disabled != nil {
		if *req.Disabled && target.ID == actor.ID {
			return UserDetail{}, fmt.Errorf("%w: cannot disable your own account", ErrValidation)
		}
		if *req.Disabled && target.Role == "admin" && !target.Disabled {
			if admins, err := s.store.CountActiveAdmins(ctx); err == nil && admins <= 1 {
				return UserDetail{}, fmt.Errorf("%w: cannot disable the last active administrator", ErrValidation)
			}
		}
		upd.Disabled = req.Disabled
		evType := "admin.user_enabled"
		if *req.Disabled {
			evType = "admin.user_disabled"
		}
		auditEvents = append(auditEvents, store.AuditEvent{
			EventType: evType,
			ActorID:   actor.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("updated user %q", target.Username),
		})
	}

	if req.Password != nil && *req.Password != "" {
		hash, err := HashPassword(*req.Password)
		if err != nil {
			return UserDetail{}, fmt.Errorf("%w: %v", ErrValidation, err)
		}
		upd.PasswordHash = &hash
		auditEvents = append(auditEvents, store.AuditEvent{
			EventType: "admin.password_reset",
			ActorID:   actor.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("reset password for %q", target.Username),
		})
	}

	if effectiveRole == "admin" {
		if !target.AllLibraries || req.AllLibraries != nil || req.LibraryIDs != nil {
			allTrue := true
			upd.AllLibraries = &allTrue
		}
	} else {
		effectiveAll := target.AllLibraries
		if req.AllLibraries != nil {
			effectiveAll = *req.AllLibraries
			upd.AllLibraries = req.AllLibraries
		}
		if req.LibraryIDs != nil {
			if effectiveAll {
				// If caller explicitly passed library_ids without all_libraries=true,
				// only keep them when restricted.
				if req.AllLibraries == nil {
					allFalse := false
					upd.AllLibraries = &allFalse
					effectiveAll = false
				}
			}
			if !effectiveAll {
				validated, err := s.validateLibraryIDs(ctx, *req.LibraryIDs)
				if err != nil {
					return UserDetail{}, err
				}
				upd.LibraryIDs = &validated
			}
		}
		if upd.AllLibraries != nil || upd.LibraryIDs != nil {
			auditEvents = append(auditEvents, store.AuditEvent{
				EventType: "admin.user_library_access_updated",
				ActorID:   actor.ID,
				ClientIP:  clientIP,
				Detail:    fmt.Sprintf("updated library permissions for %q", target.Username),
			})
		}
	}

	if req.TOTPRequired != nil && *req.TOTPRequired != target.TOTPRequired {
		upd.TOTPRequired = req.TOTPRequired
		auditEvents = append(auditEvents, store.AuditEvent{
			EventType: "admin.totp_policy_updated",
			ActorID:   actor.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("set totp_required=%t for %q", *req.TOTPRequired, target.Username),
		})
	}

	if req.CanRequest != nil {
		upd.CanRequest = req.CanRequest
	}
	if req.AutoApproveRequests != nil {
		upd.AutoApproveRequests = req.AutoApproveRequests
	}

	if err := s.store.AdminUpdateUser(ctx, targetID, upd); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UserDetail{}, ErrNotFound
		}
		return UserDetail{}, err
	}

	if len(auditEvents) == 0 && (upd.DisplayName != nil || upd.CanRequest != nil || upd.AutoApproveRequests != nil) {
		auditEvents = append(auditEvents, store.AuditEvent{
			EventType: "admin.user_updated",
			ActorID:   actor.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("updated profile for %q", target.Username),
		})
	}
	for _, ev := range auditEvents {
		_ = s.store.InsertAuditEvent(ctx, ev)
	}

	return s.GetUserDetail(ctx, targetID)
}

// UpdateSelfProfile updates the authenticated user's own display name and preferences.
func (s *Service) UpdateSelfProfile(ctx context.Context, actor store.User, displayName *string, prefs *store.UserPreferences, clientIP string) (UserDetail, error) {
	dn := actor.DisplayName
	if displayName != nil {
		trimmed := strings.TrimSpace(*displayName)
		if len(trimmed) < 1 || len(trimmed) > 64 {
			return UserDetail{}, fmt.Errorf("%w: display_name must be between 1 and 64 characters", ErrValidation)
		}
		dn = trimmed
	}

	curPrefs := actor.Preferences()
	if prefs != nil {
		p := *prefs
		p.AudioLanguage = strings.TrimSpace(p.AudioLanguage)
		p.SubtitleLanguage = strings.TrimSpace(p.SubtitleLanguage)
		if len(p.AudioLanguage) > 16 {
			return UserDetail{}, fmt.Errorf("%w: audio_language must be at most 16 characters", ErrValidation)
		}
		if len(p.SubtitleLanguage) > 16 {
			return UserDetail{}, fmt.Errorf("%w: subtitle_language must be at most 16 characters", ErrValidation)
		}
		if p.MaxBitrate < 0 {
			return UserDetail{}, fmt.Errorf("%w: max_bitrate must be >= 0", ErrValidation)
		}
		if len(p.PosterBadges) > 0 {
			seen := make(map[string]struct{}, len(p.PosterBadges))
			cleaned := make([]string, 0, len(p.PosterBadges))
			for _, raw := range p.PosterBadges {
				badge := strings.ToLower(strings.TrimSpace(raw))
				switch badge {
				case "resolution", "hdr", "dual_audio", "seasons", "rating":
					if _, dup := seen[badge]; !dup {
						seen[badge] = struct{}{}
						cleaned = append(cleaned, badge)
					}
				default:
					return UserDetail{}, fmt.Errorf("%w: unknown poster badge %q", ErrValidation, raw)
				}
			}
			p.PosterBadges = cleaned
		} else {
			p.PosterBadges = nil
		}
		curPrefs = p
	}

	prefsBytes, err := json.Marshal(curPrefs)
	if err != nil {
		return UserDetail{}, err
	}

	if err := s.store.UpdateUserProfile(ctx, actor.ID, dn, string(prefsBytes)); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UserDetail{}, ErrNotFound
		}
		return UserDetail{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "user.profile_updated",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("updated profile for %q", actor.Username),
	})
	return s.GetUserDetail(ctx, actor.ID)
}

// ChangeOwnPassword verifies the user's current password, sets a new Argon2id hash,
// and revokes all other active sessions while keeping currentSessionID valid.
func (s *Service) ChangeOwnPassword(ctx context.Context, actor store.User, currentSessionID, currentPassword, newPassword, clientIP string) error {
	if allowed, retryAfter := s.limiter.Check("chpw:"+clientIP, "chpw:"+actor.ID); !allowed {
		return &RateLimitError{RetryAfter: retryAfter}
	}
	if !VerifyPassword(currentPassword, actor.PasswordHash) {
		s.limiter.RecordFailure("chpw:"+clientIP, "chpw:"+actor.ID)
		return ErrInvalidCredentials
	}
	s.limiter.RecordSuccess("chpw:"+clientIP, "chpw:"+actor.ID)
	hash, err := HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrValidation, err)
	}
	if err := s.store.UpdateUserPasswordKeepSession(ctx, actor.ID, hash, currentSessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "user.password_changed",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    "changed own password and revoked other sessions",
	})
	return nil
}

// RevokeOtherSessions revokes every active session belonging to actor except currentSessionID.
func (s *Service) RevokeOtherSessions(ctx context.Context, actor store.User, currentSessionID, clientIP string) (int64, error) {
	n, err := s.store.RevokeUserSessions(ctx, actor.ID, currentSessionID)
	if err != nil {
		return 0, err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.other_sessions_revoked",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("revoked %d other session(s)", n),
	})
	return n, nil
}

// ListUserSessionsAdmin returns active sessions for targetID (admin-only at API layer).
func (s *Service) ListUserSessionsAdmin(ctx context.Context, targetID string) ([]store.Session, error) {
	if _, err := s.store.GetUserByID(ctx, targetID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return s.store.ListUserSessions(ctx, targetID)
}

// RevokeAllUserSessionsAdmin revokes all active sessions for targetID (admin-only at API layer).
func (s *Service) RevokeAllUserSessionsAdmin(ctx context.Context, actor store.User, targetID, clientIP string) (int64, error) {
	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, ErrNotFound
		}
		return 0, err
	}
	n, err := s.store.RevokeUserSessions(ctx, targetID, "")
	if err != nil {
		return 0, err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.sessions_revoked",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("revoked %d session(s) for %q", n, target.Username),
	})
	return n, nil
}

// UploadAvatar validates and saves an avatar image (PNG, JPEG, or WebP, max 2 MiB) for actor.
func (s *Service) UploadAvatar(ctx context.Context, actor store.User, data []byte, clientIP string) (UserDetail, error) {
	if s.dataDir == "" {
		return UserDetail{}, fmt.Errorf("%w: avatar storage is not configured", ErrValidation)
	}
	if len(data) == 0 {
		return UserDetail{}, fmt.Errorf("%w: avatar file is empty", ErrValidation)
	}
	if len(data) > MaxAvatarBytes {
		return UserDetail{}, fmt.Errorf("%w: avatar exceeds maximum size of 2 MiB", ErrPayloadTooLarge)
	}

	mimeType := http.DetectContentType(data)
	var ext string
	switch mimeType {
	case "image/png":
		ext = ".png"
	case "image/jpeg":
		ext = ".jpg"
	case "image/webp":
		ext = ".webp"
	default:
		return UserDetail{}, fmt.Errorf("%w: avatar must be a PNG, JPEG, or WebP image (detected %s)", ErrValidation, mimeType)
	}

	avatarDir := filepath.Join(s.dataDir, "avatars")
	if err := os.MkdirAll(avatarDir, 0o750); err != nil {
		return UserDetail{}, fmt.Errorf("create avatar directory: %w", err)
	}

	destPath := filepath.Join(avatarDir, actor.ID+ext)
	tmpFile, err := os.CreateTemp(avatarDir, actor.ID+".*.tmp")
	if err != nil {
		return UserDetail{}, fmt.Errorf("create temp avatar: %w", err)
	}
	tmpPath := tmpFile.Name()
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		_ = os.Remove(tmpPath)
		return UserDetail{}, err
	}
	if err := tmpFile.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return UserDetail{}, err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return UserDetail{}, err
	}

	// Remove previous avatar file if extension changed.
	if actor.AvatarPath != "" && actor.AvatarPath != destPath {
		_ = os.Remove(actor.AvatarPath)
	}

	if err := s.store.UpdateUserAvatar(ctx, actor.ID, destPath); err != nil {
		_ = os.Remove(destPath)
		return UserDetail{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "user.avatar_updated",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("updated avatar (%s)", mimeType),
	})
	return s.GetUserDetail(ctx, actor.ID)
}

// DeleteAvatar removes actor's avatar file and clears avatar_path.
func (s *Service) DeleteAvatar(ctx context.Context, actor store.User, clientIP string) (UserDetail, error) {
	if actor.AvatarPath != "" {
		_ = os.Remove(actor.AvatarPath)
	}
	if err := s.store.UpdateUserAvatar(ctx, actor.ID, ""); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return UserDetail{}, ErrNotFound
		}
		return UserDetail{}, err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "user.avatar_deleted",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    "removed avatar",
	})
	return s.GetUserDetail(ctx, actor.ID)
}

// GetAvatarPath resolves and verifies the on-disk avatar path for userID.
func (s *Service) GetAvatarPath(ctx context.Context, userID string) (string, error) {
	u, err := s.store.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", ErrNotFound
		}
		return "", err
	}
	if u.AvatarPath == "" {
		return "", ErrNotFound
	}
	if s.dataDir != "" {
		avatarRoot, err1 := filepath.Abs(filepath.Join(s.dataDir, "avatars"))
		avatarAbs, err2 := filepath.Abs(u.AvatarPath)
		if err1 != nil || err2 != nil || !strings.HasPrefix(avatarAbs, avatarRoot+string(filepath.Separator)) {
			return "", ErrNotFound
		}
	}
	if _, err := os.Stat(u.AvatarPath); err != nil {
		return "", ErrNotFound
	}
	return u.AvatarPath, nil
}

// CanAccessLibrary reports whether user is permitted to access libraryID.
func (s *Service) CanAccessLibrary(ctx context.Context, user store.User, libraryID string) (bool, error) {
	return s.store.CanUserAccessLibrary(ctx, user, libraryID)
}

// LibraryScope returns (allowedIDs, unrestricted, err) for scoping catalog queries to user.
func (s *Service) LibraryScope(ctx context.Context, user store.User) ([]string, bool, error) {
	return s.store.AccessibleLibraryIDs(ctx, user)
}

// CreateInvite creates a time-limited, optionally multi-use invitation token (admin-only).
func (s *Service) CreateInvite(ctx context.Context, actor store.User, req CreateInviteRequest, clientIP string) (CreatedInvite, error) {
	role := strings.TrimSpace(req.Role)
	if role == "" {
		role = "member"
	}
	if role != "admin" && role != "member" {
		return CreatedInvite{}, fmt.Errorf("%w: role must be 'admin' or 'member'", ErrValidation)
	}

	maxUses := req.MaxUses
	if maxUses == 0 {
		maxUses = 1
	}
	if maxUses < 1 || maxUses > 100 {
		return CreatedInvite{}, fmt.Errorf("%w: max_uses must be between 1 and 100", ErrValidation)
	}

	expiresInHours := req.ExpiresInHours
	if expiresInHours == 0 {
		expiresInHours = 72
	}
	if expiresInHours < 1 || expiresInHours > 720 {
		return CreatedInvite{}, fmt.Errorf("%w: expires_in_hours must be between 1 and 720", ErrValidation)
	}

	label := strings.TrimSpace(req.Label)
	if len(label) > 120 {
		return CreatedInvite{}, fmt.Errorf("%w: label must be at most 120 characters", ErrValidation)
	}

	allLibraries := true
	if req.AllLibraries != nil {
		allLibraries = *req.AllLibraries
	}
	if role == "admin" {
		allLibraries = true
	}

	var libIDs []string
	var err error
	if !allLibraries {
		libIDs, err = s.validateLibraryIDs(ctx, req.LibraryIDs)
		if err != nil {
			return CreatedInvite{}, err
		}
	}

	rawToken, err := randomToken(32)
	if err != nil {
		return CreatedInvite{}, err
	}
	prefix := rawToken
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}

	now := s.now().UTC()
	inv := store.UserInvite{
		ID:           uuid.NewString(),
		TokenHash:    hashToken(rawToken),
		TokenPrefix:  prefix,
		Label:        label,
		Role:         role,
		AllLibraries: allLibraries,
		RequireTOTP:  req.RequireTOTP,
		LibraryIDs:   libIDs,
		MaxUses:      maxUses,
		CreatedBy:    actor.ID,
		ExpiresAt:    now.Add(time.Duration(expiresInHours) * time.Hour),
	}
	if err := s.store.CreateUserInvite(ctx, inv); err != nil {
		return CreatedInvite{}, err
	}
	stored, err := s.store.GetUserInviteByID(ctx, inv.ID)
	if err != nil {
		return CreatedInvite{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.invite_created",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("created %s invite %s (max_uses=%d)", role, stored.ID, maxUses),
	})
	return CreatedInvite{Invite: stored, Token: rawToken}, nil
}

// ListInvites returns all invites ordered newest first (admin-only).
func (s *Service) ListInvites(ctx context.Context) ([]store.UserInvite, error) {
	return s.store.ListUserInvites(ctx)
}

// RevokeInvite revokes an invite by ID (admin-only).
func (s *Service) RevokeInvite(ctx context.Context, actor store.User, inviteID, clientIP string) error {
	if err := s.store.RevokeUserInvite(ctx, inviteID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.invite_revoked",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("revoked invite %s", inviteID),
	})
	return nil
}

// ClearInactiveInvites deletes revoked, expired, and exhausted invites (admin-only).
func (s *Service) ClearInactiveInvites(ctx context.Context, actor store.User, clientIP string) (int64, error) {
	n, err := s.store.DeleteInactiveUserInvites(ctx, s.now())
	if err != nil {
		return 0, err
	}
	if n > 0 {
		_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "admin.invites_cleared",
			ActorID:   actor.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("cleared %d inactive invite(s)", n),
		})
	}
	return n, nil
}

// InspectInvite validates an invite token for onboarding UI display.
func (s *Service) InspectInvite(ctx context.Context, rawToken, clientIP string) (store.UserInvite, error) {
	if allowed, retryAfter := s.limiter.Check(clientIP, "invite"); !allowed {
		return store.UserInvite{}, &RateLimitError{RetryAfter: retryAfter}
	}
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return store.UserInvite{}, ErrNotFound
	}
	inv, err := s.store.GetUserInviteByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure(clientIP, "invite")
			return store.UserInvite{}, ErrNotFound
		}
		return store.UserInvite{}, err
	}
	now := s.now().UTC()
	if inv.RevokedAt != nil {
		return store.UserInvite{}, ErrInviteRevoked
	}
	if !inv.ExpiresAt.After(now) {
		return store.UserInvite{}, ErrInviteExpired
	}
	if inv.UseCount >= inv.MaxUses {
		return store.UserInvite{}, ErrInviteExhausted
	}
	return inv, nil
}

// AcceptInvite claims a valid invite token, creates the user account, and issues a session.
func (s *Service) AcceptInvite(ctx context.Context, rawToken, username, displayName, password, clientKind, clientName, clientIP, userAgent string) (AuthResult, error) {
	if allowed, retryAfter := s.limiter.Check(clientIP, "invite"); !allowed {
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}

	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		s.limiter.RecordFailure(clientIP, "invite")
		return AuthResult{}, ErrNotFound
	}
	tokenHash := hashToken(rawToken)
	inviteKey := "invite:" + tokenHash[:12]
	if allowed, retryAfter := s.limiter.Check(clientIP, inviteKey); !allowed {
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}

	// Validate the invite BEFORE running Argon2id hashing to prevent CPU DoS.
	invPre, err := s.store.GetUserInviteByTokenHash(ctx, tokenHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, ErrNotFound
		}
		return AuthResult{}, err
	}
	now := s.now().UTC()
	if invPre.RevokedAt != nil {
		s.limiter.RecordFailure(clientIP, inviteKey)
		return AuthResult{}, ErrInviteRevoked
	}
	if !invPre.ExpiresAt.After(now) {
		s.limiter.RecordFailure(clientIP, inviteKey)
		return AuthResult{}, ErrInviteExpired
	}
	if invPre.UseCount >= invPre.MaxUses {
		s.limiter.RecordFailure(clientIP, inviteKey)
		return AuthResult{}, ErrInviteExhausted
	}

	username, err = validateUsername(username)
	if err != nil {
		return AuthResult{}, err
	}
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		displayName = username
	}
	if len(displayName) > 64 {
		return AuthResult{}, fmt.Errorf("%w: display_name must be at most 64 characters", ErrValidation)
	}
	if clientKind == "" {
		clientKind = "web"
	}
	if clientKind != "web" && clientKind != "app" {
		return AuthResult{}, fmt.Errorf("%w: client_kind must be 'web' or 'app'", ErrValidation)
	}
	if clientName == "" {
		if clientKind == "web" {
			clientName = "Lune Web"
		} else {
			clientName = "Lune App"
		}
	}

	hash, err := HashPassword(password)
	if err != nil {
		return AuthResult{}, fmt.Errorf("%w: %v", ErrValidation, err)
	}

	user := store.User{
		ID:           uuid.NewString(),
		Username:     username,
		DisplayName:  displayName,
		PasswordHash: hash,
	}
	created, inv, err := s.store.AcceptUserInvite(ctx, tokenHash, user, s.now().UTC())
	if err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, ErrNotFound
		case errors.Is(err, store.ErrInviteRevoked):
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, ErrInviteRevoked
		case errors.Is(err, store.ErrInviteExpired):
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, ErrInviteExpired
		case errors.Is(err, store.ErrInviteExhausted):
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, ErrInviteExhausted
		case errors.Is(err, store.ErrAlreadyExists):
			s.limiter.RecordFailure(clientIP, inviteKey)
			return AuthResult{}, fmt.Errorf("%w: username already exists", ErrConflict)
		default:
			return AuthResult{}, err
		}
	}

	s.limiter.RecordSuccess(clientIP, inviteKey)
	s.limiter.RecordSuccess(clientIP, "invite")
	res, err := s.issueSession(ctx, created, clientKind, clientName, clientIP, userAgent)
	if err != nil {
		return AuthResult{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.invite_accepted",
		ActorID:   created.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("user %q registered via invite %s", created.Username, inv.ID),
	})
	return res, nil
}

// AddUserMark adds a media item to the user's watchlist or favorites after checking library access.
func (s *Service) AddUserMark(ctx context.Context, user store.User, itemID, kind string) error {
	item, err := s.store.GetMediaItem(ctx, itemID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	allowed, err := s.store.CanUserAccessLibrary(ctx, user, item.LibraryID)
	if err != nil {
		return err
	}
	if !allowed {
		return ErrForbidden
	}
	return s.store.AddUserMediaMark(ctx, user.ID, itemID, kind)
}

// RemoveUserMark removes a media item from the user's watchlist or favorites.
func (s *Service) RemoveUserMark(ctx context.Context, user store.User, itemID, kind string) error {
	return s.store.RemoveUserMediaMark(ctx, user.ID, itemID, kind)
}

// GetItemMarks returns whether itemID is in the user's watchlist and/or favorites.
func (s *Service) GetItemMarks(ctx context.Context, userID, itemID string) (inWatchlist bool, isFavorite bool, err error) {
	return s.store.GetUserMediaMarksForItem(ctx, userID, itemID)
}

// GetDismissedAlerts returns the persisted list of cleared console alert IDs.
func (s *Service) GetDismissedAlerts(ctx context.Context) ([]string, error) {
	raw, err := s.store.GetSetting(ctx, dismissedAlertsKey)
	if errors.Is(err, store.ErrNotFound) || raw == "" {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	if err := json.Unmarshal([]byte(raw), &ids); err != nil {
		return []string{}, nil
	}
	if ids == nil {
		return []string{}, nil
	}
	return ids, nil
}

// DismissAlerts appends alert IDs to the persisted dismissed set (admin-only at API layer).
func (s *Service) DismissAlerts(ctx context.Context, newIDs []string) ([]string, error) {
	existing, err := s.GetDismissedAlerts(ctx)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(existing)+len(newIDs))
	merged := make([]string, 0, len(existing)+len(newIDs))
	for _, id := range existing {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	for _, id := range newIDs {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 160 || seen[id] {
			continue
		}
		seen[id] = true
		merged = append(merged, id)
	}
	if len(merged) > maxStoredDismissedAlerts {
		merged = merged[len(merged)-maxStoredDismissedAlerts:]
	}
	b, err := json.Marshal(merged)
	if err != nil {
		return nil, err
	}
	if err := s.store.SetSetting(ctx, dismissedAlertsKey, string(b)); err != nil {
		return nil, err
	}
	return merged, nil
}

// ListUserMarks returns the user's watchlist or favorite items scoped to accessible libraries.
func (s *Service) ListUserMarks(ctx context.Context, user store.User, kind string, limit, offset int) ([]store.MediaItem, int, error) {
	allowedIDs, unrestricted, err := s.store.AccessibleLibraryIDs(ctx, user)
	if err != nil {
		return nil, 0, err
	}
	return s.store.ListUserMediaMarks(ctx, user.ID, kind, allowedIDs, !unrestricted, limit, offset)
}

func (s *Service) validateLibraryIDs(ctx context.Context, raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	clean := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		clean = append(clean, id)
	}
	if len(clean) == 0 {
		return clean, nil
	}
	existing, err := s.store.ExistingLibraryIDs(ctx, clean)
	if err != nil {
		return nil, err
	}
	for _, id := range clean {
		if !existing[id] {
			return nil, fmt.Errorf("%w: unknown library_id %q", ErrValidation, id)
		}
	}
	return clean, nil
}

func validateUsername(raw string) (string, error) {
	u := strings.TrimSpace(raw)
	if len(u) < 2 || len(u) > 64 {
		return "", fmt.Errorf("%w: username must be between 2 and 64 characters", ErrValidation)
	}
	for _, r := range u {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return "", fmt.Errorf("%w: username must not contain spaces or control characters", ErrValidation)
		}
	}
	return u, nil
}
