package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

var (
	ErrNotFound             = errors.New("record not found")
	ErrAlreadyExists        = errors.New("record already exists")
	ErrSetupAlreadyComplete = errors.New("initial setup already completed")
	ErrInviteExpired        = errors.New("invite expired")
	ErrInviteRevoked        = errors.New("invite revoked")
	ErrInviteExhausted      = errors.New("invite exhausted")
)

type User struct {
	ID                  string
	Username            string
	DisplayName         string
	PasswordHash        string
	Role                string // "admin" | "member"
	Disabled            bool
	AllLibraries        bool
	CanRequest          bool
	AutoApproveRequests bool
	AvatarPath          string
	PreferencesJSON     string
	TOTPSecret          string
	TOTPEnabled         bool
	TOTPRequired        bool
	TOTPRecoveryHashes  string
	LastLoginAt         *time.Time
	CreatedAt           time.Time
	UpdatedAt           time.Time
}

// HasUnrestrictedLibraryAccess reports whether the user can access every library.
func (u User) HasUnrestrictedLibraryAccess() bool {
	return u.Role == "admin" || u.AllLibraries
}

type Session struct {
	ID               string
	UserID           string
	TokenHash        string
	RefreshHash      string
	ClientName       string
	ClientKind       string // "web" | "app"
	IPAddress        string
	UserAgent        string
	ExpiresAt        time.Time
	RefreshExpiresAt *time.Time
	LastSeenAt       time.Time
	RevokedAt        *time.Time
	CreatedAt        time.Time
}

type DeviceCode struct {
	DeviceCodeHash string
	UserCode       string
	ClientName     string
	ClientIP       string
	Status         string // "pending" | "approved" | "denied" | "consumed"
	ApprovedUserID string
	ExpiresAt      time.Time
	CreatedAt      time.Time
}

type AuditEvent struct {
	ID        int64
	EventType string
	ActorID   string
	ClientIP  string
	Detail    string
	CreatedAt time.Time
}

