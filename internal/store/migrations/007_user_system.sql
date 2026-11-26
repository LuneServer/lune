-- M7: Full user system (profiles, preferences, avatars, library ACLs, invites, watchlist/favorites)

ALTER TABLE users ADD COLUMN all_libraries    INTEGER NOT NULL DEFAULT 1;
ALTER TABLE users ADD COLUMN avatar_path      TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN preferences_json TEXT NOT NULL DEFAULT '{}';
ALTER TABLE users ADD COLUMN last_login_at    TEXT;

CREATE TABLE IF NOT EXISTS user_library_access (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    library_id TEXT NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (user_id, library_id)
);

CREATE INDEX IF NOT EXISTS idx_user_library_access_library
    ON user_library_access(library_id);

CREATE TABLE IF NOT EXISTS user_invites (
    id               TEXT PRIMARY KEY,
    token_hash       TEXT NOT NULL UNIQUE,
    token_prefix     TEXT NOT NULL DEFAULT '',
    label            TEXT NOT NULL DEFAULT '',
    role             TEXT NOT NULL CHECK (role IN ('admin', 'member')) DEFAULT 'member',
    all_libraries    INTEGER NOT NULL DEFAULT 1,
    library_ids_json TEXT NOT NULL DEFAULT '[]',
    max_uses         INTEGER NOT NULL DEFAULT 1,
    use_count        INTEGER NOT NULL DEFAULT 0,
    created_by       TEXT REFERENCES users(id) ON DELETE SET NULL,
    expires_at       TEXT NOT NULL,
    revoked_at       TEXT,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_user_invites_expires
    ON user_invites(expires_at);

CREATE TABLE IF NOT EXISTS user_media_marks (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id    TEXT NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('watchlist', 'favorite')),
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (user_id, item_id, kind)
);

CREATE INDEX IF NOT EXISTS idx_user_media_marks_user_kind
    ON user_media_marks(user_id, kind, created_at DESC);
