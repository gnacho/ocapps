-- Auto-mark as read on arrival: per-user rules (feed_id 0 = all)
-- that set unread=0 on new items whose title matches the regex pattern.

CREATE TABLE IF NOT EXISTS auto_read (
  id            INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  feed_id       INTEGER NOT NULL DEFAULT 0,
  title_pattern TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_auto_read_user ON auto_read(user_id);
