-- M2: Native media catalog (movies, episodes, tracks, ffprobe streams, local artwork)

CREATE TABLE IF NOT EXISTS media_items (
    id             TEXT PRIMARY KEY,
    library_id     TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    kind           TEXT NOT NULL CHECK (kind IN ('movie', 'episode', 'track', 'other')),
    title          TEXT NOT NULL,
    sort_title     TEXT NOT NULL,
    year           INTEGER,
    series_title   TEXT NOT NULL DEFAULT '',
    season_number  INTEGER,
    episode_number INTEGER,
    file_path      TEXT NOT NULL UNIQUE,
    file_size      INTEGER NOT NULL DEFAULT 0,
    file_mtime     INTEGER NOT NULL DEFAULT 0,
    container      TEXT NOT NULL DEFAULT '',
    duration_ms    INTEGER NOT NULL DEFAULT 0,
    bitrate        INTEGER NOT NULL DEFAULT 0,
    video_codec    TEXT NOT NULL DEFAULT '',
    video_width    INTEGER NOT NULL DEFAULT 0,
    video_height   INTEGER NOT NULL DEFAULT 0,
    video_hdr      TEXT NOT NULL DEFAULT '',
    audio_codec    TEXT NOT NULL DEFAULT '',
    audio_channels INTEGER NOT NULL DEFAULT 0,
    streams_json   TEXT NOT NULL DEFAULT '[]',
    poster_path    TEXT NOT NULL DEFAULT '',
    backdrop_path  TEXT NOT NULL DEFAULT '',
    probed_at      TEXT,
    created_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at     TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_media_items_library_sort ON media_items(library_id, sort_title);
CREATE INDEX IF NOT EXISTS idx_media_items_series ON media_items(series_title, season_number, episode_number);
CREATE INDEX IF NOT EXISTS idx_media_items_created ON media_items(created_at DESC);
