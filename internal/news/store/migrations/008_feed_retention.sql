-- Per-feed retention (own extension): override of the global OCNEWS_RETENTION_DAYS.
-- 0 = use the global retention, NULL is not used (we prefer 0 for clarity).

ALTER TABLE feeds ADD COLUMN retention_days INTEGER NOT NULL DEFAULT 0;
