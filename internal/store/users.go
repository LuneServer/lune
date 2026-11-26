package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	MarkKindWatchlist = "watchlist"
	MarkKindFavorite  = "favorite"
)

// UserPreferences holds per-user media playback & UI preferences.
type UserPreferences struct {
	AudioLanguage    string   `json:"audio_language,omitempty"`
	SubtitleLanguage string   `json:"subtitle_language,omitempty"`
	SubtitlesEnabled bool     `json:"subtitles_enabled"`
	AutoplayNext     bool     `json:"autoplay_next"`
	CombineNextUp    bool     `json:"combine_next_up" required:"false"`
	PosterBadges     []string `json:"poster_badges,omitempty" required:"false"`
	MaxBitrate       int64    `json:"max_bitrate,omitempty"`
}

// DefaultUserPreferences returns sensible defaults for newly created accounts.
func DefaultUserPreferences() UserPreferences {
	return UserPreferences{
		AutoplayNext:  true,
		CombineNextUp: true,
	}
}

// ParseUserPreferences decodes preferences JSON onto default values.
func ParseUserPreferences(raw string) UserPreferences {
	prefs := DefaultUserPreferences()
	if raw != "" && raw != "{}" {
		_ = json.Unmarshal([]byte(raw), &prefs)
	}
	return prefs
}

// Preferences returns the decoded preferences for the user.
func (u User) Preferences() UserPreferences {
	return ParseUserPreferences(u.PreferencesJSON)
}

// UserInvite represents an admin-generated account invitation token.
type UserInvite struct {
	ID           string
	TokenHash    string
	TokenPrefix  string
	Label        string
	Role         string
	AllLibraries bool
	RequireTOTP  bool
	LibraryIDs   []string
	MaxUses      int
	UseCount     int
	CreatedBy    string
	ExpiresAt    time.Time
	RevokedAt    *time.Time
	CreatedAt    time.Time
}

// AdminUserUpdate specifies optional fields for an atomic admin update.
type AdminUserUpdate struct {
	DisplayName         *string
	Role                *string
	Disabled            *bool
	PasswordHash        *string
	AllLibraries        *bool
	LibraryIDs          *[]string
	CanRequest          *bool
	AutoApproveRequests *bool
	TOTPRequired        *bool
}

// GetSetting retrieves a value from server_settings, returning ErrNotFound if absent.
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var val string
	err := s.reader.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&val)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return val, err
}

