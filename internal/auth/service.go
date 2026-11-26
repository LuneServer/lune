package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/LuneServer/lune/internal/security"
	"github.com/LuneServer/lune/internal/store"
)

const (
	webSessionTTL      = 14 * 24 * time.Hour
	appAccessTokenTTL  = 1 * time.Hour
	appRefreshTokenTTL = 90 * 24 * time.Hour
	deviceCodeTTL      = 10 * time.Minute
	devicePollInterval = 5 // seconds
)

var (
	ErrInvalidCredentials = errors.New("invalid username or password")
	ErrTOTPRequired       = errors.New("two-factor authentication code required")
	ErrInvalidTOTPCode    = errors.New("invalid two-factor authentication code")
	ErrAccountDisabled    = errors.New("account is disabled")
	ErrSetupCompleted     = errors.New("initial setup already completed")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrForbidden          = errors.New("forbidden")
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrDevicePending      = errors.New("authorization pending")
	ErrDeviceExpired      = errors.New("device code expired")
	ErrDeviceDenied       = errors.New("device authorization denied")
	ErrInviteExpired      = errors.New("invite expired")
	ErrInviteRevoked      = errors.New("invite revoked")
	ErrInviteExhausted    = errors.New("invite has reached its maximum uses")
	ErrPayloadTooLarge    = errors.New("payload too large")
	ErrValidation         = errors.New("validation error")
)

// RateLimitError signals that the client is temporarily locked out.
type RateLimitError struct {
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	return fmt.Sprintf("too many failed attempts; retry in %s", e.RetryAfter.Round(time.Second))
}

// Service implements Lune's identity, session, device-login, and audit logic.
type Service struct {
	store          *store.Store
	limiter        *security.AuthRateLimiter
	dataDir        string
	now            func() time.Time
	onLockoutMu    sync.RWMutex
	onLockout      func(ip, reason string)
	devicePollMu   sync.Mutex
	deviceLastPoll map[string]time.Time
}

// NewService constructs an auth Service.
func NewService(s *store.Store, limiter *security.AuthRateLimiter) *Service {
	if limiter == nil {
		limiter = security.NewAuthRateLimiter()
	}
	return &Service{
		store:          s,
		limiter:        limiter,
		now:            time.Now,
		deviceLastPoll: make(map[string]time.Time),
	}
}

// SetDataDir configures the base data directory used for user assets such as avatars.
func (s *Service) SetDataDir(dir string) {
	s.dataDir = strings.TrimSpace(dir)
}

// SetOnLockout registers a callback invoked whenever an IP triggers an auth lockout.
func (s *Service) SetOnLockout(fn func(ip, reason string)) {
	s.onLockoutMu.Lock()
	s.onLockout = fn
	s.onLockoutMu.Unlock()
}

// TOTPSetupResult holds newly generated TOTP enrollment material for a user.
type TOTPSetupResult struct {
	Secret     string
	OTPAuthURL string
	Issuer     string
	Account    string
}

type AuthResult struct {
	User         store.User
	Session      store.Session
	AccessToken  string
	RefreshToken string
}

type DeviceStartResult struct {
	DeviceCode string
	UserCode   string
	ExpiresIn  int
	Interval   int
}

// SetupAdmin creates the first administrator account and opens an initial web session.
func (s *Service) SetupAdmin(ctx context.Context, username, displayName, password, clientIP, userAgent string) (AuthResult, error) {
	if allowed, retryAfter := s.limiter.Check("setup:"+clientIP, ""); !allowed {
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}

	hasUsers, err := s.store.HasUsers(ctx)
	if err != nil {
		return AuthResult{}, err
	}
	if hasUsers {
		s.limiter.RecordFailure("setup:"+clientIP, "")
		return AuthResult{}, ErrSetupCompleted
	}

	username = strings.TrimSpace(username)
	displayName = strings.TrimSpace(displayName)
	if len(username) < 2 || len(username) > 64 {
		return AuthResult{}, fmt.Errorf("%w: username must be between 2 and 64 characters", ErrValidation)
	}
	if displayName == "" {
		displayName = username
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
		Role:         "admin",
		AllLibraries: true,
	}
	if err := s.store.CreateInitialAdmin(ctx, user); err != nil {
		if errors.Is(err, store.ErrSetupAlreadyComplete) {
			s.limiter.RecordFailure("setup:"+clientIP, "")
			return AuthResult{}, ErrSetupCompleted
		}
		return AuthResult{}, err
	}

	_ = s.store.TouchUserLastLogin(ctx, user.ID)
	if refreshed, err := s.store.GetUserByID(ctx, user.ID); err == nil {
		user = refreshed
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "setup.completed",
		ActorID:   user.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("initial admin %q created", user.Username),
	})

	return s.issueSession(ctx, user, "web", "Lune Web", clientIP, userAgent)
}

