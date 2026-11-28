-- Cached IntroDB (introdb.app) segment lookups, keyed by "<imdb_id>:<season>:<episode>" or "<imdb_id>:movie".
CREATE TABLE IF NOT EXISTS introdb_cache (
    lookup_key TEXT PRIMARY KEY,
    payload    TEXT    NOT NULL,
    fetched_at INTEGER NOT NULL
);
