-- Share individual articles with a public URL (Miniflux style): a random
-- token enables a public read-only view of the item.

CREATE TABLE IF NOT EXISTS shared_items (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  item_id    INTEGER NOT NULL REFERENCES items(id) ON DELETE CASCADE,
  token      TEXT NOT NULL UNIQUE,
  created_at INTEGER NOT NULL DEFAULT (unixepoch()),
  UNIQUE(user_id, item_id)
);