// CreateInitialAdmin atomically creates the first admin user only if zero users exist.
func (s *Store) CreateInitialAdmin(ctx context.Context, u User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(1) FROM users`).Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return ErrSetupAlreadyComplete
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, all_libraries)
		VALUES (?, ?, ?, ?, 'admin', 0, 1)
	`, u.ID, u.Username, u.DisplayName, u.PasswordHash)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CreateUser(ctx context.Context, u User) error {
	allLibs := 1
	if u.Role != "admin" && !u.AllLibraries {
		allLibs = 0
	}
	prefsJSON := u.PreferencesJSON
	if prefsJSON == "" {
		prefsJSON = "{}"
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, all_libraries, preferences_json)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?)
	`, u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, allLibs, prefsJSON)
	if err != nil && (err.Error() != "" && (containsUnique(err.Error()))) {
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, username, display_name, password_hash, role, disabled,
		       all_libraries, can_request, auto_approve_requests,
		       avatar_path, preferences_json, totp_secret, totp_enabled, totp_required, totp_recovery_hashes,
		       last_login_at, created_at, updated_at
		FROM users
		ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CountActiveAdmins(ctx context.Context) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(1) FROM users WHERE role = 'admin' AND disabled = 0`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) SetUserDisabled(ctx context.Context, userID string, disabled bool) error {
	val := 0
	if disabled {
		val = 1
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET disabled = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, val, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if disabled {
		_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE user_id = ? AND revoked_at IS NULL`, userID)
	}
	return nil
}

func (s *Store) UpdateUserPassword(ctx context.Context, userID, passwordHash string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET password_hash = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, passwordHash, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	// Revoke existing sessions on password reset.
	_, _ = s.db.ExecContext(ctx, `UPDATE sessions SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now') WHERE user_id = ? AND revoked_at IS NULL`, userID)
	return nil
}

func (s *Store) DeleteUser(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func containsUnique(s string) bool {
	for i := 0; i+6 <= len(s); i++ {
		if s[i:i+6] == "UNIQUE" {
			return true
		}
	}
	return false
}

func (s *Store) GetUserByUsername(ctx context.Context, username string) (User, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, role, disabled,
		       all_libraries, can_request, auto_approve_requests,
		       avatar_path, preferences_json, totp_secret, totp_enabled, totp_required, totp_recovery_hashes,
		       last_login_at, created_at, updated_at
		FROM users
		WHERE username = ? COLLATE NOCASE
	`, username)
	return scanUser(row)
}

func (s *Store) GetUserByID(ctx context.Context, id string) (User, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, username, display_name, password_hash, role, disabled,
		       all_libraries, can_request, auto_approve_requests,
		       avatar_path, preferences_json, totp_secret, totp_enabled, totp_required, totp_recovery_hashes,
		       last_login_at, created_at, updated_at
		FROM users
		WHERE id = ?
	`, id)
	return scanUser(row)
}

func scanUser(row rowScanner) (User, error) {
	var u User
	var disabled, allLibraries, canRequest, autoApproveRequests, totpEnabled, totpRequired int
	var lastLogin sql.NullString
	var createdStr, updatedStr string
	if err := row.Scan(
		&u.ID, &u.Username, &u.DisplayName, &u.PasswordHash, &u.Role, &disabled,
		&allLibraries, &canRequest, &autoApproveRequests,
		&u.AvatarPath, &u.PreferencesJSON, &u.TOTPSecret, &totpEnabled, &totpRequired, &u.TOTPRecoveryHashes,
		&lastLogin, &createdStr, &updatedStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return User{}, ErrNotFound
		}
		return User{}, err
	}
	u.Disabled = disabled != 0
	u.AllLibraries = allLibraries != 0
	u.CanRequest = canRequest != 0
	u.AutoApproveRequests = autoApproveRequests != 0
	u.TOTPEnabled = totpEnabled != 0
	u.TOTPRequired = totpRequired != 0
	if lastLogin.Valid && lastLogin.String != "" {
		t := parseTime(lastLogin.String)
		u.LastLoginAt = &t
	}
	u.CreatedAt = parseTime(createdStr)
	u.UpdatedAt = parseTime(updatedStr)
	return u, nil
}

func (s *Store) SetUserTOTPPending(ctx context.Context, userID, secret string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET totp_secret = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, secret, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) EnableUserTOTP(ctx context.Context, userID, recoveryHashesJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET totp_enabled = 1, totp_recovery_hashes = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND totp_secret != ''
	`, recoveryHashesJSON, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) UpdateUserTOTPRecoveryHashes(ctx context.Context, userID, recoveryHashesJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET totp_recovery_hashes = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, recoveryHashesJSON, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) DisableUserTOTP(ctx context.Context, userID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET totp_secret = '', totp_enabled = 0, totp_recovery_hashes = '', updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CreateSession(ctx context.Context, sess Session) error {
	var refreshHash sql.NullString
	if sess.RefreshHash != "" {
		refreshHash = sql.NullString{String: sess.RefreshHash, Valid: true}
	}
	var refreshExp sql.NullString
	if sess.RefreshExpiresAt != nil {
		refreshExp = sql.NullString{String: formatTime(*sess.RefreshExpiresAt), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			id, user_id, token_hash, refresh_hash, client_name, client_kind,
			ip_address, user_agent, expires_at, refresh_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, sess.ID, sess.UserID, sess.TokenHash, refreshHash, sess.ClientName, sess.ClientKind,
		sess.IPAddress, sess.UserAgent, formatTime(sess.ExpiresAt), refreshExp)
	return err
}

func (s *Store) GetSessionByTokenHash(ctx context.Context, tokenHash string) (Session, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, refresh_hash, client_name, client_kind,
		       ip_address, user_agent, expires_at, refresh_expires_at, last_seen_at, revoked_at, created_at
		FROM sessions
		WHERE token_hash = ?
	`, tokenHash)
	return scanSession(row)
}

func (s *Store) GetSessionByRefreshHash(ctx context.Context, refreshHash string) (Session, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, refresh_hash, client_name, client_kind,
		       ip_address, user_agent, expires_at, refresh_expires_at, last_seen_at, revoked_at, created_at
		FROM sessions
		WHERE refresh_hash = ?
	`, refreshHash)
	return scanSession(row)
}

func (s *Store) GetSessionByID(ctx context.Context, id string) (Session, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, refresh_hash, client_name, client_kind,
		       ip_address, user_agent, expires_at, refresh_expires_at, last_seen_at, revoked_at, created_at
		FROM sessions
		WHERE id = ?
	`, id)
	return scanSession(row)
}