// Login authenticates a user with password, enforces adaptive rate limiting, and issues tokens.
func (s *Service) Login(ctx context.Context, username, password, clientKind, clientName, clientIP, userAgent string) (AuthResult, error) {
	return s.LoginWithTOTP(ctx, username, password, "", clientKind, clientName, clientIP, userAgent)
}

// LoginWithTOTP authenticates a user with password and optional TOTP/recovery code.
func (s *Service) LoginWithTOTP(ctx context.Context, username, password, totpCode, clientKind, clientName, clientIP, userAgent string) (AuthResult, error) {
	username = strings.TrimSpace(username)
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

	if allowed, retryAfter := s.limiter.Check(clientIP, strings.ToLower(username)); !allowed {
		_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "auth.rate_limited",
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("blocked login attempt for %q", username),
		})
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}

	user, err := s.store.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Equalize Argon2id timing against invalid usernames.
			_ = VerifyPassword(password, dummyHash)
			s.recordLoginFailure(ctx, clientIP, username, "unknown username")
			return AuthResult{}, ErrInvalidCredentials
		}
		return AuthResult{}, err
	}

	if !VerifyPassword(password, user.PasswordHash) {
		s.recordLoginFailure(ctx, clientIP, username, "bad password")
		return AuthResult{}, ErrInvalidCredentials
	}
	if user.Disabled {
		_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "auth.disabled",
			ActorID:   user.ID,
			ClientIP:  clientIP,
			Detail:    "disabled account login rejected",
		})
		return AuthResult{}, ErrAccountDisabled
	}

	if user.TOTPEnabled {
		code := strings.TrimSpace(totpCode)
		if code == "" {
			return AuthResult{}, ErrTOTPRequired
		}
		if !s.verifyUserTOTPOrRecovery(ctx, user, code, clientIP) {
			s.recordLoginFailure(ctx, clientIP, username, "bad totp code")
			return AuthResult{}, ErrInvalidTOTPCode
		}
	}

	s.limiter.RecordSuccess(clientIP, strings.ToLower(username))
	_ = s.store.TouchUserLastLogin(ctx, user.ID)
	if refreshed, err := s.store.GetUserByID(ctx, user.ID); err == nil {
		user = refreshed
	}
	res, err := s.issueSession(ctx, user, clientKind, clientName, clientIP, userAgent)
	if err != nil {
		return AuthResult{}, err
	}

	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.login",
		ActorID:   user.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("%s session (%s)", clientKind, clientName),
	})
	return res, nil
}

func (s *Service) verifyUserTOTPOrRecovery(ctx context.Context, user store.User, code, clientIP string) bool {
	if VerifyTOTPCode(user.TOTPSecret, code, s.now()) {
		return true
	}
	if strings.TrimSpace(user.TOTPRecoveryHashes) == "" {
		return false
	}
	var hashes []string
	if err := json.Unmarshal([]byte(user.TOTPRecoveryHashes), &hashes); err != nil || len(hashes) == 0 {
		return false
	}
	matched, remaining := consumeRecoveryCode(code, hashes)
	if !matched {
		return false
	}
	raw, _ := json.Marshal(remaining)
	_ = s.store.UpdateUserTOTPRecoveryHashes(ctx, user.ID, string(raw))
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.totp_recovery_used",
		ActorID:   user.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("recovery code consumed (%d remaining)", len(remaining)),
	})
	return true
}

// BeginTOTPSetup generates a pending TOTP secret for the authenticated user.
func (s *Service) BeginTOTPSetup(ctx context.Context, user store.User) (TOTPSetupResult, error) {
	if user.TOTPEnabled {
		return TOTPSetupResult{}, fmt.Errorf("%w: two-factor authentication is already enabled", ErrConflict)
	}
	secret, err := GenerateTOTPSecret()
	if err != nil {
		return TOTPSetupResult{}, err
	}
	if err := s.store.SetUserTOTPPending(ctx, user.ID, secret); err != nil {
		return TOTPSetupResult{}, err
	}
	return TOTPSetupResult{
		Secret:     secret,
		OTPAuthURL: BuildTOTPURI(user.Username, secret),
		Issuer:     totpIssuer,
		Account:    user.Username,
	}, nil
}

