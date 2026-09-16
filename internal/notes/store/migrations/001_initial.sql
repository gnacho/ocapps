-- 001_initial: baseline schema (the pre-multiuser state). Idempotent so it is
-- a no-op on an existing database. Per-user settings defaults are seeded lazily
-- in code, not here.
CREATE TABLE IF NOT EXISTS notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL DEFAULT 'New note',
    content TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT '',
    favorite INTEGER NOT NULL DEFAULT 0,
    modified INTEGER NOT NULL,
    etag TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_notes_category ON notes(category);
CREATE INDEX IF NOT EXISTS idx_notes_favorite ON notes(favorite);
CREATE INDEX IF NOT EXISTS idx_notes_modified ON notes(modified);

CREATE TABLE IF NOT EXISTS settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
