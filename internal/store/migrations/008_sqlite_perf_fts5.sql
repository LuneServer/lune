-- M8: SQLite high-scale optimizations (FTS5 full-text search + covering indexes for 100k+ files)

-- Covering index for incremental scans (ListLibraryFingerprints) so unchanged-file
-- lookups avoid touching wide table pages containing streams_json / overview blobs.
CREATE INDEX IF NOT EXISTS idx_media_items_fingerprints
    ON media_items(library_id, file_path, file_size, file_mtime, id);

-- Composite sort indexes for Home "Recently Added in <Library>" and dedicated library views.
CREATE INDEX IF NOT EXISTS idx_media_items_lib_recent
    ON media_items(library_id, created_at DESC, sort_title COLLATE NOCASE ASC);

CREATE INDEX IF NOT EXISTS idx_media_items_lib_year
    ON media_items(library_id, year DESC, sort_title COLLATE NOCASE ASC);

-- Composite series grouping & Next-Episode lookup index (ListSeries / ListSeriesEpisodes / GetNextEpisode).
CREATE INDEX IF NOT EXISTS idx_media_items_lib_kind_series
    ON media_items(library_id, kind, series_title COLLATE NOCASE, season_number, episode_number);

-- External-content FTS5 index for sub-5ms prefix & diacritic-insensitive search across 100k+ items.
CREATE VIRTUAL TABLE IF NOT EXISTS media_items_fts USING fts5(
    title,
    series_title,
    overview,
    tagline,
    content='media_items',
    content_rowid='rowid',
    tokenize='unicode61 remove_diacritics 2'
);

CREATE TRIGGER IF NOT EXISTS media_items_fts_ai AFTER INSERT ON media_items BEGIN
    INSERT INTO media_items_fts(rowid, title, series_title, overview, tagline)
    VALUES (new.rowid, new.title, new.series_title, new.overview, new.tagline);
END;

CREATE TRIGGER IF NOT EXISTS media_items_fts_ad AFTER DELETE ON media_items BEGIN
    INSERT INTO media_items_fts(media_items_fts, rowid, title, series_title, overview, tagline)
    VALUES ('delete', old.rowid, old.title, old.series_title, old.overview, old.tagline);
END;

CREATE TRIGGER IF NOT EXISTS media_items_fts_au AFTER UPDATE OF title, series_title, overview, tagline ON media_items BEGIN
    INSERT INTO media_items_fts(media_items_fts, rowid, title, series_title, overview, tagline)
    VALUES ('delete', old.rowid, old.title, old.series_title, old.overview, old.tagline);
    INSERT INTO media_items_fts(rowid, title, series_title, overview, tagline)
    VALUES (new.rowid, new.title, new.series_title, new.overview, new.tagline);
END;

-- Populate FTS5 from any rows indexed before this migration and refresh planner stats.
INSERT INTO media_items_fts(media_items_fts) VALUES('rebuild');
ANALYZE;
