package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type Library struct {
	ID        string
	Name      string
	Kind      string // "movies" | "shows" | "anime" | "music" | "other"
	RootPath  string
	ItemCount int
	CreatedAt time.Time
	UpdatedAt time.Time
}

type MediaItem struct {
	ID                    string
	LibraryID             string
	Kind                  string // "movie" | "episode" | "track" | "other"
	Title                 string
	OriginalTitle         string
	SortTitle             string
	Year                  *int
	SeriesTitle           string
	SeasonNumber          *int
	EpisodeNumber         *int
	AbsoluteEpisodeNumber *int
	ReleaseGroup          string
	FilePath              string
	FileSize              int64
	FileMtime             int64
	Container             string
	DurationMS            int64
	Bitrate               int64
	VideoCodec            string
	VideoWidth            int
	VideoHeight           int
	VideoHDR              string
	AudioCodec            string
	AudioChannels         int
	AudioLanguages        string // comma-separated normalized ISO-639-1 audio languages, e.g. "ja,en"
	StreamsJSON           string
	SubtitlesJSON         string
	ChaptersJSON          string
	PosterPath            string
	BackdropPath          string
	Overview              string
	Tagline               string
	Rating                float64
	GenresJSON            string
	ContentRating         string
	ReleaseDate           string
	DirectorsJSON         string
	WritersJSON           string
	StudiosJSON           string
	CastJSON              string
	TMDBID                string
	IMDBID                string
	EpisodeTMDBID         string
	EpisodeIMDBID         string
	MetadataVersion       int
	ProbedAt              *time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

type MediaFileFingerprint struct {
	ID              string
	FilePath        string
	FileSize        int64
	FileMtime       int64
	MetadataVersion int
	TMDBID          string
}

type MediaListFilter struct {
	LibraryID         string
	AllowedLibraryIDs []string
	RestrictLibraries bool
	UserID            string
	Kind              string
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
	Sort              string // "title" | "recent" | "year" | "rating"
	Limit             int
	Offset            int
}

// LibraryFacets summarizes distinct filterable values present in a library.
type LibraryFacets struct {
	Genres         []string `json:"genres"`
	ContentRatings []string `json:"content_ratings"`
	Years          []int    `json:"years"`
	Has4K          bool     `json:"has_4k"`
	HasHDR         bool     `json:"has_hdr"`
	HasDualAudio   bool     `json:"has_dual_audio"`
}

func (s *Store) CreateLibrary(ctx context.Context, lib Library) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO libraries (id, name, kind, root_path)
		VALUES (?, ?, ?, ?)
	`, lib.ID, lib.Name, lib.Kind, lib.RootPath)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrAlreadyExists
	}
	return err
}

func (s *Store) GetLibrary(ctx context.Context, id string) (Library, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT l.id, l.name, l.kind, l.root_path, COUNT(m.id), l.created_at, l.updated_at
		FROM libraries l
		LEFT JOIN media_items m ON m.library_id = l.id
		WHERE l.id = ?
		GROUP BY l.id
	`, id)
	return scanLibrary(row)
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	return s.ListLibrariesFiltered(ctx, nil, false)
}

func (s *Store) ListLibrariesFiltered(ctx context.Context, allowedIDs []string, restrict bool) ([]Library, error) {
	if restrict && len(allowedIDs) == 0 {
		return []Library{}, nil
	}
	whereSQL := ""
	var args []any
	if restrict {
		placeholders := make([]string, len(allowedIDs))
		args = make([]any, len(allowedIDs))
		for i, id := range allowedIDs {
			placeholders[i] = "?"
			args[i] = id
		}
		whereSQL = "WHERE l.id IN (" + strings.Join(placeholders, ",") + ")"
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT l.id, l.name, l.kind, l.root_path, COUNT(m.id), l.created_at, l.updated_at
		FROM libraries l
		LEFT JOIN media_items m ON m.library_id = l.id
		`+whereSQL+`
		GROUP BY l.id
		ORDER BY l.name COLLATE NOCASE ASC
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Library
	for rows.Next() {
		lib, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lib)
	}
	return out, rows.Err()
}

