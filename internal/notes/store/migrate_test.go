package store

import (
	"database/sql"
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
)

// legacySchema is the pre-multiuser schema (user_version 0): notes with no
// user column and settings keyed by a single key primary key, with default
// rows already present.
const legacySchema = `CREATE TABLE notes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    title TEXT NOT NULL DEFAULT 'New note',
    content TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT '',
    favorite INTEGER NOT NULL DEFAULT 0,
    modified INTEGER NOT NULL,
    etag TEXT NOT NULL
);
CREATE INDEX idx_notes_category ON notes(category);
CREATE INDEX idx_notes_favorite ON notes(favorite);
CREATE INDEX idx_notes_modified ON notes(modified);
CREATE TABLE settings (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL DEFAULT ''
);
INSERT INTO settings (key, value) VALUES ('notesPath', 'Legacy');
INSERT INTO settings (key, value) VALUES ('fileSuffix', '.txt');`

func openRawDB(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, path
}

func latestVersion(t *testing.T) int {
	t.Helper()
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatalf("glob migrations: %v", err)
	}
	max := 0
	for _, f := range files {
		v, err := migrationVersion(f)
		if err != nil {
			t.Fatalf("migrationVersion(%s): %v", f, err)
		}
		if v > max {
			max = v
		}
	}
	return max
}

func TestMigrateBackfillsLegacyRows(t *testing.T) {
	db, path := openRawDB(t)
	for _, stmt := range splitStatements(legacySchema) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply legacy schema: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO notes (title, content, category, favorite, modified, etag) VALUES ('legacy', 'body', 'cat', 0, 1, '1')`); err != nil {
		t.Fatalf("insert legacy note: %v", err)
	}

	const owner = "owner-id"
	if err := migrate(db, path, owner); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	// user_version advanced to the latest migration.
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != latestVersion(t) {
		t.Fatalf("user_version = %d, want %d", v, latestVersion(t))
	}

	// Notes backfilled to the owner, none left unowned.
	var unowned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notes WHERE user = ''`).Scan(&unowned); err != nil {
		t.Fatalf("count unowned notes: %v", err)
	}
	if unowned != 0 {
		t.Fatalf("%d unowned notes remain", unowned)
	}
	var owned int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notes WHERE user = ?`, owner).Scan(&owned); err != nil {
		t.Fatalf("count owned notes: %v", err)
	}
	if owned != 1 {
		t.Fatalf("owned notes = %d, want 1", owned)
	}

	// Settings backfilled and primary key now composite (user, key).
	var unownedSettings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings WHERE user = ''`).Scan(&unownedSettings); err != nil {
		t.Fatalf("count unowned settings: %v", err)
	}
	if unownedSettings != 0 {
		t.Fatalf("%d unowned settings remain", unownedSettings)
	}
	var ddl string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='settings'`).Scan(&ddl); err != nil {
		t.Fatalf("settings ddl: %v", err)
	}
	if !strings.Contains(ddl, "PRIMARY KEY (user, key)") {
		t.Fatalf("settings primary key not composite; ddl = %s", ddl)
	}
	var val string
	if err := db.QueryRow(`SELECT value FROM settings WHERE user = ? AND key = 'notesPath'`, owner).Scan(&val); err != nil {
		t.Fatalf("settings value: %v", err)
	}
	if val != "Legacy" {
		t.Fatalf("settings notesPath = %q, want Legacy", val)
	}
}

func TestFreshDatabaseReachesLatestVersion(t *testing.T) {
	s := openTestStore(t)
	var v int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != latestVersion(t) {
		t.Fatalf("fresh user_version = %d, want %d", v, latestVersion(t))
	}
}

func TestMigrateFailsFastWithoutOwner(t *testing.T) {
	db, path := openRawDB(t)
	for _, stmt := range splitStatements(legacySchema) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("apply legacy schema: %v", err)
		}
	}
	if _, err := db.Exec(`INSERT INTO notes (title, content, category, favorite, modified, etag) VALUES ('legacy', 'body', 'cat', 0, 1, '1')`); err != nil {
		t.Fatalf("insert legacy note: %v", err)
	}

	err := migrate(db, path, "")
	if err == nil {
		t.Fatal("migrate with no owner and legacy rows succeeded, want fail-fast error")
	}
	if !strings.Contains(err.Error(), "OCNOTES_OWNER") {
		t.Fatalf("error = %q, want it to mention OCNOTES_OWNER", err)
	}
}

func TestSecondOpenIsNoOp(t *testing.T) {
	dir := t.TempDir()

	s1, err := openStoreAt(t, dir)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_ = s1.Close()

	before := backupFiles(t, dir)

	s2, err := openStoreAt(t, dir)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer s2.Close()

	var v int
	if err := s2.db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	if v != latestVersion(t) {
		t.Fatalf("second open user_version = %d, want %d", v, latestVersion(t))
	}

	after := backupFiles(t, dir)
	if len(after) != len(before) {
		t.Fatalf("second Open took a backup; before=%d files after=%d files", len(before), len(after))
	}
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "notes.db.*.bak"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}
