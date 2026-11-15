// Package store: unified SQLite opening (SPEC §5.2) and migration runner with
// PRAGMA user_version (port of the ocnews one, §5.3).
//
// Every module has its own .db file (D2) and its own domain store package on
// top of the *sql.DB returned by Open. user_version is per file, so the
// existing migration schemas keep working as is.
package store

import (
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // driver "sqlite" (pure Go, CGO_ENABLED=0)
)

// Open creates the parent directory (0700) if needed and opens the SQLite with
// the union of pragmas of the three services (SPEC §5.2): busy_timeout 10 s,
// WAL, synchronous NORMAL and foreign_keys ON. A single writer per file.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// modernc/sqlite writes better with 1 writer connection.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, nil
}

// Migrate applies the *.sql migrations of dir inside fsys (typically an
// embed.FS with the module migrations/), in name order, with a number greater
// than PRAGMA user_version. Each migration runs in its own transaction and
// sets user_version at the end of it, if it fails it rolls back and leaves
// user_version untouched.
//
// Migrations run with foreign_keys OFF: PRAGMA foreign_keys is a no-op inside
// a transaction, so it is disabled BEFORE the Begin and restored after the
// Commit. It is the standard SQLite practice for table rebuilds (H8, photos
// 002): with FK ON, the DROP TABLE of a rebuild runs an implicit DELETE that
// fires the ON DELETE CASCADE of the child tables (album_assets/asset_tags
// would lose their rows). Outside the migration transaction the DB keeps FK ON
// (Open DSN).
func Migrate(db *sql.DB, fsys fs.FS, dir string) error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read user_version: %w", err)
	}
	for _, name := range names {
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migration with an invalid name %q: %w", name, err)
		}
		if num <= version {
			continue
		}
		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return err
		}
		if err := migrateOne(db, num, string(body)); err != nil {
			return fmt.Errorf("migration %s: %w", name, err)
		}
	}
	return nil
}

// migrateOne runs one migration in its transaction with foreign_keys OFF (see
// Migrate). It always restores FK ON, also on error.
func migrateOne(db *sql.DB, num int, body string) error {
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disable foreign_keys: %w", err)
	}
	restore := func() {
		// best effort: if the DB is broken the error is reported elsewhere
		_, _ = db.Exec("PRAGMA foreign_keys = ON")
	}
	tx, err := db.Begin()
	if err != nil {
		restore()
		return err
	}
	if _, err := tx.Exec(body); err != nil {
		_ = tx.Rollback()
		restore()
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", num)); err != nil {
		_ = tx.Rollback()
		restore()
		return fmt.Errorf("set user_version=%d: %w", num, err)
	}
	if err := tx.Commit(); err != nil {
		restore()
		return err
	}
	restore()
	return nil
}

// Baseline sets PRAGMA user_version WITHOUT running SQL: adoption of live
// schemas created outside the runner (photos, §5.3). Verifying that the
// existing schema is the expected one is up to the module that adopts it.
func Baseline(db *sql.DB, version int) error {
	if version < 0 {
		return fmt.Errorf("baseline with a negative version: %d", version)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("baseline user_version=%d: %w", version, err)
	}
	return nil
}