func (s *Store) DeleteLibrary(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM libraries WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) ListLibraryFingerprints(ctx context.Context, libraryID string) (map[string]MediaFileFingerprint, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, file_path, file_size, file_mtime, metadata_version, tmdb_id
		FROM media_items
		WHERE library_id = ?
	`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]MediaFileFingerprint)
	for rows.Next() {
		var fp MediaFileFingerprint
		if err := rows.Scan(&fp.ID, &fp.FilePath, &fp.FileSize, &fp.FileMtime, &fp.MetadataVersion, &fp.TMDBID); err != nil {
			return nil, err
		}
		out[fp.FilePath] = fp
	}
	return out, rows.Err()
}

// ListLibrariesNeedingMetadataBackfill returns library IDs (for enrichable video libraries)
// that contain at least one item with metadata_version < currentVersion.
func (s *Store) ListLibrariesNeedingMetadataBackfill(ctx context.Context, currentVersion int) ([]string, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT DISTINCT m.library_id
		FROM media_items m
		JOIN libraries l ON l.id = m.library_id
		WHERE m.metadata_version < ?
		  AND l.kind IN ('movies', 'shows', 'anime')
	`, currentVersion)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

const upsertMediaItemSQL = `
	INSERT INTO media_items (
		id, library_id, kind, title, original_title, sort_title, year,
		series_title, season_number, episode_number, absolute_episode_number, release_group,
		file_path, file_size, file_mtime, container,
		duration_ms, bitrate, video_codec, video_width, video_height, video_hdr,
		audio_codec, audio_channels, audio_languages, streams_json, subtitles_json, chapters_json,
		poster_path, backdrop_path,
		overview, tagline, rating, genres_json,
		content_rating, release_date, directors_json, writers_json, studios_json, cast_json,
		tmdb_id, imdb_id, episode_tmdb_id, episode_imdb_id, metadata_version, probed_at
	) VALUES (
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?, ?, ?, ?, ?,
		?, ?, ?, ?, ?, ?
	)
	ON CONFLICT(file_path) DO UPDATE SET
		library_id              = excluded.library_id,
		kind                    = excluded.kind,
		title                   = excluded.title,
		original_title          = excluded.original_title,
		sort_title              = excluded.sort_title,
		year                    = excluded.year,
		series_title            = excluded.series_title,
		season_number           = excluded.season_number,
		episode_number          = excluded.episode_number,
		absolute_episode_number = excluded.absolute_episode_number,
		release_group           = excluded.release_group,
		file_size               = excluded.file_size,
		file_mtime              = excluded.file_mtime,
		container               = excluded.container,
		duration_ms             = excluded.duration_ms,
		bitrate                 = excluded.bitrate,
		video_codec             = excluded.video_codec,
		video_width             = excluded.video_width,
		video_height            = excluded.video_height,
		video_hdr               = excluded.video_hdr,
		audio_codec             = excluded.audio_codec,
		audio_channels          = excluded.audio_channels,
		audio_languages         = excluded.audio_languages,
		streams_json            = excluded.streams_json,
		subtitles_json          = excluded.subtitles_json,
		chapters_json           = excluded.chapters_json,
		poster_path             = excluded.poster_path,
		backdrop_path           = excluded.backdrop_path,
		overview                = excluded.overview,
		tagline                 = excluded.tagline,
		rating                  = excluded.rating,
		genres_json             = excluded.genres_json,
		content_rating          = excluded.content_rating,
		release_date            = excluded.release_date,
		directors_json          = excluded.directors_json,
		writers_json            = excluded.writers_json,
		studios_json            = excluded.studios_json,
		cast_json               = excluded.cast_json,
		tmdb_id                 = excluded.tmdb_id,
		imdb_id                 = excluded.imdb_id,
		episode_tmdb_id         = excluded.episode_tmdb_id,
		episode_imdb_id         = excluded.episode_imdb_id,
		metadata_version        = excluded.metadata_version,
		probed_at               = excluded.probed_at,
		updated_at              = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
`

