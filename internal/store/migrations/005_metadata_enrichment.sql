-- M5: Rich metadata fields (local NFO + optional TMDB enrichment)

ALTER TABLE media_items ADD COLUMN overview    TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN tagline     TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN rating      REAL NOT NULL DEFAULT 0;
ALTER TABLE media_items ADD COLUMN genres_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE media_items ADD COLUMN tmdb_id     TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN imdb_id     TEXT NOT NULL DEFAULT '';
