// Package store: domain store of notes on top of the shared *sql.DB
// (SPEC §5.3). Opening the SQLite file and the pragmas live in
// internal/common/store. Only the adoption runner (pre-migration backup plus
// owner backfill) and the notes/settings operations remain here.
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

// Open migrates the database (with a verified pre-migration backup and owner
// backfill, SPEC §5.3) and returns the domain store on top of the *sql.DB
// opened by common/store.Open. dbPath is the path of the notes.db file, only
// used to name the notes.db.<ts>.bak backup. The *sql.DB is NOT closed on
// error: its lifecycle belongs to the caller.
func Open(db *sql.DB, dbPath, owner string) (*Store, error) {
	if err := migrate(db, dbPath, owner); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}
