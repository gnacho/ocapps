-- 002 - multi-owner (H8, SPEC §5.3/§6.2).
--
-- Turns the photos index from single-tenant to multi-tenant:
--   - assets: table rebuild (SQLite cannot drop the inline UNIQUE on
--     path): new UNIQUE(owner, path) and indexes with the owner prefix. IDs
--     are preserved, so the FKs of album_assets/asset_tags remain valid. The
--     runner executes this migration with foreign_keys OFF
--     (common/store.Migrate): with FK ON the DROP TABLE would make an implicit
--     DELETE that would fire the ON DELETE CASCADE of the child tables.
--     The INSERT copies the column list COMMON to the live pre-002 schema and
--     to baseline 001 already multi-owner (owner keeps its DEFAULT ''), so the
--     same script works for the live database adopted in 1 and for the new
--     database created by 001 (empty: effective no-op). Rows with owner=''
--     are those of the single-tenant era: they are adopted by the Go backfill
--     (Store.BackfillOwner), which cannot be SQL because it needs to resolve
--     the oc_id against Graph.
--   - albums: ALTER ADD COLUMN owner (valid both for the live database and for
--     the new one, whose baseline 001 creates albums without owner).
--   - geocode: NO changes on purpose - it is a GLOBAL cache of place names
--     (public Nominatim data), not personal information.
--   - scan_state: no changes (legacy table unused in the code).

CREATE TABLE assets_new (
    id          INTEGER PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',
    path        TEXT NOT NULL,
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
    created_at  INTEGER NOT NULL DEFAULT (unixepoch()),
    is_archived INTEGER NOT NULL DEFAULT 0,
    phash       TEXT,
    UNIQUE (owner, path)
);
INSERT INTO assets_new
    (id, path, etag, filename, media_type, taken_at, exif_done, width, height,
     camera, lens, iso, aperture, shutter, focal, lat, lon, size, is_favorite,
     deleted_at, created_at, is_archived, phash)
SELECT
    id, path, etag, filename, media_type, taken_at, exif_done, width, height,
    camera, lens, iso, aperture, shutter, focal, lat, lon, size, is_favorite,
    deleted_at, created_at, is_archived, phash
FROM assets;
DROP TABLE assets;
ALTER TABLE assets_new RENAME TO assets;

-- old indexes (without owner) dropped if they exist (adopted live database).
-- The new ones carry the owner prefix. On a new database 001 already created
-- them: IF NOT EXISTS.
DROP INDEX IF EXISTS assets_taken;
DROP INDEX IF EXISTS assets_geo;
CREATE INDEX IF NOT EXISTS assets_owner_taken ON assets (owner, taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS assets_owner_geo   ON assets (owner, lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;

ALTER TABLE albums ADD COLUMN owner TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS albums_owner_name ON albums (owner, name);