func mediaItemUpsertArgs(item MediaItem) []any {
	var year, season, episode, absEpisode sql.NullInt64
	if item.Year != nil {
		year = sql.NullInt64{Int64: int64(*item.Year), Valid: true}
	}
	if item.SeasonNumber != nil {
		season = sql.NullInt64{Int64: int64(*item.SeasonNumber), Valid: true}
	}
	if item.EpisodeNumber != nil {
		episode = sql.NullInt64{Int64: int64(*item.EpisodeNumber), Valid: true}
	}
	if item.AbsoluteEpisodeNumber != nil {
		absEpisode = sql.NullInt64{Int64: int64(*item.AbsoluteEpisodeNumber), Valid: true}
	}
	var probedAt sql.NullString
	if item.ProbedAt != nil {
		probedAt = sql.NullString{String: formatTime(*item.ProbedAt), Valid: true}
	}
	streamsJSON := defaultJSONArray(item.StreamsJSON)
	subtitlesJSON := defaultJSONArray(item.SubtitlesJSON)
	chaptersJSON := defaultJSONArray(item.ChaptersJSON)
	genresJSON := defaultJSONArray(item.GenresJSON)
	directorsJSON := defaultJSONArray(item.DirectorsJSON)
	writersJSON := defaultJSONArray(item.WritersJSON)
	studiosJSON := defaultJSONArray(item.StudiosJSON)
	castJSON := defaultJSONArray(item.CastJSON)
	return []any{
		item.ID, item.LibraryID, item.Kind, item.Title, item.OriginalTitle, item.SortTitle, year,
		item.SeriesTitle, season, episode, absEpisode, item.ReleaseGroup,
		item.FilePath, item.FileSize, item.FileMtime, item.Container,
		item.DurationMS, item.Bitrate, item.VideoCodec, item.VideoWidth, item.VideoHeight, item.VideoHDR,
		item.AudioCodec, item.AudioChannels, item.AudioLanguages, streamsJSON, subtitlesJSON, chaptersJSON,
		item.PosterPath, item.BackdropPath,
		item.Overview, item.Tagline, item.Rating, genresJSON,
		item.ContentRating, item.ReleaseDate, directorsJSON, writersJSON, studiosJSON, castJSON,
		item.TMDBID, item.IMDBID, item.EpisodeTMDBID, item.EpisodeIMDBID, item.MetadataVersion, probedAt,
	}
}

func defaultJSONArray(raw string) string {
	if raw == "" {
		return "[]"
	}
	return raw
}

func (s *Store) UpsertMediaItem(ctx context.Context, item MediaItem) error {
	_, err := s.db.ExecContext(ctx, upsertMediaItemSQL, mediaItemUpsertArgs(item)...)
	return err
}

