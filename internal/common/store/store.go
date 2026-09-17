// Package store: apertura SQLite unificada (SPEC §5.2) y runner de
// migraciones con PRAGMA user_version (port del de ocnews, §5.3).
//
// Cada módulo tiene su propio fichero .db (D2) y su propio paquete store de
// dominio encima del *sql.DB que devuelve Open. user_version es por fichero,
// así que los esquemas de migración existentes siguen funcionando tal cual.
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

	_ "modernc.org/sqlite" // driver "sqlite" (Go puro, CGO_ENABLED=0)
)

// Open crea el directorio padre (0700) si hace falta y abre la SQLite con la
// unión de pragmas de los tres servicios (SPEC §5.2): busy_timeout 10 s,
// WAL, synchronous NORMAL y foreign_keys ON. Un solo escritor por fichero.
func Open(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("crear data dir: %w", err)
		}
	}
	dsn := "file:" + url.PathEscape(path) +
		"?_pragma=busy_timeout(10000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("abrir sqlite: %w", err)
	}
	// modernc/sqlite escribe mejor con 1 conexión escritora.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("abrir sqlite: %w", err)
	}
	return db, nil
}

// Migrate aplica las migraciones *.sql de dir dentro de fsys (típicamente un
// embed.FS con migrations/ del módulo), en orden de nombre, con número mayor
// que PRAGMA user_version. Cada migración corre en su propia transacción y
// fija user_version al final de ella; si falla, hace rollback y deja
// user_version intacto.
//
// Las migraciones corren con foreign_keys OFF: PRAGMA foreign_keys es no-op
// dentro de una transacción, así que se desactiva ANTES del Begin y se
// restaura tras el Commit. Es la práctica estándar de SQLite para rebuilds
// de tabla (H8, photos 002): con FK ON, el DROP TABLE de un rebuild ejecuta
// un DELETE implícito que dispara los ON DELETE CASCADE de las tablas hija
// (album_assets/asset_tags perderían sus filas). Fuera de la transacción de
// migración la BD sigue con FK ON (DSN de Open).
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
		return fmt.Errorf("leer user_version: %w", err)
	}
	for _, name := range names {
		num, err := strconv.Atoi(strings.SplitN(name, "_", 2)[0])
		if err != nil {
			return fmt.Errorf("migración con nombre inválido %q: %w", name, err)
		}
		if num <= version {
			continue
		}
		body, err := fs.ReadFile(fsys, dir+"/"+name)
		if err != nil {
			return err
		}
		if err := migrateOne(db, num, string(body)); err != nil {
			return fmt.Errorf("migración %s: %w", name, err)
		}
	}
	return nil
}

// migrateOne ejecuta una migración en su transacción con foreign_keys OFF
// (ver Migrate). Restaura FK ON siempre, también en error.
func migrateOne(db *sql.DB, num int, body string) error {
	if _, err := db.Exec("PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("desactivar foreign_keys: %w", err)
	}
	restore := func() {
		// best effort: si la BD está rota el error ya se reporta por otro lado
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
		return fmt.Errorf("fijar user_version=%d: %w", num, err)
	}
	if err := tx.Commit(); err != nil {
		restore()
		return err
	}
	restore()
	return nil
}

// Baseline fija PRAGMA user_version SIN ejecutar SQL: adopción de esquemas
// vivos creados fuera del runner (photos, §5.3). La verificación de que el
// esquema existente es el esperado corresponde al módulo que la adopta.
func Baseline(db *sql.DB, version int) error {
	if version < 0 {
		return fmt.Errorf("baseline con versión negativa: %d", version)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", version)); err != nil {
		return fmt.Errorf("baseline user_version=%d: %w", version, err)
	}
	return nil
}