// SetSetting upserts a key/value entry in server_settings.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO server_settings (key, value, updated_at)
		VALUES (?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(key) DO UPDATE SET
			value      = excluded.value,
			updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
	`, key, value)
	return err
}

// TouchUserLastLogin records the user's most recent successful login timestamp.
func (s *Store) TouchUserLastLogin(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET last_login_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, userID)
	return err
}

// CreateUserWithLibraries atomically creates a user and assigns library permissions.
func (s *Store) CreateUserWithLibraries(ctx context.Context, u User, libraryIDs []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	allLibs := 1
	if u.Role != "admin" && !u.AllLibraries {
		allLibs = 0
	}
	totpReq := 0
	if u.TOTPRequired {
		totpReq = 1
	}
	prefsJSON := u.PreferencesJSON
	if prefsJSON == "" {
		prefsJSON = "{}"
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, all_libraries, totp_required, preferences_json)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?)
	`, u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, allLibs, totpReq, prefsJSON)
	if err != nil {
		if containsUnique(err.Error()) {
			return ErrAlreadyExists
		}
		return err
	}

	if allLibs == 0 && len(libraryIDs) > 0 {
		for _, libID := range libraryIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO user_library_access (user_id, library_id)
				VALUES (?, ?)
			`, u.ID, libID); err != nil {
				return err
			}
		}
	}

	return tx.Commit()
}

// UpdateUserProfile updates a user's display name and preferences JSON.
func (s *Store) UpdateUserProfile(ctx context.Context, userID, displayName, preferencesJSON string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET display_name = ?, preferences_json = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, displayName, preferencesJSON, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserAvatar sets or clears the stored avatar file path for a user.
func (s *Store) UpdateUserAvatar(ctx context.Context, userID, avatarPath string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE users
		SET avatar_path = ?, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ?
	`, avatarPath, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// UpdateUserPasswordKeepSession updates a user's password hash and revokes all active
// sessions except keepSessionID (if empty, all sessions are revoked).
func (s *Store) UpdateUserPasswordKeepSession(ctx context.Context, userID, passwordHash, keepSessionID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
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

	if keepSessionID != "" {
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE user_id = ? AND id != ? AND revoked_at IS NULL
		`, userID, keepSessionID)
	} else {
		_, err = tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE user_id = ? AND revoked_at IS NULL
		`, userID)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// AdminUpdateUser atomically applies admin user edits, library access changes, and session revocation.
func (s *Store) AdminUpdateUser(ctx context.Context, userID string, upd AdminUserUpdate) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var sets []string
	var args []any

	if upd.DisplayName != nil {
		sets = append(sets, "display_name = ?")
		args = append(args, *upd.DisplayName)
	}
	if upd.Role != nil {
		sets = append(sets, "role = ?")
		args = append(args, *upd.Role)
	}
	if upd.Disabled != nil {
		v := 0
		if *upd.Disabled {
			v = 1
		}
		sets = append(sets, "disabled = ?")
		args = append(args, v)
	}
	if upd.PasswordHash != nil {
		sets = append(sets, "password_hash = ?")
		args = append(args, *upd.PasswordHash)
	}
	if upd.AllLibraries != nil {
		v := 0
		if *upd.AllLibraries {
			v = 1
		}
		sets = append(sets, "all_libraries = ?")
		args = append(args, v)
	}
	if upd.CanRequest != nil {
		v := 0
		if *upd.CanRequest {
			v = 1
		}
		sets = append(sets, "can_request = ?")
		args = append(args, v)
	}
	if upd.AutoApproveRequests != nil {
		v := 0
		if *upd.AutoApproveRequests {
			v = 1
		}
		sets = append(sets, "auto_approve_requests = ?")
		args = append(args, v)
	}
	if upd.TOTPRequired != nil {
		v := 0
		if *upd.TOTPRequired {
			v = 1
		}
		sets = append(sets, "totp_required = ?")
		args = append(args, v)
	}

	if len(sets) > 0 {
		sets = append(sets, "updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')")
		args = append(args, userID)
		res, err := tx.ExecContext(ctx, "UPDATE users SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
	}

	if upd.AllLibraries != nil && *upd.AllLibraries {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_library_access WHERE user_id = ?`, userID); err != nil {
			return err
		}
	} else if upd.LibraryIDs != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM user_library_access WHERE user_id = ?`, userID); err != nil {
			return err
		}
		for _, libID := range *upd.LibraryIDs {
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO user_library_access (user_id, library_id)
				VALUES (?, ?)
			`, userID, libID); err != nil {
				return err
			}
		}
	}

	revokeSessions := (upd.Disabled != nil && *upd.Disabled) || upd.PasswordHash != nil
	if revokeSessions {
		if _, err := tx.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE user_id = ? AND revoked_at IS NULL
		`, userID); err != nil {
			return err
		}
	}

	return tx.Commit()
}

// RevokeUserSessions revokes all active sessions for userID, optionally preserving keepSessionID.
func (s *Store) RevokeUserSessions(ctx context.Context, userID, keepSessionID string) (int64, error) {
	var (
		res sql.Result
		err error
	)
	if keepSessionID != "" {
		res, err = s.db.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE user_id = ? AND id != ? AND revoked_at IS NULL
		`, userID, keepSessionID)
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE sessions
			SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE user_id = ? AND revoked_at IS NULL
		`, userID)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// CountActiveSessionsByUser returns the number of non-revoked, non-expired sessions per user.
func (s *Store) CountActiveSessionsByUser(ctx context.Context, now time.Time) (map[string]int, error) {
	nowStr := formatTime(now)
	rows, err := s.reader.QueryContext(ctx, `
		SELECT user_id, COUNT(1)
		FROM sessions
		WHERE revoked_at IS NULL
		  AND (expires_at > ? OR (refresh_expires_at IS NOT NULL AND refresh_expires_at > ?))
		GROUP BY user_id
	`, nowStr, nowStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var uid string
		var cnt int
		if err := rows.Scan(&uid, &cnt); err != nil {
			return nil, err
		}
		out[uid] = cnt
	}
	return out, rows.Err()
}

// CountActiveSessionsForUser returns active session count for one user.
func (s *Store) CountActiveSessionsForUser(ctx context.Context, userID string, now time.Time) (int, error) {
	nowStr := formatTime(now)
	var cnt int
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM sessions
		WHERE user_id = ?
		  AND revoked_at IS NULL
		  AND (expires_at > ? OR (refresh_expires_at IS NOT NULL AND refresh_expires_at > ?))
	`, userID, nowStr, nowStr).Scan(&cnt); err != nil {
		return 0, err
	}
	return cnt, nil
}

// GetUserLibraryIDs returns explicitly granted library IDs for a user.
func (s *Store) GetUserLibraryIDs(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT library_id
		FROM user_library_access
		WHERE user_id = ?
		ORDER BY library_id ASC
	`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]string, 0)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetAllUserLibraryIDs batch-loads explicit library grants keyed by user_id.
func (s *Store) GetAllUserLibraryIDs(ctx context.Context) (map[string][]string, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT user_id, library_id
		FROM user_library_access
		ORDER BY user_id ASC, library_id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string][]string)
	for rows.Next() {
		var uid, lid string
		if err := rows.Scan(&uid, &lid); err != nil {
			return nil, err
		}
		out[uid] = append(out[uid], lid)
	}
	return out, rows.Err()
}

