package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

type PlaybackProgress struct {
	UserID       string
	ItemID       string
	PositionMS   int64
	DurationMS   int64
	Completed    bool
	PlayCount    int
	LastPlayedAt time.Time
}

type ContinueWatchingEntry struct {
	Item     MediaItem
	Progress PlaybackProgress
}

type NextUpEntry struct {
	Item         MediaItem
	LastPlayedAt time.Time
}

// GetOrCreateSetting atomically retrieves a server setting or initializes it with generate().
func (s *Store) GetOrCreateSetting(ctx context.Context, key string, generate func() (string, error)) (string, error) {
	var val string
	err := s.reader.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&val)
	if err == nil {
		return val, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}

	newVal, err := generate()
	if err != nil {
		return "", err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO server_settings (key, value) VALUES (?, ?)`, key, newVal); err != nil {
		return "", err
	}
	if err := s.reader.QueryRowContext(ctx, `SELECT value FROM server_settings WHERE key = ?`, key).Scan(&val); err != nil {
		return "", err
	}
	return val, nil
}

// UpsertPlaybackProgress records a user's watch position and completion status for an item.
func (s *Store) UpsertPlaybackProgress(ctx context.Context, userID, itemID string, positionMS, durationMS int64, completedOverride *bool) (PlaybackProgress, error) {
	if positionMS < 0 {
		positionMS = 0
	}

	var prevCompleted int
	var prevPlayCount int
	err := s.reader.QueryRowContext(ctx, `
		SELECT completed, play_count FROM playback_progress WHERE user_id = ? AND item_id = ?
	`, userID, itemID).Scan(&prevCompleted, &prevPlayCount)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PlaybackProgress{}, err
	}

	completed := false
	if completedOverride != nil {
		completed = *completedOverride
	} else if durationMS > 0 && float64(positionMS)/float64(durationMS) >= 0.92 {
		completed = true
	}

	playCount := prevPlayCount
	if completed && prevCompleted == 0 {
		playCount++
	}
	completedInt := 0
	if completed {
		completedInt = 1
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PlaybackProgress{}, err
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO playback_progress (user_id, item_id, position_ms, duration_ms, completed, play_count, last_played_at)
		VALUES (?, ?, ?, ?, ?, ?, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
		ON CONFLICT(user_id, item_id) DO UPDATE SET
			position_ms    = excluded.position_ms,
			duration_ms    = CASE WHEN excluded.duration_ms > 0 THEN excluded.duration_ms ELSE playback_progress.duration_ms END,
			completed      = excluded.completed,
			play_count     = excluded.play_count,
			last_played_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
	`, userID, itemID, positionMS, durationMS, completedInt, playCount)
	if err != nil {
		return PlaybackProgress{}, err
	}

	// Completing an item automatically clears it from the user's Watchlist queue while preserving Favorites.
	if completed {
		if _, err := tx.ExecContext(ctx, `
			DELETE FROM user_media_marks
			WHERE user_id = ? AND item_id = ? AND kind = ?
		`, userID, itemID, MarkKindWatchlist); err != nil {
			return PlaybackProgress{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return PlaybackProgress{}, err
	}

	return s.GetPlaybackProgress(ctx, userID, itemID)
}

func (s *Store) GetPlaybackProgress(ctx context.Context, userID, itemID string) (PlaybackProgress, error) {
	var p PlaybackProgress
	var completed int
	var lastStr string
	err := s.reader.QueryRowContext(ctx, `
		SELECT user_id, item_id, position_ms, duration_ms, completed, play_count, last_played_at
		FROM playback_progress
		WHERE user_id = ? AND item_id = ?
	`, userID, itemID).Scan(&p.UserID, &p.ItemID, &p.PositionMS, &p.DurationMS, &completed, &p.PlayCount, &lastStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PlaybackProgress{}, ErrNotFound
		}
		return PlaybackProgress{}, err
	}
	p.Completed = completed != 0
	p.LastPlayedAt = parseTime(lastStr)
	return p, nil
}

