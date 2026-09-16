-- Baseline del esquema de photos (SPEC §5.3).
--
-- Es el esquema completo actual de ocphotos tal y como queda en una
-- memories.db viva: los CREATE TABLE originales + los dos ALTER históricos
-- (is_archived, phash) ya incorporados al CREATE de assets. Todo es
-- idempotente (IF NOT EXISTS) para que la adopción de una BD viva y la
-- creación de una BD nueva converjan al mismo estado con user_version=1.
--
-- Regla (§5.3): toda migración futura es un fichero 002_*.sql inmutable;
-- prohibido ALTERs tolerantes a error fuera del runner.

CREATE TABLE IF NOT EXISTS assets (
    id          INTEGER PRIMARY KEY,
    path        TEXT NOT NULL UNIQUE,      -- ruta WebDAV completa
    etag        TEXT NOT NULL,
    filename    TEXT NOT NULL,
    media_type  TEXT NOT NULL DEFAULT 'image',
    taken_at    INTEGER NOT NULL,          -- unix epoch (EXIF o mtime)
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
    phash       TEXT
);
CREATE INDEX IF NOT EXISTS assets_taken ON assets (taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS assets_geo   ON assets (lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;

CREATE TABLE IF NOT EXISTS scan_state (
    key   TEXT PRIMARY KEY,
    value TEXT
);

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

-- caché de geocodificación inversa (Lugares): clave = coords redondeadas
CREATE TABLE IF NOT EXISTS geocode (
    lat_key REAL NOT NULL,
    lon_key REAL NOT NULL,
    name    TEXT NOT NULL,
    ts      INTEGER NOT NULL,
    PRIMARY KEY (lat_key, lon_key)
);
