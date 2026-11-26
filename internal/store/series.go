package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type SeriesSummary struct {
	LibraryID            string
	LibraryKind          string
	Title                string
	OriginalTitle        string
	Year                 *int
	Rating               float64
	ContentRating        string
	GenresJSON           string
	Overview             string
	VideoHeight          int
	VideoHDR             string
	SeasonCount          int
	EpisodeCount         int
	RepresentativeItemID string
	HasPoster            bool
	HasBackdrop          bool
	DualAudio            bool
	LatestAddedAt        time.Time
}

type EpisodeWithProgress struct {
	Item     MediaItem
	Progress *PlaybackProgress
}

type SeriesListFilter struct {
	LibraryID         string
	AllowedLibraryIDs []string
	RestrictLibraries bool
	UserID            string
	Query             string
	Genre             string
	Person            string
	Studio            string
	ContentRating     string
	MinRating         float64
	Year              int
	Decade            int
	Resolution        string // "4k" | "1080p" | "720p"
	HDR               bool
	DualAudio         bool
	Watched           string // "" | "watched" | "unwatched"
	Sort              string
}

// ListSeries aggregates episode items into grouped TV/Anime series cards, optionally restricted to allowed libraries.
func (s *Store) ListSeries(ctx context.Context, libraryID, query, sortBy string, allowedLibraryIDs []string, restrictLibraries bool) ([]SeriesSummary, error) {
	return s.ListSeriesFiltered(ctx, SeriesListFilter{
		LibraryID:         libraryID,
		AllowedLibraryIDs: allowedLibraryIDs,
		RestrictLibraries: restrictLibraries,
		Query:             query,
		Sort:              sortBy,
	})
}