// UpsertMediaItemsBatch persists a slice of media items in a single prepared transaction,
// eliminating per-item fsync overhead and writer lock contention during large scans.
func (s *Store) UpsertMediaItemsBatch(ctx context.Context, items []MediaItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, upsertMediaItemSQL)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, item := range items {
		if _, err := stmt.ExecContext(ctx, mediaItemUpsertArgs(item)...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteMediaItemsByIDs(ctx context.Context, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `DELETE FROM media_items WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var deleted int64
	for _, id := range ids {
		res, err := stmt.ExecContext(ctx, id)
		if err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

const mediaItemColumns = `id, library_id, kind, title, original_title, sort_title, year,
		       series_title, season_number, episode_number, absolute_episode_number, release_group,
		       file_path, file_size, file_mtime, container,
		       duration_ms, bitrate, video_codec, video_width, video_height, video_hdr,
		       audio_codec, audio_channels, audio_languages, streams_json, subtitles_json, chapters_json,
		       poster_path, backdrop_path,
		       overview, tagline, rating, genres_json,
		       content_rating, release_date, directors_json, writers_json, studios_json, cast_json,
		       tmdb_id, imdb_id, episode_tmdb_id, episode_imdb_id, metadata_version,
		       probed_at, created_at, updated_at`

const mediaItemColumnsM = `m.id, m.library_id, m.kind, m.title, m.original_title, m.sort_title, m.year,
		       m.series_title, m.season_number, m.episode_number, m.absolute_episode_number, m.release_group,
		       m.file_path, m.file_size, m.file_mtime, m.container,
		       m.duration_ms, m.bitrate, m.video_codec, m.video_width, m.video_height, m.video_hdr,
		       m.audio_codec, m.audio_channels, m.audio_languages, m.streams_json, m.subtitles_json, m.chapters_json,
		       m.poster_path, m.backdrop_path,
		       m.overview, m.tagline, m.rating, m.genres_json,
		       m.content_rating, m.release_date, m.directors_json, m.writers_json, m.studios_json, m.cast_json,
		       m.tmdb_id, m.imdb_id, m.episode_tmdb_id, m.episode_imdb_id, m.metadata_version,
		       m.probed_at, m.created_at, m.updated_at`

func (s *Store) GetMediaItem(ctx context.Context, id string) (MediaItem, error) {
	row := s.reader.QueryRowContext(ctx, `
		SELECT `+mediaItemColumns+`
		FROM media_items
		WHERE id = ?
	`, id)
	return scanMediaItem(row)
}

func (s *Store) ListMediaItems(ctx context.Context, f MediaListFilter) ([]MediaItem, int, error) {
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	if f.Offset < 0 {
		f.Offset = 0
	}

	if f.RestrictLibraries && len(f.AllowedLibraryIDs) == 0 {
		return []MediaItem{}, 0, nil
	}

	var where []string
	var args []any
	if f.LibraryID != "" {
		where = append(where, "library_id = ?")
		args = append(args, f.LibraryID)
	}
	if f.RestrictLibraries {
		placeholders := make([]string, len(f.AllowedLibraryIDs))
		for i, id := range f.AllowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "library_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if f.Kind != "" {
		where = append(where, "kind = ?")
		args = append(args, f.Kind)
	}
	if q := strings.TrimSpace(f.Query); q != "" {
		if ftsQ := buildFTSQuery(q); ftsQ != "" {
			where = append(where, "rowid IN (SELECT rowid FROM media_items_fts WHERE media_items_fts MATCH ?)")
			args = append(args, ftsQ)
		} else {
			like := "%" + q + "%"
			where = append(where, "(title LIKE ? COLLATE NOCASE OR original_title LIKE ? COLLATE NOCASE OR series_title LIKE ? COLLATE NOCASE)")
			args = append(args, like, like, like)
		}
	}
	if genre := strings.TrimSpace(f.Genre); genre != "" {
		where = append(where, `genres_json LIKE ? COLLATE NOCASE`)
		args = append(args, `%"`+escapeJSONLike(genre)+`"%`)
	}
	if studio := strings.TrimSpace(f.Studio); studio != "" {
		where = append(where, `studios_json LIKE ? COLLATE NOCASE`)
		args = append(args, `%"`+escapeJSONLike(studio)+`"%`)
	}
	if person := strings.TrimSpace(f.Person); person != "" {
		pat := `%"` + escapeJSONLike(person) + `"%`
		where = append(where, `(cast_json LIKE ? COLLATE NOCASE OR directors_json LIKE ? COLLATE NOCASE OR writers_json LIKE ? COLLATE NOCASE)`)
		args = append(args, pat, pat, pat)
	}
	if cr := strings.TrimSpace(f.ContentRating); cr != "" {
		where = append(where, "content_rating = ? COLLATE NOCASE")
		args = append(args, cr)
	}
	if f.MinRating > 0 {
		where = append(where, "rating >= ?")
		args = append(args, f.MinRating)
	}
	if f.Year > 0 {
		where = append(where, "year = ?")
		args = append(args, f.Year)
	} else if f.Decade > 0 {
		where = append(where, "year >= ? AND year <= ?")
		args = append(args, f.Decade, f.Decade+9)
	}
	switch strings.ToLower(strings.TrimSpace(f.Resolution)) {
	case "4k":
		where = append(where, "(video_width >= 3200 OR video_height >= 2000)")
	case "1080p":
		where = append(where, "NOT (video_width >= 3200 OR video_height >= 2000) AND (video_width >= 1800 OR video_height >= 1000)")
	case "720p":
		where = append(where, "NOT (video_width >= 3200 OR video_height >= 2000) AND NOT (video_width >= 1800 OR video_height >= 1000) AND (video_width >= 1200 OR video_height >= 700)")
	}
	if f.HDR {
		where = append(where, "video_hdr != '' AND lower(video_hdr) != 'sdr'")
	}
	if f.DualAudio {
		where = append(where, "instr(audio_languages, ',') > 0")
	}
	if f.UserID != "" {
		switch strings.ToLower(strings.TrimSpace(f.Watched)) {
		case "watched":
			where = append(where, "EXISTS (SELECT 1 FROM playback_progress p WHERE p.item_id = media_items.id AND p.user_id = ? AND p.completed = 1)")
			args = append(args, f.UserID)
		case "unwatched":
			where = append(where, "NOT EXISTS (SELECT 1 FROM playback_progress p WHERE p.item_id = media_items.id AND p.user_id = ? AND p.completed = 1)")
			args = append(args, f.UserID)
		}
	}

	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	var total int
	if err := s.reader.QueryRowContext(ctx, "SELECT COUNT(1) FROM media_items "+whereSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	orderSQL := "ORDER BY sort_title COLLATE NOCASE ASC, season_number ASC, episode_number ASC"
	switch f.Sort {
	case "recent":
		orderSQL = "ORDER BY created_at DESC, sort_title COLLATE NOCASE ASC"
	case "year":
		orderSQL = "ORDER BY COALESCE(year, 0) DESC, sort_title COLLATE NOCASE ASC"
	case "rating":
		orderSQL = "ORDER BY rating DESC, sort_title COLLATE NOCASE ASC"
	}

	querySQL := `
		SELECT ` + mediaItemColumns + `
		FROM media_items ` + whereSQL + " " + orderSQL + " LIMIT ? OFFSET ?"

	queryArgs := append(append([]any{}, args...), f.Limit, f.Offset)
	rows, err := s.reader.QueryContext(ctx, querySQL, queryArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var items []MediaItem
	for rows.Next() {
		item, err := scanMediaItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func escapeJSONLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return s
}

func decodeJSONStrings(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil
	}
	return out
}

func toLowerSet(list []string) map[string]struct{} {
	if len(list) == 0 {
		return nil
	}
	m := make(map[string]struct{}, len(list))
	for _, v := range list {
		if t := strings.ToLower(strings.TrimSpace(v)); t != "" {
			m[t] = struct{}{}
		}
	}
	return m
}

func countOverlap(a, b map[string]struct{}) int {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := 0
	for k := range a {
		if _, ok := b[k]; ok {
			n++
		}
	}
	return n
}

func decodeCastNames(raw string) []string {
	if raw == "" || raw == "[]" {
		return nil
	}
	var people []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal([]byte(raw), &people); err != nil {
		return nil
	}
	names := make([]string, 0, len(people))
	for _, p := range people {
		if n := strings.TrimSpace(p.Name); n != "" {
			names = append(names, n)
		}
	}
	return names
}

type similaritySeed struct {
	libraryID   string
	libraryKind string
	genres      map[string]struct{}
	directors   map[string]struct{}
	writers     map[string]struct{}
	studios     map[string]struct{}
	cast        map[string]struct{}
}

func scoreSimilarity(seed similaritySeed, candLibID, candLibKind, genresJSON, directorsJSON, writersJSON, studiosJSON, castJSON string) int {
	candGenres := toLowerSet(decodeJSONStrings(genresJSON))
	specificGenreOverlap := 0
	animationOverlap := false
	for g := range seed.genres {
		if _, ok := candGenres[g]; ok {
			if g == "animation" {
				animationOverlap = true
			} else {
				specificGenreOverlap++
			}
		}
	}

	dirOverlap := countOverlap(seed.directors, toLowerSet(decodeJSONStrings(directorsJSON)))
	writerOverlap := countOverlap(seed.writers, toLowerSet(decodeJSONStrings(writersJSON)))
	studioOverlap := countOverlap(seed.studios, toLowerSet(decodeJSONStrings(studiosJSON)))
	castOverlap := countOverlap(seed.cast, toLowerSet(decodeCastNames(castJSON)))
	if castOverlap > 3 {
		castOverlap = 3
	}

	nonAnimScore := specificGenreOverlap*3 + dirOverlap*5 + writerOverlap*3 + studioOverlap*2 + castOverlap*2
	contentScore := nonAnimScore
	if animationOverlap {
		// Generic "animation" gets weight 1; if it is the ONLY overlap across different library kinds, ignore it.
		if nonAnimScore > 0 || seed.libraryKind == "" || candLibKind == "" || strings.EqualFold(seed.libraryKind, candLibKind) {
			contentScore += 1
		}
	}
	if contentScore == 0 {
		return 0
	}
	if candLibID == seed.libraryID {
		contentScore += 1
	}
	return contentScore
}

// FindSimilarMedia scores local movies and series in accessible libraries by shared genres,
// directors, writers, studios, and cast without making any external network requests.
func (s *Store) FindSimilarMedia(ctx context.Context, seed MediaItem, allowedLibraryIDs []string, restrictLibraries bool, limit int) ([]MediaItem, []SeriesSummary, error) {
	if limit <= 0 || limit > 24 {
		limit = 12
	}
	if restrictLibraries && len(allowedLibraryIDs) == 0 {
		return []MediaItem{}, []SeriesSummary{}, nil
	}

	libKinds := make(map[string]string)
	if kRows, err := s.reader.QueryContext(ctx, `SELECT id, kind FROM libraries`); err == nil {
		for kRows.Next() {
			var id, kind string
			if err := kRows.Scan(&id, &kind); err == nil {
				libKinds[id] = kind
			}
		}
		_ = kRows.Close()
	}

	simSeed := similaritySeed{
		libraryID:   seed.LibraryID,
		libraryKind: libKinds[seed.LibraryID],
		genres:      toLowerSet(decodeJSONStrings(seed.GenresJSON)),
		directors:   toLowerSet(decodeJSONStrings(seed.DirectorsJSON)),
		writers:     toLowerSet(decodeJSONStrings(seed.WritersJSON)),
		studios:     toLowerSet(decodeJSONStrings(seed.StudiosJSON)),
		cast:        toLowerSet(decodeCastNames(seed.CastJSON)),
	}

	// For episodes, recommend similar series; for movies/other, recommend similar non-episode items.
	if seed.Kind == "episode" {
		var where []string
		var args []any
		where = append(where, "m.kind = 'episode'", "m.series_title != ''")
		if seed.SeriesTitle != "" {
			where = append(where, "NOT (m.library_id = ? AND m.series_title = ? COLLATE NOCASE)")
			args = append(args, seed.LibraryID, seed.SeriesTitle)
		}
		if restrictLibraries {
			placeholders := make([]string, len(allowedLibraryIDs))
			for i, id := range allowedLibraryIDs {
				placeholders[i] = "?"
				args = append(args, id)
			}
			where = append(where, "m.library_id IN ("+strings.Join(placeholders, ",")+")")
		}

		rows, err := s.reader.QueryContext(ctx, `
			SELECT
				m.library_id,
				l.kind,
				m.series_title,
				MAX(m.original_title) AS original_title,
				MIN(m.year) AS min_year,
				COUNT(DISTINCT COALESCE(m.season_number, 1)) AS season_count,
				COUNT(1) AS episode_count,
				MIN(m.id) AS rep_id,
				MAX(CASE WHEN m.poster_path != '' THEN 1 ELSE 0 END) AS has_poster,
				MAX(CASE WHEN m.backdrop_path != '' THEN 1 ELSE 0 END) AS has_backdrop,
				MAX(CASE WHEN instr(m.audio_languages, ',') > 0 THEN 1 ELSE 0 END) AS dual_audio,
				MAX(m.created_at) AS latest_added,
				COALESCE(MAX(NULLIF(m.genres_json, '[]')), '[]') AS genres_json,
				COALESCE(MAX(NULLIF(m.directors_json, '[]')), '[]') AS directors_json,
				COALESCE(MAX(NULLIF(m.writers_json, '[]')), '[]') AS writers_json,
				COALESCE(MAX(NULLIF(m.studios_json, '[]')), '[]') AS studios_json,
				COALESCE(MAX(NULLIF(m.cast_json, '[]')), '[]') AS cast_json
			FROM media_items m
			JOIN libraries l ON l.id = m.library_id
			WHERE `+strings.Join(where, " AND ")+`
			GROUP BY m.library_id, m.series_title COLLATE NOCASE
			LIMIT 250
		`, args...)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()

		type scoredSeries struct {
			ss    SeriesSummary
			score int
		}
		var scored []scoredSeries
		for rows.Next() {
			var ss SeriesSummary
			var minYear sql.NullInt64
			var hasPoster, hasBackdrop, dualAudio int
			var latestStr, genresJSON, directorsJSON, writersJSON, studiosJSON, castJSON string
			if err := rows.Scan(
				&ss.LibraryID, &ss.LibraryKind, &ss.Title, &ss.OriginalTitle, &minYear,
				&ss.SeasonCount, &ss.EpisodeCount, &ss.RepresentativeItemID,
				&hasPoster, &hasBackdrop, &dualAudio, &latestStr,
				&genresJSON, &directorsJSON, &writersJSON, &studiosJSON, &castJSON,
			); err != nil {
				return nil, nil, err
			}
			if minYear.Valid && minYear.Int64 > 0 {
				v := int(minYear.Int64)
				ss.Year = &v
			}
			ss.HasPoster = hasPoster != 0
			ss.HasBackdrop = hasBackdrop != 0
			ss.DualAudio = dualAudio != 0
			ss.LatestAddedAt = parseTime(latestStr)

			sc := scoreSimilarity(simSeed, ss.LibraryID, ss.LibraryKind, genresJSON, directorsJSON, writersJSON, studiosJSON, castJSON)
			if sc > 0 {
				scored = append(scored, scoredSeries{ss: ss, score: sc})
			}
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		sort.SliceStable(scored, func(i, j int) bool {
			if scored[i].score != scored[j].score {
				return scored[i].score > scored[j].score
			}
			return scored[i].ss.LatestAddedAt.After(scored[j].ss.LatestAddedAt)
		})
		if len(scored) > limit {
			scored = scored[:limit]
		}
		out := make([]SeriesSummary, 0, len(scored))
		for _, item := range scored {
			out = append(out, item.ss)
		}
		return []MediaItem{}, out, nil
	}

	var where []string
	var args []any
	where = append(where, "kind = ?", "id != ?")
	args = append(args, seed.Kind, seed.ID)
	if restrictLibraries {
		placeholders := make([]string, len(allowedLibraryIDs))
		for i, id := range allowedLibraryIDs {
			placeholders[i] = "?"
			args = append(args, id)
		}
		where = append(where, "library_id IN ("+strings.Join(placeholders, ",")+")")
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT `+mediaItemColumns+`
		FROM media_items
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY rating DESC, created_at DESC
		LIMIT 250
	`, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	type scoredItem struct {
		item  MediaItem
		score int
	}
	var scored []scoredItem
	for rows.Next() {
		cand, err := scanMediaItem(rows)
		if err != nil {
			return nil, nil, err
		}
		sc := scoreSimilarity(simSeed, cand.LibraryID, libKinds[cand.LibraryID], cand.GenresJSON, cand.DirectorsJSON, cand.WritersJSON, cand.StudiosJSON, cand.CastJSON)
		if sc > 0 {
			scored = append(scored, scoredItem{item: cand, score: sc})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].item.Rating > scored[j].item.Rating
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]MediaItem, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.item)
	}
	return out, []SeriesSummary{}, nil
}

