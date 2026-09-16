// Package store: store de dominio de notes sobre el *sql.DB común
// (SPEC §5.3). La apertura del fichero SQLite y los pragmas viven en
// internal/common/store; aquí quedan el runner de adopción (backup
// pre-migración + backfill de owner) y las operaciones de notas/settings.
package store

import (
	"database/sql"
	"fmt"
)

func scanNote(row dbRowScanner) (*Note, error) {
	var n Note
	var favInt int
	err := row.Scan(&n.ID, &n.Etag, &n.Modified, &n.Title, &n.Category, &n.Content, &favInt)
	n.Favorite = favInt == 1
	return &n, err
}

type dbRowScanner interface {
	Scan(dest ...interface{}) error
}

// Open migra la base de datos (con backup pre-migración verificado y backfill
// del owner, SPEC §5.3) y devuelve el store de dominio sobre el *sql.DB
// abierto por common/store.Open. dbPath es la ruta del fichero notes.db, solo
// se usa para nombrar el backup notes.db.<ts>.bak. El *sql.DB NO se cierra en
// caso de error: su ciclo de vida es del llamador.
func Open(db *sql.DB, dbPath, owner string) (*Store, error) {
	if err := migrate(db, dbPath, owner); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}
