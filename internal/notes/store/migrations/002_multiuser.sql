-- 002_multiuser: add the user dimension. Notes gain a user column; settings is
-- rebuilt with a composite (user, key) primary key. Existing rows keep an empty
-- user and are backfilled to the configured owner after all migrations run.
ALTER TABLE notes ADD COLUMN user TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_notes_user ON notes(user);
CREATE INDEX IF NOT EXISTS idx_notes_user_category ON notes(user, category);
CREATE INDEX IF NOT EXISTS idx_notes_user_modified ON notes(user, modified);

CREATE TABLE settings_new (
    user TEXT NOT NULL DEFAULT '',
    key TEXT NOT NULL DEFAULT '',
    value TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (user, key)
);

INSERT INTO settings_new (user, key, value)
    SELECT '', key, value FROM settings;

DROP TABLE settings;

ALTER TABLE settings_new RENAME TO settings;