// EnableTOTP verifies password + 6-digit code against the pending secret, enables TOTP,
// and returns 8 one-time recovery codes.
func (s *Service) EnableTOTP(ctx context.Context, user store.User, password, code, clientIP string) ([]string, error) {
	fresh, err := s.store.GetUserByID(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if fresh.TOTPEnabled {
		return nil, fmt.Errorf("%w: two-factor authentication is already enabled", ErrConflict)
	}
	if fresh.TOTPSecret == "" {
		return nil, fmt.Errorf("%w: start two-factor setup first", ErrValidation)
	}
	if !VerifyPassword(password, fresh.PasswordHash) {
		return nil, ErrInvalidCredentials
	}
	if !VerifyTOTPCode(fresh.TOTPSecret, code, s.now()) {
		return nil, ErrInvalidTOTPCode
	}
	plain, hashes, err := GenerateRecoveryCodes()
	if err != nil {
		return nil, err
	}
	rawHashes, err := json.Marshal(hashes)
	if err != nil {
		return nil, err
	}
	if err := s.store.EnableUserTOTP(ctx, fresh.ID, string(rawHashes)); err != nil {
		return nil, err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.totp_enabled",
		ActorID:   fresh.ID,
		ClientIP:  clientIP,
		Detail:    "two-factor authentication enabled",
	})
	return plain, nil
}

// DisableTOTP disables TOTP for the authenticated user after verifying password and current code.
func (s *Service) DisableTOTP(ctx context.Context, user store.User, password, code, clientIP string) error {
	fresh, err := s.store.GetUserByID(ctx, user.ID)
	if err != nil {
		return err
	}
	if !fresh.TOTPEnabled {
		return nil
	}
	if fresh.TOTPRequired {
		return fmt.Errorf("%w: two-factor authentication is required for your account by an administrator", ErrForbidden)
	}
	if !VerifyPassword(password, fresh.PasswordHash) {
		return ErrInvalidCredentials
	}
	if !s.verifyUserTOTPOrRecovery(ctx, fresh, code, clientIP) {
		return ErrInvalidTOTPCode
	}
	if err := s.store.DisableUserTOTP(ctx, fresh.ID); err != nil {
		return err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.totp_disabled",
		ActorID:   fresh.ID,
		ClientIP:  clientIP,
		Detail:    "two-factor authentication disabled",
	})
	return nil
}

// AdminResetTOTP allows an administrator to disable TOTP for a locked-out user.
func (s *Service) AdminResetTOTP(ctx context.Context, actor store.User, targetUserID, clientIP string) error {
	if actor.Role != "admin" {
		return ErrForbidden
	}
	target, err := s.store.GetUserByID(ctx, targetUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if err := s.store.DisableUserTOTP(ctx, target.ID); err != nil {
		return err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.totp_reset",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("reset two-factor authentication for %q", target.Username),
	})
	return nil
}

// Authenticate validates an opaque access/session token and returns its user & session.
func (s *Service) Authenticate(ctx context.Context, rawToken, clientIP string) (store.User, store.Session, error) {
	if rawToken == "" {
		return store.User{}, store.Session{}, ErrUnauthorized
	}

	sess, err := s.store.GetSessionByTokenHash(ctx, hashToken(rawToken))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return store.User{}, store.Session{}, ErrUnauthorized
		}
		return store.User{}, store.Session{}, err
	}

	now := s.now().UTC()
	if sess.RevokedAt != nil || !sess.ExpiresAt.After(now) {
		return store.User{}, store.Session{}, ErrUnauthorized
	}

	user, err := s.store.GetUserByID(ctx, sess.UserID)
	if err != nil || user.Disabled {
		return store.User{}, store.Session{}, ErrUnauthorized
	}

	if now.Sub(sess.LastSeenAt) > time.Minute {
		_ = s.store.TouchSession(ctx, sess.ID, clientIP)
	}
	return user, sess, nil
}

