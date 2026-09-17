-- Baseline del esquema de photos (SPEC §5.3), ya multi-owner (H8).
--
-- Es el esquema completo actual de ocphotos tal y como queda en una
-- memories.db viva: los CREATE TABLE originales + los dos ALTER históricos
-- (is_archived, phash) ya incorporados al CREATE de assets, más la columna
-- owner de H8 (multi-tenant): una BD NUEVA nace ya con el esquema
-- multi-owner y convergerá a user_version=2 cuando Migrate aplique también
-- 002_multiowner.sql (en una BD nueva 002 reconstruye tablas vacías: no-op
-- efectivo). Las BDs VIVAS pre-H8 se adoptan con Baseline(1) sin ejecutar
-- este SQL y es 002 quien les añade owner (rebuild de assets + ALTER de
-- albums).
--
-- Regla (§5.3): toda migración futura es un fichero 003_*.sql inmutable;
-- prohibido ALTERs tolerantes a error fuera del runner.

CREATE TABLE IF NOT EXISTS assets (
    id          INTEGER PRIMARY KEY,
    owner       TEXT NOT NULL DEFAULT '',  -- oc_id del dueño (H8); '' = era single-tenant, pendiente de backfill
    path        TEXT NOT NULL,             -- ruta WebDAV completa (incluye el space-UUID del usuario)
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
    phash       TEXT,
    UNIQUE (owner, path)                   -- el mismo path puede coexistir en dos owners
);
CREATE INDEX IF NOT EXISTS assets_owner_taken ON assets (owner, taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS assets_owner_geo   ON assets (owner, lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;

CREATE TABLE IF NOT EXISTS scan_state (
    key   TEXT PRIMARY KEY,
    value TEXT
);

-- albums sin owner en el baseline: 002 lo añade con ALTER (válido para la BD
-- nueva y para la viva adoptada; el estado final tras 001+002 es idéntico).
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

-- caché de geocodificación inversa (Lugares): clave = coords redondeadas.
-- SIN owner a propósito (H8): es una caché global de nombres de lugares
-- (dato público de Nominatim), no información personal del usuario.
CREATE TABLE IF NOT EXISTS geocode (
    lat_key REAL NOT NULL,
    lon_key REAL NOT NULL,
    name    TEXT NOT NULL,
    ts      INTEGER NOT NULL,
    PRIMARY KEY (lat_key, lon_key)
);
