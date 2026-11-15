-- Persisted per-user settings (own extension, the v1.3 spec does not define them):
-- key-value table per user for app preferences (reader theme,
-- refresh interval, UI language, etc.). language lives in users.*, this
-- covers the rest without migrating columns for every setting.

CREATE TABLE IF NOT EXISTS user_settings (
  user_id INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  key     TEXT NOT NULL,
  value   TEXT NOT NULL,
  PRIMARY KEY (user_id, key)
);