// ExistingLibraryIDs filters the provided library IDs down to those present in the libraries table.
func (s *Store) ExistingLibraryIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	found := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return found, nil
	}
	placeholders := make([]string, len(ids))
	args := make([]any, len(ids))
	for i, id := range ids {
		placeholders[i] = "?"
		args[i] = id
	}
	rows, err := s.reader.QueryContext(ctx, "SELECT id FROM libraries WHERE id IN ("+strings.Join(placeholders, ",")+")", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		found[id] = true
	}
	return found, rows.Err()
}

// CanUserAccessLibrary checks whether user is permitted to access libraryID.
func (s *Store) CanUserAccessLibrary(ctx context.Context, user User, libraryID string) (bool, error) {
	if user.HasUnrestrictedLibraryAccess() {
		return true, nil
	}
	var exists int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM user_library_access WHERE user_id = ? AND library_id = ?
	`, user.ID, libraryID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AccessibleLibraryIDs returns (ids, unrestricted, err) for scoping queries to a user's libraries.
func (s *Store) AccessibleLibraryIDs(ctx context.Context, user User) ([]string, bool, error) {
	if user.HasUnrestrictedLibraryAccess() {
		return nil, true, nil
	}
	ids, err := s.GetUserLibraryIDs(ctx, user.ID)
	if err != nil {
		return nil, false, err
	}
	return ids, false, nil
}

// CreateUserInvite stores a hashed invitation token.
func (s *Store) CreateUserInvite(ctx context.Context, inv UserInvite) error {
	allLibs := 1
	if inv.Role != "admin" && !inv.AllLibraries {
		allLibs = 0
	}
	libJSON := "[]"
	if allLibs == 0 && len(inv.LibraryIDs) > 0 {
		if b, err := json.Marshal(inv.LibraryIDs); err == nil {
			libJSON = string(b)
		}
	}
	var createdBy sql.NullString
	reqTOTP := 0
	if inv.RequireTOTP {
		reqTOTP = 1
	}
	if inv.CreatedBy != "" {
		createdBy = sql.NullString{String: inv.CreatedBy, Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_invites (
			id, token_hash, token_prefix, label, role, all_libraries, require_totp,
			library_ids_json, max_uses, use_count, created_by, expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?)
	`, inv.ID, inv.TokenHash, inv.TokenPrefix, inv.Label, inv.Role, allLibs, reqTOTP,
		libJSON, inv.MaxUses, createdBy, formatTime(inv.ExpiresAt))
	return err
}

