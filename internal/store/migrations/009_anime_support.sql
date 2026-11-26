-- M9: First-class Anime support (anime library kind, original titles, absolute episode numbers,
-- release groups, MKV chapters, normalized audio languages, and FTS5 original_title indexing)

-- 1. Rebuild libraries table to add 'anime' to the kind CHECK constraint.
-- Store.migrate runs with PRAGMA foreign_keys = OFF outside the tx and validates PRAGMA foreign_key_check after.
CREATE TABLE libraries_new (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL CHECK (kind IN ('movies', 'shows', 'anime', 'music', 'other')),
    root_path  TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now')),
    updated_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ', 'now'))
);

INSERT INTO libraries_new (id, name, kind, root_path, created_at, updated_at)
SELECT id, name, kind, root_path, created_at, updated_at FROM libraries;

DROP TABLE libraries;
ALTER TABLE libraries_new RENAME TO libraries;

-- 2. Extend media_items with first-class anime & rich playback columns.
ALTER TABLE media_items ADD COLUMN original_title          TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN absolute_episode_number INTEGER;
ALTER TABLE media_items ADD COLUMN release_group           TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN chapters_json           TEXT NOT NULL DEFAULT '[]';
ALTER TABLE media_items ADD COLUMN audio_languages         TEXT NOT NULL DEFAULT '';

-- 3. Rebuild FTS5 virtual table and triggers to also index original_title (Romaji / Japanese native titles).
DROP TRIGGER IF EXISTS media_items_fts_ai;
DROP TRIGGER IF EXISTS media_items_fts_ad;
DROP TRIGGER IF EXISTS media_items_fts_au;
DROP TABLE IF EXISTS media_items_fts;

CREATE VIRTUAL TABLE media_items_fts USING fts5(
    title,
    original_title,
    series_title,
    overview,
    tagline,
    content='media_items',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER media_items_fts_ai AFTER INSERT ON media_items BEGIN
    INSERT INTO media_items_fts(rowid, title, original_title, series_title, overview, tagline)
    VALUES (new.rowid, new.title, new.original_title, new.series_title, new.overview, new.tagline);
END;

CREATE TRIGGER media_items_fts_ad AFTER DELETE ON media_items BEGIN
    INSERT INTO media_items_fts(media_items_fts, rowid, title, original_title, series_title, overview, tagline)
    VALUES ('delete', old.rowid, old.title, old.original_title, old.series_title, old.overview, old.tagline);
END;

CREATE TRIGGER media_items_fts_au AFTER UPDATE OF title, original_title, series_title, overview, tagline ON media_items BEGIN
    INSERT INTO media_items_fts(media_items_fts, rowid, title, original_title, series_title, overview, tagline)
    VALUES ('delete', old.rowid, old.title, old.original_title, old.series_title, old.overview, old.tagline);
    INSERT INTO media_items_fts(rowid, title, original_title, series_title, overview, tagline)
    VALUES (new.rowid, new.title, new.original_title, new.series_title, new.overview, new.tagline);
END;

INSERT INTO media_items_fts(media_items_fts) VALUES('rebuild');
ANALYZE;
