// Package store: store de dominio del módulo news sobre el *sql.DB común
// (SPEC §2.1/§5). La apertura (pragmas unificados) y el runner de
// migraciones viven en internal/common/store; aquí solo quedan el embed de
// las 18 migraciones de news y las operaciones de dominio. La BD viva llega
// con user_version=18 → Migrate es no-op (SPEC §5.3).
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

// Open abre la SQLite con los pragmas unificados (common/store.Open:
// busy_timeout 10 s, WAL, synchronous NORMAL, foreign_keys ON) y aplica las
// migraciones pendientes con el runner común. Un solo escritor por fichero.
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

// Ping verifica que la BD sigue viva (health del módulo, SPEC §4.5).
func (s *Store) Ping(ctx context.Context) error {
	return s.db.PingContext(ctx)
}

func now() int64 { return time.Now().Unix() }
