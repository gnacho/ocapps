package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// liveSchemaSQL reproduce una memories.db VIVA tal y como la deja el servicio
// histórico: los CREATE TABLE originales (sin is_archived/phash) más los dos
// ALTER idempotentes que se aplicaban en caliente al abrir.
const liveSchemaSQL = `
CREATE TABLE assets (
    id          INTEGER PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,
    etag        TEXT NOT NULL,
    filename    TEXT NOT NULL,
    media_type  TEXT NOT NULL DEFAULT 'image',
    taken_at    INTEGER NOT NULL,
    exif_done   INTEGER NOT NULL DEFAULT 0,
    width       INTEGER,
    height      INTEGER,
    camera      TEXT,
    lens        TEXT,
    iso         INTEGER,
    aperture    TEXT,
    shutter     TEXT,
    focal       TEXT,
    lat         REAL,
    lon         REAL,
    size        INTEGER NOT NULL DEFAULT 0,
    is_favorite INTEGER NOT NULL DEFAULT 0,
    deleted_at  INTEGER,
    created_at  INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE INDEX assets_taken ON assets (taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX assets_geo   ON assets (lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;
CREATE TABLE scan_state (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE albums (id INTEGER PRIMARY KEY, name TEXT NOT NULL, created_at INTEGER NOT NULL DEFAULT (unixepoch()));
CREATE TABLE album_assets (
    album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    added_at INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (album_id, asset_id)
);
CREATE INDEX album_assets_asset ON album_assets (asset_id);
CREATE TABLE asset_tags (
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag      TEXT NOT NULL,
    PRIMARY KEY (asset_id, tag)
);
CREATE INDEX asset_tags_tag ON asset_tags (tag);
CREATE TABLE geocode (lat_key REAL NOT NULL, lon_key REAL NOT NULL, name TEXT NOT NULL, ts INTEGER NOT NULL, PRIMARY KEY (lat_key, lon_key));
ALTER TABLE assets ADD COLUMN is_archived INTEGER NOT NULL DEFAULT 0;
ALTER TABLE assets ADD COLUMN phash TEXT;
`

func userVersion(t *testing.T, path string) int {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// execFixture crea una BD con SQL crudo (sin pasar por Open ni el runner).
func execFixture(t *testing.T, path, sqlBody string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(sqlBody); err != nil {
		t.Fatalf("fixture: %v", err)
	}
}

// TestBaselineAdoptaBDViva: una memories.db viva (esquema completo,
// user_version=0) se adopta con Baseline(1) sin ejecutar SQL de esquema.
func TestBaselineAdoptaBDViva(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	execFixture(t, path, liveSchemaSQL)
	// dato preexistente: la adopción no debe tocarlo
	execFixture(t, path, `INSERT INTO assets (path, etag, filename, taken_at) VALUES ('/dav/x/a.jpg','e1','a.jpg',1700000000)`)
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("fixture debería tener user_version=0, tiene %d", v)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("adopción de BD viva: %v", err)
	}
	defer st.Close()
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version tras baseline = %d, quiero 1", v)
	}
	stats, err := st.Stats(context.Background())
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("stats tras adopción: %v %v", stats, err)
	}
	// re-apertura: no-op
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-apertura: %v", err)
	}
	st2.Close()
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version tras re-apertura = %d", v)
	}
}

// TestBaselineBDNueva: una BD nueva aplica 001_baseline.sql vía Migrate.
func TestBaselineBDNueva(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v := userVersion(t, path); v != 1 {
		t.Fatalf("user_version = %d, quiero 1", v)
	}
	// el esquema funciona de verdad
	ctx := context.Background()
	if _, ch, err := st.UpsertByETag(ctx, "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10); err != nil || !ch {
		t.Fatalf("upsert: ch=%v err=%v", ch, err)
	}
	if err := st.SetPHash(ctx, 1, "0000000000000000"); err != nil {
		t.Fatalf("phash (columna del baseline): %v", err)
	}
	if err := st.SetArchived(ctx, 1, true); err != nil {
		t.Fatalf("is_archived (columna del baseline): %v", err)
	}
}

// TestBaselineEsquemaParcialFalla: tablas del esquema pero sin alguna columna
// histórica → error fail-loud (nada de ALTERs tolerantes).
func TestBaselineEsquemaParcialFalla(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	partial := strings.Replace(liveSchemaSQL, "ALTER TABLE assets ADD COLUMN phash TEXT;", "", 1)
	execFixture(t, path, partial)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "phash") {
		t.Fatalf("esperaba error por esquema parcial (phash), got %v", err)
	}
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("user_version no debe tocarse en el fallo: %d", v)
	}
}

// TestBaselineBDAjenaFalla: una BD con tablas ajenas no se adopta ni se migra.
func TestBaselineBDAjenaFalla(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	execFixture(t, path, `CREATE TABLE otra_cosa (id INTEGER PRIMARY KEY)`)
	if _, err := Open(path); err == nil {
		t.Fatal("esperaba error ante BD con tablas ajenas")
	}
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("user_version no debe tocarse en el fallo: %d", v)
	}
}