// ListSeriesFiltered aggregates episode items into grouped TV/Anime series cards with optional metadata filters.
func (s *Store) ListSeriesFiltered(ctx context.Context, f SeriesListFilter) ([]SeriesSummary, error) {
	if f.RestrictLibraries && len(f.AllowedLibraryIDs) == 0 {
		return []SeriesSummary{}, nil
	}

	var where []string
	var args []any

	where = append(where, "m.kind = 'episode'", "m.series_title != ''")
	if f.LibraryID != "" {
		where = append(where, "m.library_id = ?")
		args = append(args, f.LibraryID)
	}
	if f.RestrictLibraries {
		placeholders := make([]string, len(f.AllowedLibraryIDs))
		for i, id := range f.AllowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "m.library_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if ftsQ := buildFTSQuery(q); ftsQ != "" {
			where = append(where, "m.rowid IN (SELECT rowid FROM media_items_fts WHERE media_items_fts MATCH ?)")
			args = append(args, ftsQ)
		} else {
			like := "%" + q + "%"
			where = append(where, "(m.series_title LIKE ? COLLATE NOCASE OR m.original_title LIKE ? COLLATE NOCASE)")
			args = append(args, like, like)
		}
	}
	if genre := strings.TrimSpace(f.Genre); genre != "" {
		where = append(where, `m.genres_json LIKE ? COLLATE NOCASE`)
		args = append(args, `%"`+escapeJSONLike(genre)+`"%`)
	}
	if studio := strings.TrimSpace(f.Studio); studio != "" {
		where = append(where, `m.studios_json LIKE ? COLLATE NOCASE`)
		args = append(args, `%"`+escapeJSONLike(studio)+`"%`)
	}
	if person := strings.TrimSpace(f.Person); person != "" {
		pat := `%"` + escapeJSONLike(person) + `"%`
		where = append(where, `(m.cast_json LIKE ? COLLATE NOCASE OR m.directors_json LIKE ? COLLATE NOCASE OR m.writers_json LIKE ? COLLATE NOCASE)`)
		args = append(args, pat, pat, pat)
	}
	if cr := strings.TrimSpace(f.ContentRating); cr != "" {
		where = append(where, "m.content_rating = ? COLLATE NOCASE")
		args = append(args, cr)
	}
	if f.Year > 0 {
		where = append(where, "m.year = ?")
		args = append(args, f.Year)
	} else if f.Decade > 0 {
		where = append(where, "m.year >= ? AND m.year <= ?")
		args = append(args, f.Decade, f.Decade+9)
	}

	var having []string
	var havingArgs []any
	if f.MinRating > 0 {
		having = append(having, "MAX(m.rating) >= ?")
		havingArgs = append(havingArgs, f.MinRating)
	}
	switch strings.ToLower(strings.TrimSpace(f.Resolution)) {
	case "4k":
		having = append(having, "MAX(m.video_height) >= 2000")
	case "1080p":
		having = append(having, "MAX(m.video_height) >= 1000 AND MAX(m.video_height) < 2000")
	case "720p":
		having = append(having, "MAX(m.video_height) >= 700 AND MAX(m.video_height) < 1000")
	}
	if f.HDR {
		having = append(having, "MAX(CASE WHEN m.video_hdr != '' AND lower(m.video_hdr) != 'sdr' THEN 1 ELSE 0 END) = 1")
	}
	if f.DualAudio {
		having = append(having, "MAX(CASE WHEN instr(m.audio_languages, ',') > 0 THEN 1 ELSE 0 END) = 1")
	}

	joinProgress := ""
	var joinArgs []any
	if f.UserID != "" && (strings.EqualFold(f.Watched, "watched") || strings.EqualFold(f.Watched, "unwatched")) {
		joinProgress = "LEFT JOIN playback_progress p ON p.item_id = m.id AND p.user_id = ?"
		joinArgs = append(joinArgs, f.UserID)
		if strings.EqualFold(f.Watched, "watched") {
			having = append(having, "SUM(CASE WHEN p.completed = 1 THEN 1 ELSE 0 END) = COUNT(1)")
		} else {
			having = append(having, "SUM(CASE WHEN p.completed = 1 THEN 1 ELSE 0 END) < COUNT(1)")
		}
	}

	havingSQL := ""
	if len(having) > 0 {
		havingSQL = "HAVING " + strings.Join(having, " AND ")
	}

	orderSQL := "ORDER BY m.series_title COLLATE NOCASE ASC"
	switch f.Sort {
	case "recent":
		orderSQL = "ORDER BY latest_added DESC, m.series_title COLLATE NOCASE ASC"
	case "year":
		orderSQL = "ORDER BY COALESCE(min_year, 0) DESC, m.series_title COLLATE NOCASE ASC"
	case "rating":
		orderSQL = "ORDER BY max_rating DESC, m.series_title COLLATE NOCASE ASC"
	}

	sqlStr := `
		SELECT
			m.library_id,
			l.kind,
			m.series_title,
			MAX(m.original_title) AS original_title,
			MIN(m.year) AS min_year,
			MAX(m.rating) AS max_rating,
			MAX(m.content_rating) AS content_rating,
			MAX(m.genres_json) AS genres_json,
			MAX(m.overview) AS overview,
			MAX(m.video_height) AS max_height,
			MAX(CASE WHEN m.video_hdr != '' AND lower(m.video_hdr) != 'sdr' THEN m.video_hdr ELSE '' END) AS video_hdr,
			COUNT(DISTINCT COALESCE(m.season_number, 1)) AS season_count,
			COUNT(1) AS episode_count,
			MIN(m.id) AS rep_id,
			MAX(CASE WHEN m.poster_path != '' THEN 1 ELSE 0 END) AS has_poster,
			MAX(CASE WHEN m.backdrop_path != '' THEN 1 ELSE 0 END) AS has_backdrop,
			MAX(CASE WHEN instr(m.audio_languages, ',') > 0 THEN 1 ELSE 0 END) AS dual_audio,
			MAX(m.created_at) AS latest_added
		FROM media_items m
		JOIN libraries l ON l.id = m.library_id
		` + joinProgress + `
		WHERE ` + strings.Join(where, " AND ") + `
		GROUP BY m.library_id, m.series_title COLLATE NOCASE
		` + havingSQL + `
		` + orderSQL

	queryArgs := make([]any, 0, len(joinArgs)+len(args)+len(havingArgs))
	queryArgs = append(queryArgs, joinArgs...)
	queryArgs = append(queryArgs, args...)
	queryArgs = append(queryArgs, havingArgs...)

	rows, err := s.reader.QueryContext(ctx, sqlStr, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SeriesSummary
	for rows.Next() {
		var ss SeriesSummary
		var minYear sql.NullInt64
		var hasPoster, hasBackdrop, dualAudio int
		var latestStr string
		if err := rows.Scan(
			&ss.LibraryID, &ss.LibraryKind, &ss.Title, &ss.OriginalTitle, &minYear,
			&ss.Rating, &ss.ContentRating, &ss.GenresJSON, &ss.Overview, &ss.VideoHeight, &ss.VideoHDR,
			&ss.SeasonCount, &ss.EpisodeCount, &ss.RepresentativeItemID,
			&hasPoster, &hasBackdrop, &dualAudio, &latestStr,
		); err != nil {
			return nil, err
		}
		if minYear.Valid && minYear.Int64 > 0 {
			v := int(minYear.Int64)
			ss.Year = &v
		}
		ss.HasPoster = hasPoster != 0
		ss.HasBackdrop = hasBackdrop != 0
		ss.DualAudio = dualAudio != 0
		ss.LatestAddedAt = parseTime(latestStr)
		out = append(out, ss)
	}
	return out, rows.Err()
}