// Refresh rotates an app session's access and refresh tokens.
func (s *Service) Refresh(ctx context.Context, rawRefreshToken, clientIP string) (AuthResult, error) {
	if allowed, retryAfter := s.limiter.Check("refresh:"+clientIP, ""); !allowed {
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}
	if rawRefreshToken == "" {
		s.limiter.RecordFailure("refresh:"+clientIP, "")
		return AuthResult{}, ErrUnauthorized
	}
	oldRefreshHash := hashToken(rawRefreshToken)
	sess, err := s.store.GetSessionByRefreshHash(ctx, oldRefreshHash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure("refresh:"+clientIP, "")
			return AuthResult{}, ErrUnauthorized
		}
		return AuthResult{}, err
	}
	now := s.now().UTC()
	if sess.RevokedAt != nil || sess.RefreshExpiresAt == nil || !sess.RefreshExpiresAt.After(now) {
		s.limiter.RecordFailure("refresh:"+clientIP, "")
		return AuthResult{}, ErrUnauthorized
	}

	user, err := s.store.GetUserByID(ctx, sess.UserID)
	if err != nil || user.Disabled {
		s.limiter.RecordFailure("refresh:"+clientIP, "")
		return AuthResult{}, ErrUnauthorized
	}

	newAccess, err := randomToken(32)
	if err != nil {
		return AuthResult{}, err
	}
	newRefresh, err := randomToken(32)
	if err != nil {
		return AuthResult{}, err
	}
	newExp := now.Add(appAccessTokenTTL)
	newRefreshExp := now.Add(appRefreshTokenTTL)

	if err := s.store.RotateSessionTokens(ctx, sess.ID, oldRefreshHash, hashToken(newAccess), hashToken(newRefresh), newExp, newRefreshExp, clientIP); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure("refresh:"+clientIP, "")
			return AuthResult{}, ErrUnauthorized
		}
		return AuthResult{}, err
	}

	sess.ExpiresAt = newExp
	sess.RefreshExpiresAt = &newRefreshExp
	return AuthResult{
		User:         user,
		Session:      sess,
		AccessToken:  newAccess,
		RefreshToken: newRefresh,
	}, nil
}

// RevokeSession revokes a session if owned by actor or if actor is an admin.
func (s *Service) RevokeSession(ctx context.Context, actor store.User, sessionID, clientIP string) error {
	sess, err := s.store.GetSessionByID(ctx, sessionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if sess.UserID != actor.ID && actor.Role != "admin" {
		return ErrForbidden
	}
	if err := s.store.RevokeSession(ctx, sessionID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.session_revoked",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("session %s revoked", sessionID),
	})
	return nil
}

// ListSessions returns active sessions for the given user.
func (s *Service) ListSessions(ctx context.Context, userID string) ([]store.Session, error) {
	return s.store.ListUserSessions(ctx, userID)
}

// StartDeviceFlow initiates RFC 8628 device authorization for TV/mobile apps.
func (s *Service) StartDeviceFlow(ctx context.Context, clientName, clientIP string) (DeviceStartResult, error) {
	if allowed, retryAfter := s.limiter.Check("device_start:"+clientIP, ""); !allowed {
		return DeviceStartResult{}, &RateLimitError{RetryAfter: retryAfter}
	}
	s.limiter.RecordFailure("device_start:"+clientIP, "")

	if clientName == "" {
		clientName = "Lune App"
	}
	rawDeviceCode, err := randomToken(32)
	if err != nil {
		return DeviceStartResult{}, err
	}
	userCode, err := generateUserCode()
	if err != nil {
		return DeviceStartResult{}, err
	}

	dc := store.DeviceCode{
		DeviceCodeHash: hashToken(rawDeviceCode),
		UserCode:       userCode,
		ClientName:     clientName,
		ClientIP:       clientIP,
		ExpiresAt:      s.now().UTC().Add(deviceCodeTTL),
	}
	if err := s.store.CreateDeviceCode(ctx, dc); err != nil {
		return DeviceStartResult{}, err
	}

	return DeviceStartResult{
		DeviceCode: rawDeviceCode,
		UserCode:   userCode,
		ExpiresIn:  int(deviceCodeTTL.Seconds()),
		Interval:   devicePollInterval,
	}, nil
}

// ApproveDeviceFlow authorizes a pending user_code on behalf of an authenticated user.
func (s *Service) ApproveDeviceFlow(ctx context.Context, actor store.User, userCode, clientIP string) error {
	if allowed, retryAfter := s.limiter.Check("device_approve:"+clientIP, "device_approve:"+actor.ID); !allowed {
		return &RateLimitError{RetryAfter: retryAfter}
	}
	normalized := normalizeUserCode(userCode)
	if err := s.store.ApproveDeviceCode(ctx, normalized, actor.ID, s.now().UTC()); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure("device_approve:"+clientIP, "device_approve:"+actor.ID)
			return ErrNotFound
		}
		return err
	}
	s.limiter.RecordSuccess("device_approve:"+clientIP, "device_approve:"+actor.ID)
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.device_approved",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("device code %s approved", normalized),
	})
	return nil
}

