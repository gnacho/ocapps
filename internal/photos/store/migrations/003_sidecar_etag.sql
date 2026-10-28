-- 003 — etag del sidecar XMP aplicado al índice (ver issue #19).
--
-- El scanner lee los .xmp que encuentra junto a cada foto y proyecta sus
-- etiquetas y su favorito sobre la fila del asset. Guardar el etag del
-- sidecar aplicado evita descargarlo en cada scan: si el etag del listado
-- coincide, no se pide el fichero ni se toca la BD.
--
-- Vacío = nunca se ha aplicado un sidecar a este asset (o se borró la
-- columna al reconstruir). No se borra al desaparecer el sidecar: los
-- metadatos locales siguen ahí hasta que otro cambio los sustituya.

ALTER TABLE assets ADD COLUMN sidecar_etag TEXT NOT NULL DEFAULT '';