// DeletePlaybackProgress removes a user's watch progress record for an item,
// clearing its resume position and removing it from Continue Watching.
func (s *Store) DeletePlaybackProgress(ctx context.Context, userID, itemID string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM playback_progress
		WHERE user_id = ? AND item_id = ?
	`, userID, itemID)
	return err
}

func (s *Store) ListContinueWatching(ctx context.Context, userID string, limit int, allowedLibraryIDs []string, restrictLibraries bool) ([]ContinueWatchingEntry, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if restrictLibraries && len(allowedLibraryIDs) == 0 {
		return []ContinueWatchingEntry{}, nil
	}

	args := []any{userID}
	libClause := ""
	if restrictLibraries {
		placeholders := make([]string, len(allowedLibraryIDs))
		for i, id := range allowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		libClause = "AND m.library_id IN (" + strings.Join(placeholders, ",") + ")"
	}
	args = append(args, limit)

	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.id, m.library_id, m.kind, m.title, m.original_title, m.sort_title, m.year,
		       m.series_title, m.season_number, m.episode_number, m.absolute_episode_number, m.release_group,
		       m.file_path, m.file_size, m.file_mtime, m.container,
		       m.duration_ms, m.bitrate, m.video_codec, m.video_width, m.video_height, m.video_hdr,
		       m.audio_codec, m.audio_channels, m.audio_languages, m.streams_json, m.poster_path, m.backdrop_path,
		       m.overview, m.rating, m.content_rating, m.genres_json,
		       m.probed_at, m.created_at, m.updated_at,
		       p.position_ms, p.duration_ms, p.completed, p.play_count, p.last_played_at
		FROM playback_progress p
		JOIN media_items m ON m.id = p.item_id
		WHERE p.user_id = ? AND p.completed = 0 AND p.position_ms > 0 `+libClause+`
		ORDER BY p.last_played_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ContinueWatchingEntry
	for rows.Next() {
		var m MediaItem
		var year, season, episode, absEpisode sql.NullInt64
		var probedStr sql.NullString
		var createdStr, updatedStr string
		var p PlaybackProgress
		var completed int
		var lastStr string

		if err := rows.Scan(
			&m.ID, &m.LibraryID, &m.Kind, &m.Title, &m.OriginalTitle, &m.SortTitle, &year,
			&m.SeriesTitle, &season, &episode, &absEpisode, &m.ReleaseGroup,
			&m.FilePath, &m.FileSize, &m.FileMtime, &m.Container,
			&m.DurationMS, &m.Bitrate, &m.VideoCodec, &m.VideoWidth, &m.VideoHeight, &m.VideoHDR,
			&m.AudioCodec, &m.AudioChannels, &m.AudioLanguages, &m.StreamsJSON, &m.PosterPath, &m.BackdropPath,
			&m.Overview, &m.Rating, &m.ContentRating, &m.GenresJSON,
			&probedStr, &createdStr, &updatedStr,
			&p.PositionMS, &p.DurationMS, &completed, &p.PlayCount, &lastStr,
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

		p.UserID = userID
		p.ItemID = m.ID
		p.Completed = completed != 0
		p.LastPlayedAt = parseTime(lastStr)
		out = append(out, ContinueWatchingEntry{Item: m, Progress: p})
	}
	return out, rows.Err()
}

// ListNextUp returns the next unwatched episode for each series the user has recently watched,
// ordered by the series's most recent playback timestamp. allowedLibraryIDs == nil means unrestricted.
func (s *Store) ListNextUp(ctx context.Context, userID string, allowedLibraryIDs []string, limit int) ([]NextUpEntry, error) {
	if limit <= 0 || limit > 50 {
		limit = 20
	}
	if allowedLibraryIDs != nil && len(allowedLibraryIDs) == 0 {
		return []NextUpEntry{}, nil
	}

	args := []any{userID}
	libClause := ""
	if allowedLibraryIDs != nil {
		placeholders := make([]string, len(allowedLibraryIDs))
		for i, id := range allowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		libClause = "AND m.library_id IN (" + strings.Join(placeholders, ",") + ")"
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT m.library_id, m.series_title, MAX(p.last_played_at) AS last_played
		FROM playback_progress p
		JOIN media_items m ON m.id = p.item_id
		WHERE p.user_id = ? AND m.kind = 'episode' AND m.series_title != '' `+libClause+`
		GROUP BY m.library_id, m.series_title COLLATE NOCASE
		ORDER BY last_played DESC
	`, args...)
	if err != nil {
		return nil, err
	}

	type seriesActivity struct {
		libraryID    string
		seriesTitle  string
		lastPlayedAt time.Time
	}
	var activeSeries []seriesActivity
	for rows.Next() {
		var sr seriesActivity
		var lastStr string
		if err := rows.Scan(&sr.libraryID, &sr.seriesTitle, &lastStr); err != nil {
			rows.Close()
			return nil, err
		}
		sr.lastPlayedAt = parseTime(lastStr)
		activeSeries = append(activeSeries, sr)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	epSeason := func(m MediaItem) int {
		if m.SeasonNumber != nil {
			return *m.SeasonNumber
		}
		return 1
	}

	out := make([]NextUpEntry, 0, min(len(activeSeries), limit))
	for _, sr := range activeSeries {
		if len(out) >= limit {
			break
		}
		eps, err := s.ListSeriesEpisodes(ctx, sr.libraryID, sr.seriesTitle, userID, nil, false)
		if err != nil {
			return nil, err
		}
		if len(eps) == 0 {
			continue
		}

		// Find the highest completed regular-season episode (season > 0);
		// fall back to season 0 only if no regular-season episodes have been completed.
		lastCompletedIdx := -1
		for i, ep := range eps {
			if ep.Progress != nil && ep.Progress.Completed && epSeason(ep.Item) > 0 {
				lastCompletedIdx = i
			}
		}
		regularOnly := true
		if lastCompletedIdx < 0 {
			for i, ep := range eps {
				if ep.Progress != nil && ep.Progress.Completed {
					lastCompletedIdx = i
				}
			}
			regularOnly = false
		}
		if lastCompletedIdx < 0 {
			continue
		}

		for _, ep := range eps[lastCompletedIdx+1:] {
			if regularOnly && epSeason(ep.Item) <= 0 {
				continue
			}
			if ep.Progress != nil && ep.Progress.Completed {
				continue
			}
			// Exclude episodes currently in progress so they aren't duplicated across Continue Watching and Next Up.
			if ep.Progress != nil && ep.Progress.PositionMS > 0 && !ep.Progress.Completed {
				break
			}
			lastPlayed := sr.lastPlayedAt
			if lastPlayed.IsZero() {
				lastPlayed = ep.Item.CreatedAt
			}
			out = append(out, NextUpEntry{
				Item:         ep.Item,
				LastPlayedAt: lastPlayed,
			})
			break
		}
	}
	return out, nil
}