// PollDeviceFlow exchanges an approved device_code for app access + refresh tokens.
func (s *Service) PollDeviceFlow(ctx context.Context, rawDeviceCode, clientIP, userAgent string) (AuthResult, error) {
	if allowed, retryAfter := s.limiter.Check("device_poll:"+clientIP, ""); !allowed {
		return AuthResult{}, &RateLimitError{RetryAfter: retryAfter}
	}
	if rawDeviceCode == "" {
		s.limiter.RecordFailure("device_poll:"+clientIP, "")
		return AuthResult{}, ErrUnauthorized
	}
	now := s.now().UTC()
	codeHash := hashToken(rawDeviceCode)
	dc, consumedNow, err := s.store.ConsumeApprovedDeviceCode(ctx, codeHash, now)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.limiter.RecordFailure("device_poll:"+clientIP, "")
			return AuthResult{}, ErrDeviceExpired
		}
		return AuthResult{}, err
	}
	if !dc.ExpiresAt.After(now) {
		s.clearDevicePollInterval(codeHash)
		s.limiter.RecordFailure("device_poll:"+clientIP, "")
		return AuthResult{}, ErrDeviceExpired
	}

	if consumedNow {
		s.clearDevicePollInterval(codeHash)
		if dc.ApprovedUserID == "" {
			return AuthResult{}, ErrDeviceExpired
		}
		user, err := s.store.GetUserByID(ctx, dc.ApprovedUserID)
		if err != nil || user.Disabled {
			return AuthResult{}, ErrUnauthorized
		}
		_ = s.store.TouchUserLastLogin(ctx, user.ID)
		if refreshed, err := s.store.GetUserByID(ctx, user.ID); err == nil {
			user = refreshed
		}
		res, err := s.issueSession(ctx, user, "app", dc.ClientName, clientIP, userAgent)
		if err != nil {
			return AuthResult{}, err
		}
		_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
			EventType: "auth.device_login",
			ActorID:   user.ID,
			ClientIP:  clientIP,
			Detail:    fmt.Sprintf("device %q authenticated", dc.ClientName),
		})
		return res, nil
	}

	switch dc.Status {
	case "pending":
		if wait := s.checkDevicePollInterval(codeHash, now); wait > 0 {
			return AuthResult{}, &RateLimitError{RetryAfter: wait}
		}
		return AuthResult{}, ErrDevicePending
	case "denied":
		s.clearDevicePollInterval(codeHash)
		return AuthResult{}, ErrDeviceDenied
	default:
		s.clearDevicePollInterval(codeHash)
		s.limiter.RecordFailure("device_poll:"+clientIP, "")
		return AuthResult{}, ErrDeviceExpired
	}
}

func (s *Service) checkDevicePollInterval(codeHash string, now time.Time) time.Duration {
	minInterval := time.Duration(devicePollInterval) * time.Second
	s.devicePollMu.Lock()
	defer s.devicePollMu.Unlock()

	if len(s.deviceLastPoll) > 1024 {
		for k, ts := range s.deviceLastPoll {
			if now.Sub(ts) > deviceCodeTTL {
				delete(s.deviceLastPoll, k)
			}
		}
	}
	if last, ok := s.deviceLastPoll[codeHash]; ok {
		if elapsed := now.Sub(last); elapsed < minInterval {
			return minInterval - elapsed
		}
	}
	s.deviceLastPoll[codeHash] = now
	return 0
}

func (s *Service) clearDevicePollInterval(codeHash string) {
	s.devicePollMu.Lock()
	delete(s.deviceLastPoll, codeHash)
	s.devicePollMu.Unlock()
}

// ListAuditEvents returns recent security events (admin-only at the API layer).
func (s *Service) ListAuditEvents(ctx context.Context, limit int) ([]store.AuditEvent, error) {
	return s.store.ListAuditEvents(ctx, limit)
}

// ListUsers returns all user accounts (admin-only).
func (s *Service) ListUsers(ctx context.Context) ([]store.User, error) {
	return s.store.ListUsers(ctx)
}