// GetLibraryFacets inspects indexed items in a library and returns the distinct genres,
// content ratings, release years, and video/audio capabilities available for filtering.
func (s *Store) GetLibraryFacets(ctx context.Context, libraryID string) (LibraryFacets, error) {
	facets := LibraryFacets{
		Genres:         []string{},
		ContentRatings: []string{},
		Years:          []int{},
	}

	rows, err := s.reader.QueryContext(ctx, `
		SELECT genres_json, content_rating, year, video_width, video_height, video_hdr, audio_languages
		FROM media_items
		WHERE library_id = ?
	`, libraryID)
	if err != nil {
		return facets, err
	}
	defer rows.Close()

	genreSet := make(map[string]string) // lower -> canonical display
	ratingSet := make(map[string]string)
	yearSet := make(map[int]struct{})

	for rows.Next() {
		var genresJSON, cert, hdr, audioLangs string
		var yr sql.NullInt64
		var width, height int
		if err := rows.Scan(&genresJSON, &cert, &yr, &width, &height, &hdr, &audioLangs); err != nil {
			return facets, err
		}
		for _, g := range decodeJSONStrings(genresJSON) {
			trimmed := strings.TrimSpace(g)
			if trimmed == "" {
				continue
			}
			low := strings.ToLower(trimmed)
			if _, exists := genreSet[low]; !exists {
				genreSet[low] = trimmed
			}
		}
		if trimmed := strings.TrimSpace(cert); trimmed != "" {
			low := strings.ToLower(trimmed)
			if _, exists := ratingSet[low]; !exists {
				ratingSet[low] = trimmed
			}
		}
		if yr.Valid && yr.Int64 > 1880 {
			yearSet[int(yr.Int64)] = struct{}{}
		}
		if width >= 3200 || height >= 2000 {
			facets.Has4K = true
		}
		if hdr != "" && !strings.EqualFold(hdr, "sdr") {
			facets.HasHDR = true
		}
		if strings.Contains(audioLangs, ",") {
			facets.HasDualAudio = true
		}
	}
	if err := rows.Err(); err != nil {
		return facets, err
	}

	for _, g := range genreSet {
		facets.Genres = append(facets.Genres, g)
	}
	sort.Slice(facets.Genres, func(i, j int) bool {
		return strings.ToLower(facets.Genres[i]) < strings.ToLower(facets.Genres[j])
	})

	for _, r := range ratingSet {
		facets.ContentRatings = append(facets.ContentRatings, r)
	}
	sort.Slice(facets.ContentRatings, func(i, j int) bool {
		return strings.ToLower(facets.ContentRatings[i]) < strings.ToLower(facets.ContentRatings[j])
	})

	for y := range yearSet {
		facets.Years = append(facets.Years, y)
	}
	sort.Slice(facets.Years, func(i, j int) bool {
		return facets.Years[i] > facets.Years[j]
	})

	return facets, nil
}

