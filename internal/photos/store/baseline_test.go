package store

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"
)

// lastVersion is the last migration in migrations/: when a new one is added
// this number has to be bumped. The adoption tests compare against it so that
// three literals do not have to be edited every time.
const lastVersion = 4

// liveSchemaSQL reproduces a LIVE pre-H8 memories.db as the historical service
// left it: the original CREATE TABLE statements (without is_archived/phash and
// without owner) plus the two idempotent ALTERs that were applied on the fly
// when opening.
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
	defer func() { _ = db.Close() }()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// execFixture creates a database with raw SQL (without going through Open or
// the runner).
func execFixture(t *testing.T, path, sqlBody string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
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
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='index' AND name=?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

// TestBaselineAdoptsLiveDB: a live pre-H8 memories.db (full single-tenant
// schema, user_version=0) is adopted with Baseline(1) without running schema
// SQL and Migrate applies 002 (multi-owner): final user_version = 2 (§5.3).
func TestBaselineAdoptsLiveDB(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	execFixture(t, path, liveSchemaSQL)
	// pre-existing data: adoption must not touch it
	execFixture(t, path, `INSERT INTO assets (path, etag, filename, taken_at) VALUES ('/dav/x/a.jpg','e1','a.jpg',1700000000)`)
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("fixture should have user_version=0, has %d", v)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("adoption of the live database: %v", err)
	}
	defer func() { _ = st.Close() }()
	if v := userVersion(t, path); v != lastVersion {
		t.Fatalf("user_version after baseline+migrations = %d, want %d", v, lastVersion)
	}
	// the pre-existing row keeps owner empty waiting for the backfill (H8)
	stats, err := st.Stats(context.Background(), "")
	if err != nil || stats["assets"].(int64) != 1 {
		t.Fatalf("stats after adoption: %v %v", stats, err)
	}
	// reopening: no-op
	st2, err := Open(path)
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	_ = st2.Close()
	if v := userVersion(t, path); v != lastVersion {
		t.Fatalf("user_version after reopening = %d, want %d", v, lastVersion)
	}
}

// TestBaselineNewDB: a new database applies 001 (already multi-owner) plus 002
// through Migrate and ends up at user_version=2.
func TestBaselineNewDB(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if v := userVersion(t, path); v != lastVersion {
		t.Fatalf("user_version = %d, want %d", v, lastVersion)
	}
	// the schema really works (with owner, H8)
	ctx := context.Background()
	if _, ch, err := st.UpsertByETag(ctx, "user-a", "/dav/x/a.jpg", "e1", "a.jpg", "image", time.Now(), 10); err != nil || !ch {
		t.Fatalf("upsert: ch=%v err=%v", ch, err)
	}
	if err := st.SetPHash(ctx, "user-a", 1, "0000000000000000"); err != nil {
		t.Fatalf("phash (baseline column): %v", err)
	}
	if err := st.SetArchived(ctx, "user-a", 1, true); err != nil {
		t.Fatalf("is_archived (baseline column): %v", err)
	}
}

// TestMigration002KeepsIDsAndFKs: a live database with data migrates to
// multi-owner preserving the assets IDs (the FKs of album_assets/asset_tags
// remain valid) and leaving owner empty for the Go backfill (H8 §5.3). It is the
// test that catches the DROP TABLE with FK CASCADE: if the migration ran with
// foreign_keys ON, album_assets and asset_tags would lose their rows.
func TestMigration002KeepsIDsAndFKs(t *testing.T) {
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
		t.Fatalf("migration 002: %v", err)
	}
	defer func() { _ = st.Close() }()
	ctx := context.Background()

	// owner empty after the migration, IDs preserved (7 and 9)
	if _, err := st.AssetByID(ctx, "", 7); err != nil {
		t.Fatalf("asset id=7 not preserved: %v", err)
	}
	if _, err := st.AssetByID(ctx, "", 9); err != nil {
		t.Fatalf("asset id=9 not preserved: %v", err)
	}
	// FKs intact: the album still has its 2 assets and the tag survives
	if got, err := st.AlbumAssets(ctx, "", 3); err != nil || len(got) != 2 {
		t.Fatalf("album_assets after 002: %d %v (DROP TABLE with FK ON?)", len(got), err)
	}
	if got, err := st.AssetTags(ctx, "", 7); err != nil || len(got) != 1 || got[0] != "playa" {
		t.Fatalf("asset_tags after 002: %v %v", got, err)
	}
	// indexes: the old global ones are gone, the new owner-prefixed ones created
	if indexExists(t, path, "assets_taken") || indexExists(t, path, "assets_geo") {
		t.Fatal("the pre-002 indexes (without owner) must disappear")
	}
	if !indexExists(t, path, "assets_owner_taken") || !indexExists(t, path, "assets_owner_geo") {
		t.Fatal("the 002 indexes with owner prefix are missing")
	}

	// Go backfill: adopts the orphan rows into an owner
	oa, ob, err := st.OrphanCounts(ctx)
	if err != nil || oa != 2 || ob != 1 {
		t.Fatalf("orphans: %d assets %d albums %v", oa, ob, err)
	}
	nA, nB, rmA, rmB, err := st.BackfillOwner(ctx, "oc-id-legacy")
	if err != nil || nA != 2 || nB != 1 || rmA != 0 || rmB != 0 {
		t.Fatalf("backfill: adopted %d/%d removed %d/%d %v", nA, nB, rmA, rmB, err)
	}
	if got, err := st.AssetByID(ctx, "oc-id-legacy", 7); err != nil || got.Path != "/dav/x/a.jpg" {
		t.Fatalf("adopted asset: %+v %v", got, err)
	}
	if oa, ob, _ = st.OrphanCounts(ctx); oa != 0 || ob != 0 {
		t.Fatalf("orphans after backfill: %d %d", oa, ob)
	}
	// backfill with an empty owner is an error
	if _, _, _, _, err := st.BackfillOwner(ctx, ""); err == nil {
		t.Fatal("backfill with an empty owner should fail")
	}
}

// TestBaselinePartialSchemaFails: schema tables but missing some historical
// column -> fail-loud error (no tolerant ALTERs).
func TestBaselinePartialSchemaFails(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	partial := strings.Replace(liveSchemaSQL, "ALTER TABLE assets ADD COLUMN phash TEXT;", "", 1)
	execFixture(t, path, partial)
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "phash") {
		t.Fatalf("expected an error for the partial schema (phash), got %v", err)
	}
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("user_version must not be touched on failure: %d", v)
	}
}

// TestBaselineForeignDBFails: a database with foreign tables is neither
// adopted nor migrated.
func TestBaselineForeignDBFails(t *testing.T) {
	path := t.TempDir() + "/memories.db"
	execFixture(t, path, `CREATE TABLE otra_cosa (id INTEGER PRIMARY KEY)`)
	if _, err := Open(path); err == nil {
		t.Fatal("expected an error for a database with foreign tables")
	}
	if v := userVersion(t, path); v != 0 {
		t.Fatalf("user_version must not be touched on failure: %d", v)
	}
}
