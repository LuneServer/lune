-- M3: Per-user playback progress & continue watching state

CREATE TABLE IF NOT EXISTS playback_progress (
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    item_id        TEXT NOT NULL REFERENCES media_items(id) ON DELETE CASCADE,
    position_ms    INTEGER NOT NULL DEFAULT 0,
    duration_ms    INTEGER NOT NULL DEFAULT 0,
    completed      INTEGER NOT NULL DEFAULT 0,
    play_count     INTEGER NOT NULL DEFAULT 0,
    last_played_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    PRIMARY KEY (user_id, item_id)
);

CREATE INDEX IF NOT EXISTS idx_playback_continue
    ON playback_progress(user_id, completed, last_played_at DESC);
