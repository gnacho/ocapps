-- 002 — multi-owner (H8, SPEC §5.3/§6.2).
--
-- Convierte el índice de photos de single-tenant a multi-tenant:
--   - assets: rebuild de tabla (SQLite no puede quitar el UNIQUE inline de
--     path): nueva UNIQUE(owner, path) e índices con prefijo owner. Se
--     conservan los IDs, así que las FKs de album_assets/asset_tags siguen
--     válidas. El runner ejecuta esta migración con foreign_keys OFF
--     (common/store.Migrate): con FK ON el DROP TABLE haría un DELETE
--     implícito que dispararía los ON DELETE CASCADE de las tablas hija.
--     El INSERT copia la lista de columnas COMUN al esquema vivo pre-002 y
--     al baseline 001 ya multi-owner (owner queda con su DEFAULT ''), así
--     que el mismo script sirve para la BD viva adoptada en 1 y para la BD
--     nueva creada por 001 (vacía: no-op efectivo). Las filas con owner=''
--     son las de la era single-tenant: las adopta el backfill en Go
--     (Store.BackfillOwner), que no puede ser SQL porque necesita resolver
--     el oc_id contra Graph.
--   - albums: ALTER ADD COLUMN owner (sirve tanto para la BD viva como
--     para la nueva, cuyo baseline 001 crea albums sin owner).
--   - geocode: SIN cambios a propósito — es una caché GLOBAL de nombres de
--     lugares (dato público de Nominatim), no información personal.
--   - scan_state: sin cambios (tabla legacy sin uso en código).

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

-- índices viejos (sin owner) fuera si existen (BD viva adoptada); los nuevos
-- llevan prefijo owner. En una BD nueva 001 ya los creó: IF NOT EXISTS.
DROP INDEX IF EXISTS assets_taken;
DROP INDEX IF EXISTS assets_geo;
CREATE INDEX IF NOT EXISTS assets_owner_taken ON assets (owner, taken_at DESC) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS assets_owner_geo   ON assets (owner, lat, lon) WHERE deleted_at IS NULL AND lat IS NOT NULL;

ALTER TABLE albums ADD COLUMN owner TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS albums_owner_name ON albums (owner, name);