// CreateUser creates a new member or admin account (admin-only).
func (s *Service) CreateUser(ctx context.Context, actor store.User, username, displayName, password, role, clientIP string) (store.User, error) {
	detail, err := s.CreateUserWithAccess(ctx, actor, CreateUserRequest{
		Username:    username,
		DisplayName: displayName,
		Password:    password,
		Role:        role,
	}, clientIP)
	if err != nil {
		return store.User{}, err
	}
	return detail.User, nil
}

// UpdateUser toggles disabled state and/or resets password (admin-only).
func (s *Service) UpdateUser(ctx context.Context, actor store.User, targetID string, disabled *bool, newPassword *string, clientIP string) (store.User, error) {
	detail, err := s.UpdateUserDetailed(ctx, actor, targetID, UpdateUserRequest{
		Disabled: disabled,
		Password: newPassword,
	}, clientIP)
	if err != nil {
		return store.User{}, err
	}
	return detail.User, nil
}

// DeleteUser removes a user account (protecting self and the last active administrator).
func (s *Service) DeleteUser(ctx context.Context, actor store.User, targetID, clientIP string) error {
	if targetID == actor.ID {
		return fmt.Errorf("%w: cannot delete your own account", ErrValidation)
	}
	target, err := s.store.GetUserByID(ctx, targetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if target.Role == "admin" && !target.Disabled {
		if admins, err := s.store.CountActiveAdmins(ctx); err == nil && admins <= 1 {
			return fmt.Errorf("%w: cannot delete the last active administrator", ErrValidation)
		}
	}
	if err := s.store.DeleteUser(ctx, targetID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	if target.AvatarPath != "" {
		_ = os.Remove(target.AvatarPath)
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "admin.user_deleted",
		ActorID:   actor.ID,
		ClientIP:  clientIP,
		Detail:    fmt.Sprintf("deleted user %q", target.Username),
	})
	return nil
}

func (s *Service) issueSession(ctx context.Context, user store.User, clientKind, clientName, clientIP, userAgent string) (AuthResult, error) {
	accessToken, err := randomToken(32)
	if err != nil {
		return AuthResult{}, err
	}

	now := s.now().UTC()
	sess := store.Session{
		ID:         uuid.NewString(),
		UserID:     user.ID,
		TokenHash:  hashToken(accessToken),
		ClientName: clientName,
		ClientKind: clientKind,
		IPAddress:  clientIP,
		UserAgent:  userAgent,
	}

	var refreshToken string
	if clientKind == "app" {
		refreshToken, err = randomToken(32)
		if err != nil {
			return AuthResult{}, err
		}
		sess.RefreshHash = hashToken(refreshToken)
		sess.ExpiresAt = now.Add(appAccessTokenTTL)
		re := now.Add(appRefreshTokenTTL)
		sess.RefreshExpiresAt = &re
	} else {
		sess.ExpiresAt = now.Add(webSessionTTL)
	}

	if err := s.store.CreateSession(ctx, sess); err != nil {
		return AuthResult{}, err
	}

	return AuthResult{
		User:         user,
		Session:      sess,
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
	}, nil
}

func (s *Service) recordLoginFailure(ctx context.Context, clientIP, username, reason string) {
	locked, retryAfter := s.limiter.RecordFailure(clientIP, strings.ToLower(username))
	detail := fmt.Sprintf("failed login for %q (%s)", username, reason)
	if locked {
		detail += fmt.Sprintf("; locked for %s", retryAfter.Round(time.Second))
		s.onLockoutMu.RLock()
		fn := s.onLockout
		s.onLockoutMu.RUnlock()
		if fn != nil {
			fn(clientIP, "repeated auth lockouts")
		}
	}
	_ = s.store.InsertAuditEvent(ctx, store.AuditEvent{
		EventType: "auth.failed",
		ClientIP:  clientIP,
		Detail:    detail,
	})
}

func randomToken(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Unambiguous uppercase alphabet (no 0/O/1/I/L) for TV remote readability.
const userCodeAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

func generateUserCode() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	chars := make([]byte, 8)
	for i := range b {
		chars[i] = userCodeAlphabet[int(b[i])%len(userCodeAlphabet)]
	}
	return string(chars[:4]) + "-" + string(chars[4:]), nil
}

func normalizeUserCode(raw string) string {
	clean := strings.ToUpper(strings.TrimSpace(raw))
	clean = strings.ReplaceAll(clean, " ", "")
	clean = strings.ReplaceAll(clean, "-", "")
	if len(clean) == 8 {
		return clean[:4] + "-" + clean[4:]
	}
	return strings.ToUpper(strings.TrimSpace(raw))
}
