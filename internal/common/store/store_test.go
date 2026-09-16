package store

import (
	"database/sql"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatalf("user_version: %v", err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		"SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?", name).Scan(&n); err != nil {
		t.Fatalf("sqlite_master: %v", err)
	}
	return n == 1
}

func open(t *testing.T) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mod", "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db, path
}

func TestOpenPragmas(t *testing.T) {
	db, _ := open(t)
	defer db.Close()
	for pragma, want := range map[string]string{
		"journal_mode": "wal",
		"synchronous":  "1", // NORMAL
		"foreign_keys": "1",
		"busy_timeout": "10000",
	} {
		var got string
		if err := db.QueryRow("PRAGMA " + pragma).Scan(&got); err != nil {
			t.Fatalf("%s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("%s: got %q, want %q", pragma, got, want)
		}
	}
}

func TestMigrateOrdenYUserVersion(t *testing.T) {
	db, _ := open(t)
	defer db.Close()

	fsys := fstest.MapFS{
		"migrations/002_segunda.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE dos (id INTEGER PRIMARY KEY);
			 INSERT INTO dos (id) VALUES (2);`)},
		"migrations/001_primera.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE uno (id INTEGER PRIMARY KEY);
			 INSERT INTO uno (id) VALUES (1);`)},
		"migrations/003_tercera.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE tres (id INTEGER PRIMARY KEY);
			 INSERT INTO tres (id) SELECT id FROM uno;
			 INSERT INTO tres (id) SELECT id FROM dos;`)},
		"migrations/notas.txt": &fstest.MapFile{Data: []byte("no es SQL")},
	}

	if err := Migrate(db, fsys, "migrations"); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if v := userVersion(t, db); v != 3 {
		t.Fatalf("user_version: got %d, want 3", v)
	}
	// la 003 depende de uno y dos → prueba que el orden fue 001, 002, 003
	var n int
	if err := db.QueryRow("SELECT count(*) FROM tres").Scan(&n); err != nil || n != 2 {
		t.Fatalf("tres: n=%d err=%v (¿orden incorrecto?)", n, err)
	}
}

func TestMigrateReaperturaNoOp(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m.db")
	fsys := fstest.MapFS{
		"migrations/001_uno.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE uno (id INTEGER PRIMARY KEY);`)},
	}

	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Migrate(db, fsys, "migrations"); err != nil {
		t.Fatalf("primera: %v", err)
	}
	db.Close()

	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	if err := Migrate(db2, fsys, "migrations"); err != nil {
		t.Fatalf("re-apertura: %v", err)
	}
	if v := userVersion(t, db2); v != 1 {
		t.Fatalf("user_version tras re-apertura: %d", v)
	}
}

func TestMigrateRollbackEnDefectuosa(t *testing.T) {
	db, _ := open(t)
	defer db.Close()

	good := fstest.MapFS{
		"migrations/001_ok.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE uno (id INTEGER PRIMARY KEY);`)},
	}
	if err := Migrate(db, good, "migrations"); err != nil {
		t.Fatal(err)
	}

	bad := fstest.MapFS{
		"migrations/001_ok.sql":  good["migrations/001_ok.sql"],
		"migrations/002_mal.sql": &fstest.MapFile{Data: []byte(`CREATE TABL roto (`)},
	}
	err := Migrate(db, bad, "migrations")
	if err == nil {
		t.Fatal("esperaba error en migración defectuosa")
	}
	if v := userVersion(t, db); v != 1 {
		t.Fatalf("user_version tras rollback: got %d, want 1 (intacto)", v)
	}
	// la transacción de 002 quedó revertida: la BD sigue usable
	if _, err := db.Exec(`INSERT INTO uno (id) VALUES (1)`); err != nil {
		t.Fatalf("BD inusable tras rollback: %v", err)
	}
}

// TestBaselineSobreEsquemaPhotos: simula la adopción de una memories.db viva
// (SPEC §5.3): BD creada con el esquema histórico de photos (CREATE TABLE IF
// NOT EXISTS + los 2 ALTER de is_archived/phash, user_version=0) → verificar
// tablas/columnas esperadas → Baseline(db, 1).
func TestBaselineSobreEsquemaPhotos(t *testing.T) {
	db, _ := open(t)
	defer db.Close()

	// esquema histórico de photos (ocphotos internal/store/sqlite.go), ya con
	// las columnas de los ALTERs idempotentes aplicadas
	const photosSchema = `
CREATE TABLE IF NOT EXISTS assets (
    id          INTEGER PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    etag        TEXT NOT NULL,
    filename    TEXT NOT NULL,
    media_type  TEXT NOT NULL DEFAULT 'image',
    taken_at    INTEGER NOT NULL,
    exif_done   INTEGER NOT NULL DEFAULT 0,
    is_archived INTEGER NOT NULL DEFAULT 0,
    phash       TEXT,
    size        INTEGER NOT NULL DEFAULT 0,
    deleted_at  INTEGER,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS scan_state (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS albums (id INTEGER PRIMARY KEY, name TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch()));
CREATE TABLE IF NOT EXISTS album_assets (
    album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    PRIMARY KEY (album_id, asset_id)
);
CREATE TABLE IF NOT EXISTS asset_tags (
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag TEXT NOT NULL, PRIMARY KEY (asset_id, tag)
);
CREATE TABLE IF NOT EXISTS geocode (
    lat_key REAL NOT NULL, lon_key REAL NOT NULL,
    name TEXT NOT NULL, ts INTEGER NOT NULL,
    PRIMARY KEY (lat_key, lon_key)
);`
	if _, err := db.Exec(photosSchema); err != nil {
		t.Fatalf("crear esquema photos: %v", err)
	}
	if v := userVersion(t, db); v != 0 {
		t.Fatalf("una BD viva de photos llega con user_version=0, got %d", v)
	}

	// verificación previa a la adopción (la hará internal/photos en H3):
	// tablas esperadas y columnas is_archived/phash en assets
	for _, table := range []string{"assets", "scan_state", "albums", "album_assets", "asset_tags", "geocode"} {
		if !tableExists(t, db, table) {
			t.Fatalf("falta la tabla esperada %s", table)
		}
	}
	cols := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(assets)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		cols[name] = true
	}
	for _, col := range []string{"is_archived", "phash"} {
		if !cols[col] {
			t.Fatalf("falta la columna assets.%s", col)
		}
	}

	if err := Baseline(db, 1); err != nil {
		t.Fatalf("Baseline: %v", err)
	}
	if v := userVersion(t, db); v != 1 {
		t.Fatalf("user_version tras Baseline: got %d, want 1", v)
	}

	// tras baselinar, el runner no aplica 001_baseline.sql de nuevo
	fsys := fstest.MapFS{
		"migrations/001_baseline.sql": &fstest.MapFile{Data: []byte(
			`CREATE TABLE IF NOT EXISTS baseline_marker (id INTEGER);`)},
	}
	if err := Migrate(db, fsys, "migrations"); err != nil {
		t.Fatal(err)
	}
	if tableExists(t, db, "baseline_marker") {
		t.Fatal("Migrate aplicó 001_baseline.sql sobre una BD ya baselinada")
	}
}

func TestBaselineRechazaNegativo(t *testing.T) {
	db, _ := open(t)
	defer db.Close()
	if err := Baseline(db, -1); err == nil {
		t.Fatal("esperaba error con versión negativa")
	}
}
