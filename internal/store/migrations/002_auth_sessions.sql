-- M1: Sessions and RFC 8628 device authorization codes

CREATE TABLE IF NOT EXISTS sessions (
    id                 TEXT PRIMARY KEY,
    user_id            TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash         TEXT NOT NULL UNIQUE,
    refresh_hash       TEXT UNIQUE,
    client_name        TEXT NOT NULL DEFAULT '',
    client_kind        TEXT NOT NULL CHECK (client_kind IN ('web', 'app')),
    ip_address         TEXT NOT NULL DEFAULT '',
    user_agent         TEXT NOT NULL DEFAULT '',
    expires_at         TEXT NOT NULL,
    refresh_expires_at TEXT,
    last_seen_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    revoked_at         TEXT,
    created_at         TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_sessions_user_id ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS device_codes (
    device_code_hash TEXT PRIMARY KEY,
    user_code        TEXT NOT NULL UNIQUE,
    client_name      TEXT NOT NULL DEFAULT '',
    client_ip        TEXT NOT NULL DEFAULT '',
    status           TEXT NOT NULL CHECK (status IN ('pending', 'approved', 'denied', 'consumed')) DEFAULT 'pending',
    approved_user_id TEXT REFERENCES users(id) ON DELETE CASCADE,
    expires_at       TEXT NOT NULL,
    created_at       TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

CREATE INDEX IF NOT EXISTS idx_device_codes_expires_at ON device_codes(expires_at);
