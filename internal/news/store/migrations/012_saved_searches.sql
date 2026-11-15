-- User saved searches: a persisted query shown in the
-- sidebar as a virtual feed (FreshRSS user queries / tt-rss
-- generated feeds style). It is not a real subscription.

CREATE TABLE IF NOT EXISTS saved_searches (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  query      TEXT NOT NULL,
  created_at INTEGER NOT NULL DEFAULT (unixepoch())
);

CREATE INDEX IF NOT EXISTS idx_saved_searches_user ON saved_searches(user_id);
