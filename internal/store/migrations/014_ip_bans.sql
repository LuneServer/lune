-- Persist active automatic and manual edge IP bans across restarts.
CREATE TABLE IF NOT EXISTS ip_bans (
    ip         TEXT PRIMARY KEY,
    reason     TEXT NOT NULL DEFAULT '',
    banned_at  TEXT NOT NULL,
    expires_at TEXT,
    permanent  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_ip_bans_expires_at ON ip_bans(expires_at);
