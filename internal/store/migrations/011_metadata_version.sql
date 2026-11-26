-- Track metadata enrichment schema version so incremental scans and startup backfill
-- automatically populate newly-added descriptive fields (cast, directors, writers, studios,
-- content rating, release date, IMDb IDs) for items indexed by older server versions or
-- before TMDB was configured.
ALTER TABLE media_items ADD COLUMN metadata_version INTEGER NOT NULL DEFAULT 0;

-- Mark items that already have rich credits/certification metadata as version 1 so
-- they are not unnecessarily re-queried after upgrading.
UPDATE media_items
SET metadata_version = 1
WHERE cast_json != '[]'
   OR directors_json != '[]'
   OR writers_json != '[]'
   OR studios_json != '[]'
   OR content_rating != ''
   OR release_date != '';

-- Extend the covering fingerprint index with metadata_version so ListLibraryFingerprints
-- remains an index-only scan at 100k+ item scale.
DROP INDEX IF EXISTS idx_media_items_fingerprints;
CREATE INDEX idx_media_items_fingerprints
    ON media_items(library_id, file_path, file_size, file_mtime, id, metadata_version);
