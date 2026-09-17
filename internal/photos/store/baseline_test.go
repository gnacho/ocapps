package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// liveSchemaSQL reproduce una memories.db VIVA pre-H8 tal y como la deja el
// servicio histórico: los CREATE TABLE originales (sin is_archived/phash ni
// owner) más los dos ALTER idempotentes que se aplicaban en caliente al abrir.
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

func indexExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestBaselineAdoptaBDViva: una memories.db viva pre-H8 (esquema single-tenant
// completo, user_version=0) se adopta con Baseline(1) sin ejecutar SQL de
// esquema y Migrate aplica 002 (multi-owner): user_version final = 2 (§5.3).
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
	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version tras baseline+002 = %d, quiero 2", v)
	}
	// el dato preexistente queda con owner='' a la espera del backfill (H8)
	stats, err := st.Stats(context.Background(), "")
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("stats tras adopción: %v %v", stats, err)
	}
	// re-apertura: no-op
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("re-apertura: %v", err)
	}
	st2.Close()
	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version tras re-apertura = %d", v)
	}
}

// TestBaselineBDNueva: una BD nueva aplica 001 (ya multi-owner) + 002 vía
// Migrate y queda en user_version=2.
func TestBaselineBDNueva(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if v := userVersion(t, path); v != 2 {
		t.Fatalf("user_version = %d, quiero 2", v)
	}
	// el esquema funciona de verdad (con owner, H8)
	ctx := context.Background()
	if _, ch, err := st.UpsertByETag(ctx, "user-a", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10); err != nil || !ch {
		t.Fatalf("upsert: ch=%v err=%v", ch, err)
	}
	if err := st.SetPHash(ctx, "user-a", 1, "0000000000000000"); err != nil {
		t.Fatalf("phash (columna del baseline): %v", err)
	}
	if err := st.SetArchived(ctx, "user-a", 1, true); err != nil {
		t.Fatalf("is_archived (columna del baseline): %v", err)
	}
}

// TestMigracion002ConservaIDsYFKs: una BD viva con datos migra a multi-owner
// conservando los IDs de assets (las FKs de album_assets/asset_tags siguen
// válidas) y dejando owner=” para el backfill en Go (H8 §5.3). Es el test
// que detecta el DROP TABLE con FK CASCADE: si la migración corriera con
// foreign_keys ON, album_assets y asset_tags perderían sus filas.
func TestMigracion002ConservaIDsYFKs(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	execFixture(t, path, liveSchemaSQL)
	execFixture(t, path, `
		INSERT INTO assets (id, path, etag, filename, taken_at) VALUES
			(7, '/dav/x/a.jpg', 'e1', 'a.jpg', 1700000000),
			(9, '/dav/x/b.jpg', 'e2', 'b.jpg', 1700000100);
		INSERT INTO albums (id, name) VALUES (3, 'Vacaciones');
		INSERT INTO album_assets (album_id, asset_id) VALUES (3, 7), (3, 9);
		INSERT INTO asset_tags (asset_id, tag) VALUES (7, 'playa');
	`)
	st, err := Open(path)
	if err != nil {
		t.Fatalf("migración 002: %v", err)
	}
	defer st.Close()
	ctx := context.Background()

	// owner='' tras la migración; IDs conservados (7 y 9)
	if _, err := st.AssetByID(ctx, "", 7); err != nil {
		t.Fatalf("asset id=7 no conservado: %v", err)
	}
	if _, err := st.AssetByID(ctx, "", 9); err != nil {
		t.Fatalf("asset id=9 no conservado: %v", err)
	}
	// FKs íntegras: el álbum sigue teniendo sus 2 assets y el tag sobrevive
	if got, err := st.AlbumAssets(ctx, "", 3); err != nil || len(got) != 2 {
		t.Fatalf("album_assets tras 002: %d %v (¿DROP TABLE con FK ON?)", len(got), err)
	}
	if got, err := st.AssetTags(ctx, "", 7); err != nil || len(got) != 1 || got[0] != "playa" {
		t.Fatalf("asset_tags tras 002: %v %v", got, err)
	}
	// índices: los globales viejos fuera, los nuevos con prefijo owner creados
	if indexExists(t, path, "assets_taken") || indexExists(t, path, "assets_geo") {
		t.Fatal("los índices pre-002 (sin owner) deben desaparecer")
	}
	if !indexExists(t, path, "assets_owner_taken") || !indexExists(t, path, "assets_owner_geo") {
		t.Fatal("faltan los índices 002 con prefijo owner")
	}

	// backfill en Go: adopta las filas huérfanas a un owner
	oa, ob, err := st.OrphanCounts(ctx)
	if err != nil || oa != 2 || ob != 1 {
		t.Fatalf("orphans: %d assets %d albums %v", oa, ob, err)
	}
	nA, nB, err := st.BackfillOwner(ctx, "oc-id-legacy")
	if err != nil || nA != 2 || nB != 1 {
		t.Fatalf("backfill: %d %d %v", nA, nB, err)
	}
	if got, err := st.AssetByID(ctx, "oc-id-legacy", 7); err != nil || got.Path != "/dav/x/a.jpg" {
		t.Fatalf("asset adoptado: %+v %v", got, err)
	}
	if oa, ob, _ = st.OrphanCounts(ctx); oa != 0 || ob != 0 {
		t.Fatalf("orphans tras backfill: %d %d", oa, ob)
	}
	// backfill con owner vacío es error
	if _, _, err := st.BackfillOwner(ctx, ""); err == nil {
		t.Fatal("backfill con owner vacío debería fallar")
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
