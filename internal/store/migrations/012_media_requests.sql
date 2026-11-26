-- Native Media Requests: per-user request permissions and media_requests table.
ALTER TABLE users ADD COLUMN can_request INTEGER NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN auto_approve_requests INTEGER NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS media_requests (
    id               TEXT PRIMARY KEY,
    user_id          TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    media_type       TEXT NOT NULL CHECK (media_type IN ('movie','tv')),
    is_anime         INTEGER NOT NULL DEFAULT 0,
    tmdb_id          INTEGER NOT NULL,
    tvdb_id          INTEGER NOT NULL DEFAULT 0,
    title            TEXT NOT NULL,
    original_title   TEXT NOT NULL DEFAULT '',
    year             INTEGER,
    overview         TEXT NOT NULL DEFAULT '',
    poster_path      TEXT NOT NULL DEFAULT '',
    backdrop_path    TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL CHECK (status IN ('pending','approved','processing','available','declined','failed')),
    status_note      TEXT NOT NULL DEFAULT '',
    external_service TEXT NOT NULL DEFAULT '',
    external_id      INTEGER NOT NULL DEFAULT 0,
    reviewed_by      TEXT REFERENCES users(id) ON DELETE SET NULL,
    reviewed_at      TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_media_requests_active_unique
    ON media_requests(media_type, tmdb_id)
    WHERE status IN ('pending','approved','processing');

CREATE INDEX IF NOT EXISTS idx_media_requests_user
    ON media_requests(user_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_media_requests_status
    ON media_requests(status, created_at DESC);
