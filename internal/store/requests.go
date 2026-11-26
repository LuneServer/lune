package store

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"
)

const (
	RequestStatusPending    = "pending"
	RequestStatusApproved   = "approved"
	RequestStatusProcessing = "processing"
	RequestStatusAvailable  = "available"
	RequestStatusDeclined   = "declined"
	RequestStatusFailed     = "failed"
)

// MediaRequest represents a user's request for a movie or TV/anime series.
type MediaRequest struct {
	ID              string
	UserID          string
	MediaType       string // "movie" | "tv"
	IsAnime         bool
	TMDBID          int
	TVDBID          int
	Title           string
	OriginalTitle   string
	Year            *int
	Overview        string
	PosterPath      string
	BackdropPath    string
	Status          string // "pending" | "approved" | "processing" | "available" | "declined" | "failed"
	StatusNote      string
	ExternalService string
	ExternalID      int
	ReviewedBy      string
	ReviewedAt      *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// MediaRequestWithUser pairs a MediaRequest with the requesting user's identity.
type MediaRequestWithUser struct {
	MediaRequest
	Username    string
	DisplayName string
}

// LocalMediaMatch describes an existing catalog entry matched by TMDB ID.
type LocalMediaMatch struct {
	ItemID      string
	LibraryID   string
	SeriesTitle string
	Kind        string
}

const mediaRequestColumnsR = `
	r.id, r.user_id, r.media_type, r.is_anime, r.tmdb_id, r.tvdb_id,
	r.title, r.original_title, r.year, r.overview, r.poster_path, r.backdrop_path,
	r.status, r.status_note, r.external_service, r.external_id,
	r.reviewed_by, r.reviewed_at, r.created_at, r.updated_at
`

// CreateMediaRequest inserts a new media request, returning ErrAlreadyExists if an active request
// for the same (media_type, tmdb_id) already exists.
func (s *Store) CreateMediaRequest(ctx context.Context, req MediaRequest) error {
	isAnime := 0
	if req.IsAnime {
		isAnime = 1
	}
	var yearVal sql.NullInt64
	if req.Year != nil && *req.Year > 0 {
		yearVal = sql.NullInt64{Int64: int64(*req.Year), Valid: true}
	}
	var reviewedBy sql.NullString
	if req.ReviewedBy != "" {
		reviewedBy = sql.NullString{String: req.ReviewedBy, Valid: true}
	}
	var reviewedAt sql.NullString
	if req.ReviewedAt != nil {
		reviewedAt = sql.NullString{String: formatTime(*req.ReviewedAt), Valid: true}
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media_requests (
			id, user_id, media_type, is_anime, tmdb_id, tvdb_id,
			title, original_title, year, overview, poster_path, backdrop_path,
			status, status_note, external_service, external_id,
			reviewed_by, reviewed_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, req.ID, req.UserID, req.MediaType, isAnime, req.TMDBID, req.TVDBID,
		req.Title, req.OriginalTitle, yearVal, req.Overview, req.PosterPath, req.BackdropPath,
		req.Status, req.StatusNote, req.ExternalService, req.ExternalID,
		reviewedBy, reviewedAt)
	if err != nil && containsUnique(err.Error()) {
		return ErrAlreadyExists
	}
	return err
}

// GetMediaRequest retrieves a single media request joined with its requesting user.
func (s *Store) GetMediaRequest(ctx context.Context, id string) (MediaRequestWithUser, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT `+mediaRequestColumnsR+`, u.username, u.display_name
		FROM media_requests r
		JOIN users u ON u.id = r.user_id
		WHERE r.id = ?
	`, id)
	return scanMediaRequestWithUser(row)
}

// ListMediaRequests lists media requests optionally filtered by user_id and/or status, ordered newest first.
func (s *Store) ListMediaRequests(ctx context.Context, userIDFilter, statusFilter string, limit int) ([]MediaRequestWithUser, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var where []string
	var args []any
	if userIDFilter != "" {
		where = append(where, "r.user_id = ?")
		args = append(args, userIDFilter)
	}
	if statusFilter != "" {
		where = append(where, "r.status = ?")
		args = append(args, statusFilter)
	}
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+mediaRequestColumnsR+`, u.username, u.display_name
		FROM media_requests r
		JOIN users u ON u.id = r.user_id
		`+whereSQL+`
		ORDER BY r.created_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]MediaRequestWithUser, 0)
	for rows.Next() {
		item, err := scanMediaRequestWithUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// CountPendingRequests returns the total number of requests currently in 'pending' status.
func (s *Store) CountPendingRequests(ctx context.Context) (int, error) {
	var n int
	if err := s.reader.QueryRowContext(ctx, `SELECT COUNT(1) FROM media_requests WHERE status = 'pending'`).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// MediaRequestStatusUpdate specifies fields to update on a media request.
type MediaRequestStatusUpdate struct {
	Status          string
	StatusNote      string
	ExternalService string
	ExternalID      int
	ReviewedBy      string
	ReviewedAt      *time.Time
	IsAnime         *bool
}

// UpdateMediaRequestStatus updates the status, review metadata, external service linkage,
// and optional anime classification of a media request.
func (s *Store) UpdateMediaRequestStatus(ctx context.Context, id string, upd MediaRequestStatusUpdate) error {
	sets := []string{
		"status = ?",
		"status_note = ?",
		"updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')",
	}
	args := []any{upd.Status, upd.StatusNote}

	if upd.ExternalService != "" {
		sets = append(sets, "external_service = ?")
		args = append(args, upd.ExternalService)
	}
	if upd.ExternalID > 0 {
		sets = append(sets, "external_id = ?")
		args = append(args, upd.ExternalID)
	}
	if upd.ReviewedBy != "" {
		sets = append(sets, "reviewed_by = ?")
		args = append(args, upd.ReviewedBy)
	}
	if upd.ReviewedAt != nil {
		sets = append(sets, "reviewed_at = ?")
		args = append(args, formatTime(*upd.ReviewedAt))
	}
	if upd.IsAnime != nil {
		v := 0
		if *upd.IsAnime {
			v = 1
		}
		sets = append(sets, "is_anime = ?")
		args = append(args, v)
	}

	args = append(args, id)
	res, err := s.db.ExecContext(ctx, "UPDATE media_requests SET "+strings.Join(sets, ", ")+" WHERE id = ?", args...)
	if err != nil {
		if containsUnique(err.Error()) {
			return ErrAlreadyExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteMediaRequest permanently removes a media request by ID.
func (s *Store) DeleteMediaRequest(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM media_requests WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// FindActiveRequestsByTMDB returns active ('pending', 'approved', 'processing') requests keyed by tmdb_id
// for the given mediaType ('movie' or 'tv').
func (s *Store) FindActiveRequestsByTMDB(ctx context.Context, mediaType string, tmdbIDs []int) (map[int]MediaRequest, error) {
	out := make(map[int]MediaRequest)
	if len(tmdbIDs) == 0 {
		return out, nil
	}
	placeholders := make([]string, len(tmdbIDs))
	args := make([]any, 0, len(tmdbIDs)+1)
	args = append(args, mediaType)
	for i, id := range tmdbIDs {
		placeholders[i] = "?"
		args = append(args, id)
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+mediaRequestColumnsR+`
		FROM media_requests r
		WHERE r.media_type = ?
		  AND r.status IN ('pending','approved','processing')
		  AND r.tmdb_id IN (`+strings.Join(placeholders, ",")+`)
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		req, err := scanMediaRequest(rows)
		if err != nil {
			return nil, err
		}
		out[req.TMDBID] = req
	}
	return out, rows.Err()
}

// FindLocalMediaByTMDB returns existing local catalog items matching any of tmdbIDs,
// scoped to the user's allowed libraries when restrictLibraries is true.
func (s *Store) FindLocalMediaByTMDB(ctx context.Context, tmdbIDs []int, allowedLibraryIDs []string, restrictLibraries bool) (map[int]LocalMediaMatch, error) {
	return s.FindLocalMediaByTMDBAndType(ctx, "", tmdbIDs, allowedLibraryIDs, restrictLibraries)
}

// FindLocalMediaByTMDBAndType returns existing local catalog items matching tmdbIDs and an optional
// mediaType ('movie' -> kind='movie', 'tv' -> kind='episode', '' -> any).
func (s *Store) FindLocalMediaByTMDBAndType(ctx context.Context, mediaType string, tmdbIDs []int, allowedLibraryIDs []string, restrictLibraries bool) (map[int]LocalMediaMatch, error) {
	out := make(map[int]LocalMediaMatch)
	if len(tmdbIDs) == 0 {
		return out, nil
	}
	if restrictLibraries && len(allowedLibraryIDs) == 0 {
		return out, nil
	}

	idPlaceholders := make([]string, len(tmdbIDs))
	args := make([]any, 0, len(tmdbIDs)+len(allowedLibraryIDs)+1)
	for i, id := range tmdbIDs {
		idPlaceholders[i] = "?"
		args = append(args, strconv.Itoa(id))
	}

	where := []string{"tmdb_id IN (" + strings.Join(idPlaceholders, ",") + ")"}
	switch mediaType {
	case "movie":
		where = append(where, "kind = 'movie'")
	case "tv":
		where = append(where, "kind = 'episode'")
	}
	if restrictLibraries {
		libPlaceholders := make([]string, len(allowedLibraryIDs))
		for i, lid := range allowedLibraryIDs {
			libPlaceholders[i] = "?"
			args = append(args, lid)
		}
		where = append(where, "library_id IN ("+strings.Join(libPlaceholders, ",")+")")
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT tmdb_id, id, library_id, series_title, kind
		FROM media_items
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var tmdbStr string
		var m LocalMediaMatch
		if err := rows.Scan(&tmdbStr, &m.ItemID, &m.LibraryID, &m.SeriesTitle, &m.Kind); err != nil {
			return nil, err
		}
		id, err := strconv.Atoi(strings.TrimSpace(tmdbStr))
		if err != nil || id <= 0 {
			continue
		}
		if _, exists := out[id]; !exists {
			out[id] = m
		}
	}
	return out, rows.Err()
}

// MarkRequestsAvailableByTMDB transitions any active ('pending', 'approved', 'processing')
// media requests matching tmdbID (and optional mediaType) to 'available'.
func (s *Store) MarkRequestsAvailableByTMDB(ctx context.Context, tmdbID int, mediaType ...string) (int64, error) {
	if tmdbID <= 0 {
		return 0, nil
	}
	var (
		res sql.Result
		err error
	)
	if len(mediaType) > 0 && mediaType[0] != "" {
		res, err = s.db.ExecContext(ctx, `
			UPDATE media_requests
			SET status = 'available',
			    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE tmdb_id = ?
			  AND media_type = ?
			  AND status IN ('pending','approved','processing')
		`, tmdbID, mediaType[0])
	} else {
		res, err = s.db.ExecContext(ctx, `
			UPDATE media_requests
			SET status = 'available',
			    updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
			WHERE tmdb_id = ?
			  AND status IN ('pending','approved','processing')
		`, tmdbID)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func scanMediaRequest(row rowScanner) (MediaRequest, error) {
	var r MediaRequest
	var isAnime int
	var year sql.NullInt64
	var reviewedBy, reviewedAt sql.NullString
	var createdStr, updatedStr string
	if err := row.Scan(
		&r.ID, &r.UserID, &r.MediaType, &isAnime, &r.TMDBID, &r.TVDBID,
		&r.Title, &r.OriginalTitle, &year, &r.Overview, &r.PosterPath, &r.BackdropPath,
		&r.Status, &r.StatusNote, &r.ExternalService, &r.ExternalID,
		&reviewedBy, &reviewedAt, &createdStr, &updatedStr,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MediaRequest{}, ErrNotFound
		}
		return MediaRequest{}, err
	}
	r.IsAnime = isAnime != 0
	if year.Valid && year.Int64 > 0 {
		y := int(year.Int64)
		r.Year = &y
	}
	r.ReviewedBy = reviewedBy.String
	if reviewedAt.Valid && reviewedAt.String != "" {
		t := parseTime(reviewedAt.String)
		r.ReviewedAt = &t
	}
	r.CreatedAt = parseTime(createdStr)
	r.UpdatedAt = parseTime(updatedStr)
	return r, nil
}

func scanMediaRequestWithUser(row rowScanner) (MediaRequestWithUser, error) {
	var r MediaRequestWithUser
	var isAnime int
	var year sql.NullInt64
	var reviewedBy, reviewedAt sql.NullString
	var createdStr, updatedStr string
	if err := row.Scan(
		&r.ID, &r.UserID, &r.MediaType, &isAnime, &r.TMDBID, &r.TVDBID,
		&r.Title, &r.OriginalTitle, &year, &r.Overview, &r.PosterPath, &r.BackdropPath,
		&r.Status, &r.StatusNote, &r.ExternalService, &r.ExternalID,
		&reviewedBy, &reviewedAt, &createdStr, &updatedStr,
		&r.Username, &r.DisplayName,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MediaRequestWithUser{}, ErrNotFound
		}
		return MediaRequestWithUser{}, err
	}
	r.IsAnime = isAnime != 0
	if year.Valid && year.Int64 > 0 {
		y := int(year.Int64)
		r.Year = &y
	}
	r.ReviewedBy = reviewedBy.String
	if reviewedAt.Valid && reviewedAt.String != "" {
		t := parseTime(reviewedAt.String)
		r.ReviewedAt = &t
	}
	r.CreatedAt = parseTime(createdStr)
	r.UpdatedAt = parseTime(updatedStr)
	return r, nil
}