// GetUserInviteByTokenHash fetches an invite by its SHA-256 token hash.
func (s *Store) GetUserInviteByTokenHash(ctx context.Context, tokenHash string) (UserInvite, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, token_hash, token_prefix, label, role, all_libraries, require_totp,
		       library_ids_json, max_uses, use_count, created_by,
		       expires_at, revoked_at, created_at
		FROM user_invites
		WHERE token_hash = ?
	`, tokenHash)
	return scanUserInvite(row)
}

// GetUserInviteByID fetches an invite by ID.
func (s *Store) GetUserInviteByID(ctx context.Context, id string) (UserInvite, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT id, token_hash, token_prefix, label, role, all_libraries, require_totp,
		       library_ids_json, max_uses, use_count, created_by,
		       expires_at, revoked_at, created_at
		FROM user_invites
		WHERE id = ?
	`, id)
	return scanUserInvite(row)
}

// ListUserInvites returns all invites ordered newest first.
func (s *Store) ListUserInvites(ctx context.Context) ([]UserInvite, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, token_hash, token_prefix, label, role, all_libraries, require_totp,
		       library_ids_json, max_uses, use_count, created_by,
		       expires_at, revoked_at, created_at
		FROM user_invites
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UserInvite, 0)
	for rows.Next() {
		inv, err := scanUserInvite(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
	}
	return out, rows.Err()
}

// RevokeUserInvite marks an active invite as revoked.
func (s *Store) RevokeUserInvite(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_invites
		SET revoked_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		WHERE id = ? AND revoked_at IS NULL
	`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteInactiveUserInvites removes revoked, expired, and exhausted invites while preserving active ones.
func (s *Store) DeleteInactiveUserInvites(ctx context.Context, now time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM user_invites
		WHERE revoked_at IS NOT NULL
		   OR use_count >= max_uses
		   OR expires_at <= ?
	`, now.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AcceptUserInvite atomically validates an invite, creates the user + library access,
// and increments the invite's use_count.
func (s *Store) AcceptUserInvite(ctx context.Context, tokenHash string, u User, now time.Time) (User, UserInvite, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, UserInvite{}, err
	}
	defer func() { _ = tx.Rollback() }()

	row := tx.QueryRowContext(ctx, `
		SELECT id, token_hash, token_prefix, label, role, all_libraries, require_totp,
		       library_ids_json, max_uses, use_count, created_by,
		       expires_at, revoked_at, created_at
		FROM user_invites
		WHERE token_hash = ?
	`, tokenHash)
	inv, err := scanUserInvite(row)
	if err != nil {
		return User{}, UserInvite{}, err
	}
	if inv.RevokedAt != nil {
		return User{}, UserInvite{}, ErrInviteRevoked
	}
	if !inv.ExpiresAt.After(now) {
		return User{}, UserInvite{}, ErrInviteExpired
	}
	if inv.UseCount >= inv.MaxUses {
		return User{}, UserInvite{}, ErrInviteExhausted
	}

	u.Role = inv.Role
	u.AllLibraries = inv.Role == "admin" || inv.AllLibraries
	u.TOTPRequired = inv.RequireTOTP
	allLibs := 0
	if u.AllLibraries {
		allLibs = 1
	}
	totpReq := 0
	if u.TOTPRequired {
		totpReq = 1
	}
	prefsJSON := u.PreferencesJSON
	if prefsJSON == "" {
		prefsJSON = "{}"
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO users (id, username, display_name, password_hash, role, disabled, all_libraries, totp_required, preferences_json, last_login_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
	`, u.ID, u.Username, u.DisplayName, u.PasswordHash, u.Role, allLibs, totpReq, prefsJSON)
	if err != nil {
		if containsUnique(err.Error()) {
			return User{}, UserInvite{}, ErrAlreadyExists
		}
		return User{}, UserInvite{}, err
	}

	if allLibs == 0 && len(inv.LibraryIDs) > 0 {
		for _, libID := range inv.LibraryIDs {
			// Guard against libraries deleted after invite creation.
			if _, err := tx.ExecContext(ctx, `
				INSERT OR IGNORE INTO user_library_access (user_id, library_id)
				SELECT ?, id FROM libraries WHERE id = ?
			`, u.ID, libID); err != nil {
				return User{}, UserInvite{}, err
			}
		}
	}

	res, err := tx.ExecContext(ctx, `
		UPDATE user_invites
		SET use_count = use_count + 1
		WHERE id = ? AND revoked_at IS NULL AND use_count < max_uses
	`, inv.ID)
	if err != nil {
		return User{}, UserInvite{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return User{}, UserInvite{}, ErrInviteExhausted
	}

	if err := tx.Commit(); err != nil {
		return User{}, UserInvite{}, err
	}

	inv.UseCount++
	created, err := s.GetUserByID(ctx, u.ID)
	if err != nil {
		return User{}, UserInvite{}, err
	}
	return created, inv, nil
}

func scanUserInvite(row rowScanner) (UserInvite, error) {
	var inv UserInvite
	var allLibs, reqTOTP int
	var libJSON string
	var createdBy, revokedAt sql.NullString
	var expiresStr, createdStr string
	if err := row.Scan(
		&inv.ID, &inv.TokenHash, &inv.TokenPrefix, &inv.Label, &inv.Role, &allLibs, &reqTOTP,
		&libJSON, &inv.MaxUses, &inv.UseCount, &createdBy,
		&expiresStr, &revokedAt, &createdStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return UserInvite{}, ErrNotFound
		}
		return UserInvite{}, err
	}
	inv.AllLibraries = allLibs != 0
	inv.RequireTOTP = reqTOTP != 0
	inv.CreatedBy = createdBy.String
	inv.ExpiresAt = parseTime(expiresStr)
	inv.CreatedAt = parseTime(createdStr)
	if revokedAt.Valid && revokedAt.String != "" {
		t := parseTime(revokedAt.String)
		inv.RevokedAt = &t
	}
	inv.LibraryIDs = []string{}
	if libJSON != "" && libJSON != "[]" {
		_ = json.Unmarshal([]byte(libJSON), &inv.LibraryIDs)
	}
	return inv, nil
}

// AddUserMediaMark records a watchlist or favorite entry for a user and media item.
func (s *Store) AddUserMediaMark(ctx context.Context, userID, itemID, kind string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT OR IGNORE INTO user_media_marks (user_id, item_id, kind)
		VALUES (?, ?, ?)
	`, userID, itemID, kind)
	return err
}

// RemoveUserMediaMark deletes a watchlist or favorite entry for a user and media item.
func (s *Store) RemoveUserMediaMark(ctx context.Context, userID, itemID, kind string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM user_media_marks
		WHERE user_id = ? AND item_id = ? AND kind = ?
	`, userID, itemID, kind)
	return err
}

// GetUserMediaMarksForItem reports whether itemID is in the user's watchlist and/or favorites.
func (s *Store) GetUserMediaMarksForItem(ctx context.Context, userID, itemID string) (inWatchlist bool, isFavorite bool, err error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT kind FROM user_media_marks WHERE user_id = ? AND item_id = ?
	`, userID, itemID)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()

	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			return false, false, err
		}
		switch kind {
		case MarkKindWatchlist:
			inWatchlist = true
		case MarkKindFavorite:
			isFavorite = true
		}
	}
	return inWatchlist, isFavorite, rows.Err()
}

