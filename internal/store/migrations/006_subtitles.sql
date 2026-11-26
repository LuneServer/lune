-- M6: Local sidecar subtitles metadata

ALTER TABLE media_items ADD COLUMN subtitles_json TEXT NOT NULL DEFAULT '[]';
