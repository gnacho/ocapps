-- Baseline of the photos schema (SPEC §5.3), already multi-owner (H8).
--
-- It is the current full schema of ocphotos as it ends up in a live
-- memories.db: the original CREATE TABLE statements plus the two historical
-- ALTERs (is_archived, phash) already folded into the assets CREATE, plus the
-- H8 owner column (multi-tenant): a NEW database is born with the multi-owner
-- schema and converges to user_version=2 when Migrate also applies
-- 002_multiowner.sql (on a new database 002 rebuilds empty tables: effective
-- no-op). LIVE pre-H8 databases are adopted with Baseline(1) without running
-- this SQL and it is 002 that adds owner to them (rebuild of assets + ALTER of
-- albums).
--
-- Rule (§5.3): every future migration is an immutable 003_*.sql file.
-- Tolerant ALTERs outside the runner are forbidden.

CREATE TABLE IF NOT EXISTS assets (
    id          INTEGER PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',  -- oc_id of the owner (H8), '' = single-tenant era, pending backfill
    path        TEXT NOT NULL,             -- full WebDAV path (includes the user space UUID)
    etag        TEXT NOT NULL,
    filename    TEXT NOT NULL,
    media_type  TEXT NOT NULL DEFAULT 'image',
    taken_at    INTEGER NOT NULL,          -- unix epoch (EXIF or mtime)
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
    UNIQUE (owner, path)                   -- the same path can coexist under two owners
);
CREATE INDEX IF NOT EXISTS assets_owner_taken ON assets (owner, taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS assets_owner_geo   ON assets (owner, lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;

CREATE TABLE IF NOT EXISTS scan_state (
    key   TEXT PRIMARY KEY,
    value TEXT
);

-- albums without owner in the baseline: 002 adds it with an ALTER (valid for
-- the new database and for the adopted live one, the final state after
-- 001+002 is identical).
CREATE TABLE IF NOT EXISTS albums (
    id         INTEGER PRIMARY KEY,
    name       TEXT NOT NULL,
    created_at INTEGER NOT NULL DEFAULT (unixepoch())
);
CREATE TABLE IF NOT EXISTS album_assets (
    album_id INTEGER NOT NULL REFERENCES albums(id) ON DELETE CASCADE,
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    added_at INTEGER NOT NULL DEFAULT (unixepoch()),
    PRIMARY KEY (album_id, asset_id)
);
CREATE INDEX IF NOT EXISTS album_assets_asset ON album_assets (asset_id);

CREATE TABLE IF NOT EXISTS asset_tags (
    asset_id INTEGER NOT NULL REFERENCES assets(id) ON DELETE CASCADE,
    tag      TEXT NOT NULL,
    PRIMARY KEY (asset_id, tag)
);
CREATE INDEX IF NOT EXISTS asset_tags_tag ON asset_tags (tag);

-- reverse geocoding cache (Places): key = rounded coords.
-- WITHOUT owner on purpose (H8): it is a global cache of place names
-- (public Nominatim data), not personal information of the user.
CREATE TABLE IF NOT EXISTS geocode (
    lat_key REAL NOT NULL,
    lon_key REAL NOT NULL,
    name    TEXT NOT NULL,
    ts      INTEGER NOT NULL,
    PRIMARY KEY (lat_key, lon_key)
);
