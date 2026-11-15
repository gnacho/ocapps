-- F1: full body extracted from the original article (readability),
-- feeds usually only carry the summary.

CREATE TABLE item_full (
  item_id    INTEGER PRIMARY KEY REFERENCES items(id) ON DELETE CASCADE,
  body       TEXT NOT NULL,
  fetched_at INTEGER NOT NULL
);
