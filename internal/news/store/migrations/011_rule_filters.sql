-- Advanced block/keep regex filters (Miniflux style): feed_rules per
-- feed and global_rules per user. Multiline "Field=regex" format with
-- the EntryTitle/EntryURL/EntryAuthor/EntryContent/EntryDate fields.

CREATE TABLE IF NOT EXISTS feed_rules (
  feed_id INTEGER PRIMARY KEY REFERENCES feeds(id) ON DELETE CASCADE,
  block   TEXT NOT NULL DEFAULT '',
  keep    TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS global_rules (
  user_id INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
  block   TEXT NOT NULL DEFAULT '',
  keep    TEXT NOT NULL DEFAULT ''
);