// ListUserMediaMarks returns a user's watchlist or favorite items scoped to accessible libraries.
func (s *Store) ListUserMediaMarks(ctx context.Context, userID, kind string, allowedLibraryIDs []string, restrictLibraries bool, limit, offset int) ([]MediaItem, int, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	if restrictLibraries && len(allowedLibraryIDs) == 0 {
		return []MediaItem{}, 0, nil
	}

	where := []string{"um.user_id = ?", "um.kind = ?"}
	args := []any{userID, kind}
	if restrictLibraries {
		placeholders := make([]string, len(allowedLibraryIDs))
		for i, id := range allowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "m.library_id IN ("+strings.Join(placeholders, ",")+")")
	}
	whereSQL := "WHERE " + strings.Join(where, " AND ")

	var total int
	if err := s.reader.QueryRowContext(ctx, `
		SELECT COUNT(1)
		FROM user_media_marks um
		JOIN media_items m ON m.id = um.item_id
		`+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	querySQL := `
		SELECT ` + mediaItemColumnsM + `
		FROM user_media_marks um
		JOIN media_items m ON m.id = um.item_id
		` + whereSQL + `
		ORDER BY um.created_at DESC, m.sort_title COLLATE NOCASE ASC
		LIMIT ? OFFSET ?
	`
	queryArgs := append(append([]any{}, args...), limit, offset)
	rows, err := s.reader.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	items := make([]MediaItem, 0)
	for rows.Next() {
		item, err := scanMediaItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}
