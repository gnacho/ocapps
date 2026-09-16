package store

import (
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	commonstore "github.com/gnacho/ocapps/internal/common/store"
)

// migrationsFS embeds the numbered, immutable schema migrations. PRAGMA
// user_version is the single source of truth for the applied schema state.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// migrate applies any pending migrations via the common runner, taking a
// verified pre-migration backup first, and then backfills rows that still
// have an empty owner. It must run before the HTTP server starts.
func migrate(db *sql.DB, dbPath, owner string) error {
	version, err := currentVersion(db)
	if err != nil {
		return err
	}

	pending, err := pendingMigrations(version)
	if err != nil {
		return err
	}

	if len(pending) > 0 {
		if err := backup(db, dbPath); err != nil {
			return err
		}
		if err := commonstore.Migrate(db, migrationsFS, "migrations"); err != nil {
			return err
		}
	}

	return backfill(db, owner)
}

func currentVersion(db *sql.DB) (int, error) {
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		return 0, fmt.Errorf("read user_version: %w", err)
	}
	return v, nil
}

// pendingMigrations lists the embedded migration files whose version is
// greater than the current user_version, in application order.
func pendingMigrations(version int) ([]string, error) {
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(files)

	var pending []string
	for _, f := range files {
		v, err := migrationVersion(f)
		if err != nil {
			return nil, err
		}
		if v > version {
			pending = append(pending, f)
		}
	}
	return pending, nil
}

// migrationVersion parses the leading integer of a migration file name
// (e.g. "migrations/002_multiuser.sql" -> 2).
func migrationVersion(name string) (int, error) {
	base := strings.TrimSuffix(filepath.Base(name), ".sql")
	i := strings.IndexByte(base, '_')
	if i <= 0 {
		return 0, fmt.Errorf("invalid migration name %q", name)
	}
	return strconv.Atoi(base[:i])
}

// backup writes a consistent snapshot next to the database with VACUUM INTO
// (never a hot copy) and verifies it with quick_check before any migration runs.
func backup(db *sql.DB, dbPath string) error {
	path := filepath.Join(filepath.Dir(dbPath), fmt.Sprintf("notes.db.%s.bak", time.Now().UTC().Format("20060102T150405.000000")))
	if _, err := db.Exec(fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(path, "'", "''"))); err != nil {
		return fmt.Errorf("pre-migration backup: %w", err)
	}
	if err := verifyBackup(path); err != nil {
		return fmt.Errorf("verify pre-migration backup: %w", err)
	}
	slog.Info("pre-migration backup created", "path", path)
	return nil
}

func verifyBackup(path string) error {
	bdb, err := sql.Open("sqlite", path)
	if err != nil {
		return err
	}
	defer bdb.Close()
	var result string
	if err := bdb.QueryRow(`PRAGMA quick_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("quick_check returned %q", result)
	}
	return nil
}

// backfill assigns rows that still have an empty user to the configured owner.
// It refuses to start if such rows exist and no owner is configured, so that
// pre-multiuser rows are never left invisible.
func backfill(db *sql.DB, owner string) error {
	var notes, settings int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notes WHERE user = ''`).Scan(&notes); err != nil {
		return fmt.Errorf("count unowned notes: %w", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM settings WHERE user = ''`).Scan(&settings); err != nil {
		return fmt.Errorf("count unowned settings: %w", err)
	}
	if notes == 0 && settings == 0 {
		return nil
	}
	if owner == "" {
		return fmt.Errorf("database has %d notes and %d settings rows without an owner; set OCAPPS_NOTES_OWNER (legacy OCNOTES_OWNER) to the owner's graph id (UUID) before starting (refusing to leave rows invisible)", notes, settings)
	}
	if _, err := db.Exec(`UPDATE notes SET user = ? WHERE user = ''`, owner); err != nil {
		return fmt.Errorf("backfill notes: %w", err)
	}
	if _, err := db.Exec(`UPDATE settings SET user = ? WHERE user = ''`, owner); err != nil {
		return fmt.Errorf("backfill settings: %w", err)
	}
	slog.Info("backfilled owner rows", "notes", notes, "settings", settings, "owner_id", owner)
	return nil
}

// splitStatements splits a SQL file into individual statements, stripping line
// (--) and block (/* */) comments and dropping empty/comment-only chunks.
// Statements must not contain ';' inside string literals (true for this
// package's migration files).
func splitStatements(sqlText string) []string {
	sqlText = stripComments(sqlText)
	stmts := strings.Split(sqlText, ";")
	result := make([]string, 0, len(stmts))
	for _, s := range stmts {
		s = strings.TrimSpace(s)
		if s != "" {
			result = append(result, s)
		}
	}
	return result
}

func stripComments(sqlText string) string {
	var out strings.Builder
	out.Grow(len(sqlText))
	for i := 0; i < len(sqlText); {
		if i+1 < len(sqlText) && sqlText[i] == '-' && sqlText[i+1] == '-' {
			for i < len(sqlText) && sqlText[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(sqlText) && sqlText[i] == '/' && sqlText[i+1] == '*' {
			i += 2
			for i+1 < len(sqlText) && !(sqlText[i] == '*' && sqlText[i+1] == '/') {
				i++
			}
			if i+1 < len(sqlText) {
				i += 2
			}
			continue
		}
		out.WriteByte(sqlText[i])
		i++
	}
	return out.String()
}
