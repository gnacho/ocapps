-- Podcast detection: a feed with items that bring audio/video enclosures
-- is flagged is_podcast=1 to list it in the Podcasts view of the extension.

ALTER TABLE feeds ADD COLUMN is_podcast INTEGER NOT NULL DEFAULT 0;
