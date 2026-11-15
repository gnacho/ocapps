// Package store: domain store of the news module on top of the shared *sql.DB
// (SPEC §2.1/§5). Opening (unified pragmas) and the migrations runner
// live in internal/common/store, here only the embed of
// the 18 news migrations and the domain operations remain. The live DB comes
// with user_version=18 -> Migrate is a no-op (SPEC §5.3).
package store

import (
	"context"
	"database/sql"
	"embed"
	"time"

	commonstore "github.com/gnacho/ocapps/internal/common/store"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

type Store struct {
	db *sql.DB
}

// Open opens SQLite with the unified pragmas (common/store.Open:
// busy_timeout 10 s, WAL, synchronous NORMAL, foreign_keys ON) and applies the
// pending migrations with the common runner. A single writer per file.
func Open(path string) (*Store, error) {
	db, err := commonstore.Open(path)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db}
	if err := commonstore.Migrate(db, migrationsFS, "migrations"); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Ping checks that the DB is still alive (module health, SPEC §4.5).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func now() int64 { return time.Now().Unix() }