// ChapterSourceItem holds the minimal fields required to generate chapter still thumbnails.
type ChapterSourceItem struct {
	ID           string
	Title        string
	FilePath     string
	FileMtime    int64
	VideoHDR     string
	ChaptersJSON string
}

// ListItemsWithChapters returns all indexed media items that have non-empty chapter metadata.
func (s *Store) ListItemsWithChapters(ctx context.Context) ([]ChapterSourceItem, error) {
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, title, file_path, file_mtime, video_hdr, chapters_json
		FROM media_items
		WHERE chapters_json != '' AND chapters_json != '[]'
		ORDER BY created_at DESC, id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChapterSourceItem
	for rows.Next() {
		var item ChapterSourceItem
		if err := rows.Scan(&item.ID, &item.Title, &item.FilePath, &item.FileMtime, &item.VideoHDR, &item.ChaptersJSON); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// ListAllMediaItemIDs returns the set of all live media_items IDs.
func (s *Store) ListAllMediaItemIDs(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.reader.QueryContext(ctx, `SELECT id FROM media_items`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ids := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids[id] = struct{}{}
	}
	return ids, rows.Err()
}

// HasIndexedPersonPhoto reports whether filename is referenced in any indexed media item's cast_json.
func (s *Store) HasIndexedPersonPhoto(ctx context.Context, filename string) (bool, error) {
	if filename == "" {
		return false, nil
	}
	var one int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM media_items WHERE instr(cast_json, ?) > 0 LIMIT 1
	`, filename).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func scanLibrary(row rowScanner) (Library, error) {
	var lib Library
	var createdStr, updatedStr string
	if err := row.Scan(&lib.ID, &lib.Name, &lib.Kind, &lib.RootPath, &lib.ItemCount, &createdStr, &updatedStr); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Library{}, ErrNotFound
		}
		return Library{}, err
	}
	lib.CreatedAt = parseTime(createdStr)
	lib.UpdatedAt = parseTime(updatedStr)
	return lib, nil
}

func scanMediaItem(row rowScanner) (MediaItem, error) {
	var m MediaItem
	var year, season, episode, absEpisode sql.NullInt64
	var probedStr sql.NullString
	var createdStr, updatedStr string
	if err := row.Scan(
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
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return MediaItem{}, ErrNotFound
		}
		return MediaItem{}, err
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
	return m, nil
}