func (s *Store) ListUserSessions(ctx context.Context, userID string) ([]Session, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, user_id, token_hash, refresh_hash, client_name, client_kind,
		       ip_address, user_agent, expires_at, refresh_expires_at, last_seen_at, revoked_at, created_at
		FROM sessions
		WHERE user_id = ? AND revoked_at IS NULL
		ORDER BY last_seen_at DESC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

func (s *Store) RotateSessionTokens(ctx context.Context, id, oldRefreshHash, newTokenHash, newRefreshHash string, newExp, newRefreshExp time.Time, ip string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET token_hash = ?, refresh_hash = ?, expires_at = ?, refresh_expires_at = ?,
		    ip_address = ?, last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND refresh_hash = ? AND revoked_at IS NULL
	`, newTokenHash, newRefreshHash, formatTime(newExp), formatTime(newRefreshExp), ip, id, oldRefreshHash)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return ErrNotFound
	}
	return nil
}

// PruneExpiredAuthState removes expired device codes and sessions that have been
// expired or revoked for more than 24 hours.
func (s *Store) PruneExpiredAuthState(ctx context.Context) error {
	now := time.Now().UTC()
	nowStr := formatTime(now)
	cutoff := formatTime(now.Add(-24 * time.Hour))

	if _, err := s.db.ExecContext(ctx, `DELETE FROM device_codes WHERE expires_at < ?`, nowStr); err != nil {
		return fmt.Errorf("prune device_codes: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM sessions
		WHERE (revoked_at IS NOT NULL AND revoked_at < ?)
		   OR (refresh_expires_at IS NOT NULL AND refresh_expires_at < ?)
		   OR (refresh_expires_at IS NULL AND expires_at < ?)
	`, cutoff, cutoff, cutoff); err != nil {
		return fmt.Errorf("prune sessions: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM ip_bans WHERE permanent = 0 AND expires_at IS NOT NULL AND expires_at < ?`, nowStr); err != nil {
		return fmt.Errorf("prune ip_bans: %w", err)
	}
	return nil
}

// StoredIPBan represents a persisted edge IP ban row in SQLite.
type StoredIPBan struct {
	IP        string
	Reason    string
	BannedAt  time.Time
	ExpiresAt *time.Time
	Permanent bool
}

func (s *Store) UpsertIPBan(ctx context.Context, b StoredIPBan) error {
	perm := 0
	if b.Permanent {
		perm = 1
	}
	var exp sql.NullString
	if !b.Permanent && b.ExpiresAt != nil {
		exp = sql.NullString{String: formatTime(*b.ExpiresAt), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO ip_bans (ip, reason, banned_at, expires_at, permanent)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(ip) DO UPDATE SET
			reason = excluded.reason,
			banned_at = excluded.banned_at,
			expires_at = excluded.expires_at,
			permanent = excluded.permanent
	`, b.IP, b.Reason, formatTime(b.BannedAt), exp, perm)
	return err
}

func (s *Store) DeleteIPBan(ctx context.Context, ip string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM ip_bans WHERE ip = ?`, ip)
	return err
}

func (s *Store) ListActiveIPBans(ctx context.Context) ([]StoredIPBan, error) {
	nowStr := formatTime(time.Now().UTC())
	rows, err := s.reader.QueryContext(ctx, `
		SELECT ip, reason, banned_at, expires_at, permanent
		FROM ip_bans
		WHERE permanent = 1 OR (expires_at IS NOT NULL AND expires_at > ?)
		ORDER BY banned_at DESC
	`, nowStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []StoredIPBan
	for rows.Next() {
		var b StoredIPBan
		var bannedStr string
		var exp sql.NullString
		var perm int
		if err := rows.Scan(&b.IP, &b.Reason, &bannedStr, &exp, &perm); err != nil {
			return nil, err
		}
		b.BannedAt = parseTime(bannedStr)
		b.Permanent = perm != 0
		if exp.Valid && exp.String != "" {
			t := parseTime(exp.String)
			b.ExpiresAt = &t
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *Store) RevokeSession(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND revoked_at IS NULL
	`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) TouchSession(ctx context.Context, id, ip string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_seen_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), ip_address = ?
		WHERE id = ?
	`, ip, id)
	return err
}

func (s *Store) CreateDeviceCode(ctx context.Context, dc DeviceCode) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO device_codes (device_code_hash, user_code, client_name, client_ip, status, expires_at)
		VALUES (?, ?, ?, ?, 'pending', ?)
	`, dc.DeviceCodeHash, dc.UserCode, dc.ClientName, dc.ClientIP, formatTime(dc.ExpiresAt))
	return err
}

func (s *Store) GetDeviceCodeByHash(ctx context.Context, hash string) (DeviceCode, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT device_code_hash, user_code, client_name, client_ip, status, approved_user_id, expires_at, created_at
		FROM device_codes
		WHERE device_code_hash = ?
	`, hash)
	return scanDeviceCode(row)
}

func (s *Store) ApproveDeviceCode(ctx context.Context, userCode, userID string, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE device_codes
		SET status = 'approved', approved_user_id = ?
		WHERE user_code = ? AND status = 'pending' AND expires_at > ?
	`, userID, userCode, formatTime(now))
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ConsumeApprovedDeviceCode atomically transitions an approved device code to consumed.
// consumedNow is true only on the single call that transitions status from 'approved' to 'consumed'.
func (s *Store) ConsumeApprovedDeviceCode(ctx context.Context, hash string, now time.Time) (DeviceCode, bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeviceCode{}, false, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT device_code_hash, user_code, client_name, client_ip, status, approved_user_id, expires_at, created_at
		FROM device_codes
		WHERE device_code_hash = ?
	`, hash)
	dc, err := scanDeviceCode(row)
	if err != nil {
		return DeviceCode{}, false, err
	}

	if dc.Status == "approved" && dc.ExpiresAt.After(now) {
		res, err := tx.ExecContext(ctx, `
			UPDATE device_codes
			SET status = 'consumed'
			WHERE device_code_hash = ? AND status = 'approved'
		`, hash)
		if err != nil {
			return DeviceCode{}, false, err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return dc, false, nil
		}
		if err := tx.Commit(); err != nil {
			return DeviceCode{}, false, err
		}
		dc.Status = "consumed"
		return dc, true, nil
	}
	return dc, false, nil
}

func (s *Store) InsertAuditEvent(ctx context.Context, ev AuditEvent) error {
	var actor sql.NullString
	if ev.ActorID != "" {
		actor = sql.NullString{String: ev.ActorID, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_events (event_type, actor_id, client_ip, detail)
		VALUES (?, ?, ?, ?)
	`, ev.EventType, actor, ev.ClientIP, ev.Detail)
	return err
}

func (s *Store) ListAuditEvents(ctx context.Context, limit int) ([]AuditEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, event_type, actor_id, client_ip, detail, created_at
		FROM audit_events
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEvent
	for rows.Next() {
		var ev AuditEvent
		var actor sql.NullString
		var createdStr string
		if err := rows.Scan(&ev.ID, &ev.EventType, &actor, &ev.ClientIP, &ev.Detail, &createdStr); err != nil {
			return nil, err
		}
		ev.ActorID = actor.String
		ev.CreatedAt = parseTime(createdStr)
		out = append(out, ev)
	}
	return out, rows.Err()
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanSession(row rowScanner) (Session, error) {
	var sess Session
	var refreshHash, refreshExp, revokedAt sql.NullString
	var expStr, lastSeenStr, createdStr string
	if err := row.Scan(
		&sess.ID, &sess.UserID, &sess.TokenHash, &refreshHash,
		&sess.ClientName, &sess.ClientKind, &sess.IPAddress, &sess.UserAgent,
		&expStr, &refreshExp, &lastSeenStr, &revokedAt, &createdStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Session{}, ErrNotFound
		}
		return Session{}, err
	}
	sess.RefreshHash = refreshHash.String
	sess.ExpiresAt = parseTime(expStr)
	sess.LastSeenAt = parseTime(lastSeenStr)
	sess.CreatedAt = parseTime(createdStr)
	if refreshExp.Valid {
		t := parseTime(refreshExp.String)
		sess.RefreshExpiresAt = &t
	}
	if revokedAt.Valid {
		t := parseTime(revokedAt.String)
		sess.RevokedAt = &t
	}
	return sess, nil
}

func scanDeviceCode(row *sql.Row) (DeviceCode, error) {
	var dc DeviceCode
	var approvedUser sql.NullString
	var expStr, createdStr string
	if err := row.Scan(
		&dc.DeviceCodeHash, &dc.UserCode, &dc.ClientName, &dc.ClientIP,
		&dc.Status, &approvedUser, &expStr, &createdStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeviceCode{}, ErrNotFound
		}
		return DeviceCode{}, err
	}
	dc.ApprovedUserID = approvedUser.String
	dc.ExpiresAt = parseTime(expStr)
	dc.CreatedAt = parseTime(createdStr)
	return dc, nil
}

const sqliteTimeFormat = "2006-01-02T15:04:05.000Z"

func formatTime(t time.Time) string {
	return t.UTC().Format(sqliteTimeFormat)
}

func parseTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		t, err = time.Parse(sqliteTimeFormat, raw)
		if err != nil {
			return time.Time{}
		}
	}
	return t.UTC()
}

func init() {
	_ = fmt.Sprintf // keep fmt available if needed
}
