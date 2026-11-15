-- Full extraction with a per-feed CSS selector (#39): when the site
-- resists readability, the feed selector is used as the target before
-- giving up.

ALTER TABLE feeds ADD COLUMN scraper_selector TEXT NOT NULL DEFAULT '';
