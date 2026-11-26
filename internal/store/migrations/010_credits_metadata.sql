-- Rich descriptive credits & certification metadata for Movies, TV Shows, and Episodes.
ALTER TABLE media_items ADD COLUMN content_rating TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN release_date   TEXT NOT NULL DEFAULT '';
ALTER TABLE media_items ADD COLUMN directors_json TEXT NOT NULL DEFAULT '[]';
ALTER TABLE media_items ADD COLUMN writers_json   TEXT NOT NULL DEFAULT '[]';
ALTER TABLE media_items ADD COLUMN studios_json   TEXT NOT NULL DEFAULT '[]';
ALTER TABLE media_items ADD COLUMN cast_json      TEXT NOT NULL DEFAULT '[]';
