-- Per-feed keyword filtering (News 28.4.0): feed_filter stores the
-- keywords (title/body/url) that hide the matching articles, items
-- gets the filtered=1 column for the ones the filter discards.

CREATE TABLE IF NOT EXISTS feed_filter (
  feed_id        INTEGER PRIMARY KEY,
  title_keywords TEXT NOT NULL DEFAULT '',
  body_keywords  TEXT NOT NULL DEFAULT '',
  url_keywords   TEXT NOT NULL DEFAULT ''
);

ALTER TABLE items ADD COLUMN filtered INTEGER NOT NULL DEFAULT 0;