// ListSeriesEpisodes returns all episodes for a series ordered by season and episode number,
// joined with the requesting user's playback progress and scoped to accessible libraries.
func (s *Store) ListSeriesEpisodes(ctx context.Context, libraryID, seriesTitle, userID string, allowedLibraryIDs []string, restrictLibraries bool) ([]EpisodeWithProgress, error) {
	if restrictLibraries && len(allowedLibraryIDs) == 0 {
		return []EpisodeWithProgress{}, nil
	}
	args := []any{userID, seriesTitle}
	var extraClauses []string
	if libraryID != "" {
		extraClauses = append(extraClauses, "AND m.library_id = ?")
		args = append(args, libraryID)
	}
	if restrictLibraries {
		placeholders := make([]string, len(allowedLibraryIDs))
		for i, id := range allowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		extraClauses = append(extraClauses, "AND m.library_id IN ("+strings.Join(placeholders, ",")+")")
	}
	libClause := strings.Join(extraClauses, " ")

	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+mediaItemColumnsM+`,
		       p.position_ms, p.duration_ms, p.completed, p.play_count, p.last_played_at
		FROM media_items m
		LEFT JOIN playback_progress p ON p.item_id = m.id AND p.user_id = ?
		WHERE m.kind = 'episode' AND m.series_title = ? COLLATE NOCASE `+libClause+`
		ORDER BY COALESCE(m.season_number, 1) ASC, COALESCE(m.episode_number, m.absolute_episode_number, 0) ASC, m.sort_title COLLATE NOCASE ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EpisodeWithProgress
	for rows.Next() {
		var m MediaItem
		var year, season, episode, absEpisode sql.NullInt64
		var probedStr sql.NullString
		var createdStr, updatedStr string
		var posMS, durMS, completed, playCount sql.NullInt64
		var lastPlayed sql.NullString

		if err := rows.Scan(
			&m.ID, &m.LibraryID, &m.Kind, &m.Title, &m.OriginalTitle, &m.SortTitle, &year,
			&m.SeriesTitle, &season, &episode, &absEpisode, &m.ReleaseGroup,
			&m.FilePath, &m.FileSize, &m.FileMtime, &m.Container,
			&m.DurationMS, &m.Bitrate, &m.VideoCodec, &m.VideoWidth, &m.VideoHeight, &m.VideoHDR,
			&m.AudioCodec, &m.AudioChannels, &m.AudioLanguages, &m.StreamsJSON, &m.SubtitlesJSON, &m.ChaptersJSON,
			&m.PosterPath, &m.BackdropPath,
			&m.Overview, &m.Tagline, &m.Rating, &m.GenresJSON,
			&m.ContentRating, &m.ReleaseDate, &m.DirectorsJSON, &m.WritersJSON, &m.StudiosJSON, &m.CastJSON,
			&m.TMDBID, &m.IMDBID, &m.EpisodeTMDBID, &m.EpisodeIMDBID, &m.MetadataVersion,
			&probedStr, &createdStr, &updatedStr,
			&posMS, &durMS, &completed, &playCount, &lastPlayed,
		); err != nil {
			return nil, err
		}
		if year.Valid {
			v := int(year.Int64)
			m.Year = &v
		}
		if season.Valid {
			v := int(season.Int64)
			m.SeasonNumber = &v
		}
		if episode.Valid {
			v := int(episode.Int64)
			m.EpisodeNumber = &v
		}
		if absEpisode.Valid {
			v := int(absEpisode.Int64)
			m.AbsoluteEpisodeNumber = &v
		}
		if probedStr.Valid {
			t := parseTime(probedStr.String)
			m.ProbedAt = &t
		}
		m.CreatedAt = parseTime(createdStr)
		m.UpdatedAt = parseTime(updatedStr)

		var prog *PlaybackProgress
		if posMS.Valid {
			prog = &PlaybackProgress{
				UserID:       userID,
				ItemID:       m.ID,
				PositionMS:   posMS.Int64,
				DurationMS:   durMS.Int64,
				Completed:    completed.Int64 != 0,
				PlayCount:    int(playCount.Int64),
				LastPlayedAt: parseTime(lastPlayed.String),
			}
		}
		out = append(out, EpisodeWithProgress{Item: m, Progress: prog})
	}
	return out, rows.Err()
}

// GetNextEpisode finds the immediately following episode in the same series (bridging seasons
// and supporting absolute episode numbering).
func (s *Store) GetNextEpisode(ctx context.Context, currentItemID string) (MediaItem, error) {
	cur, err := s.GetMediaItem(ctx, currentItemID)
	if err != nil {
		return MediaItem{}, err
	}
	if cur.Kind != "episode" || cur.SeriesTitle == "" {
		return MediaItem{}, ErrNotFound
	}

	curSeason := 1
	if cur.SeasonNumber != nil {
		curSeason = *cur.SeasonNumber
	}
	var curEp int
	switch {
	case cur.EpisodeNumber != nil:
		curEp = *cur.EpisodeNumber
	case cur.AbsoluteEpisodeNumber != nil:
		curEp = *cur.AbsoluteEpisodeNumber
	default:
		return MediaItem{}, ErrNotFound
	}

	row := s.reader.QueryRowContext(ctx, `
		SELECT `+mediaItemColumns+`
		FROM media_items
		WHERE library_id = ?
		  AND kind = 'episode'
		  AND series_title = ? COLLATE NOCASE
		  AND (
		      (COALESCE(season_number, 1) = ? AND COALESCE(episode_number, absolute_episode_number, 0) > ?)
		      OR (COALESCE(season_number, 1) > ?)
		  )
		ORDER BY COALESCE(season_number, 1) ASC, COALESCE(episode_number, absolute_episode_number, 0) ASC
		LIMIT 1
	`, cur.LibraryID, cur.SeriesTitle, curSeason, curEp, curSeason)

	next, err := scanMediaItem(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MediaItem{}, ErrNotFound
	}
	return next, err
}

// SetSeriesWatched atomically marks all episodes of a series (or one season when seasonNumber != nil)
// as watched or unwatched for a user.
func (s *Store) SetSeriesWatched(ctx context.Context, userID, libraryID, seriesTitle string, seasonNumber *int, completed bool) (int, error) {
	args := []any{libraryID, seriesTitle}
	seasonClause := ""
	if seasonNumber != nil {
		seasonClause = " AND COALESCE(season_number, 0) = ?"
		args = append(args, *seasonNumber)
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, duration_ms
		FROM media_items
		WHERE library_id = ? AND kind = 'episode' AND series_title = ? COLLATE NOCASE`+seasonClause,
		args...,
	)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	type epRow struct {
		id  string
		dur int64
	}
	var eps []epRow
	for rows.Next() {
		var r epRow
		if err := rows.Scan(&r.id, &r.dur); err != nil {
			return 0, err
		}
		eps = append(eps, r)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(eps) == 0 {
		return 0, ErrNotFound
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	if completed {
		upsertStmt, err := tx.PrepareContext(ctx, `
			INSERT INTO playback_progress (user_id, item_id, position_ms, duration_ms, completed, play_count, last_played_at)
			VALUES (?, ?, ?, ?, 1, 1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
			ON CONFLICT(user_id, item_id) DO UPDATE SET
				position_ms    = excluded.position_ms,
				duration_ms    = CASE WHEN excluded.duration_ms > 0 THEN excluded.duration_ms ELSE playback_progress.duration_ms END,
				completed      = 1,
				play_count     = CASE WHEN playback_progress.completed = 0 THEN playback_progress.play_count + 1 ELSE playback_progress.play_count END,
				last_played_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
		`)
		if err != nil {
			return 0, err
		}
		defer upsertStmt.Close()

		clearWLStmt, err := tx.PrepareContext(ctx, `
			DELETE FROM user_media_marks WHERE user_id = ? AND item_id = ? AND kind = ?
		`)
		if err != nil {
			return 0, err
		}
		defer clearWLStmt.Close()

		for _, ep := range eps {
			pos := ep.dur
			if pos <= 0 {
				pos = 1
			}
			if _, err := upsertStmt.ExecContext(ctx, userID, ep.id, pos, ep.dur); err != nil {
				return 0, err
			}
			if _, err := clearWLStmt.ExecContext(ctx, userID, ep.id, MarkKindWatchlist); err != nil {
				return 0, err
			}
		}
	} else {
		delStmt, err := tx.PrepareContext(ctx, `
			DELETE FROM playback_progress WHERE user_id = ? AND item_id = ?
		`)
		if err != nil {
			return 0, err
		}
		defer delStmt.Close()

		for _, ep := range eps {
			if _, err := delStmt.ExecContext(ctx, userID, ep.id); err != nil {
				return 0, err
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(eps), nil
}
