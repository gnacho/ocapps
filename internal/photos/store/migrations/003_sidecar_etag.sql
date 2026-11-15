-- 003 - etag of the XMP sidecar applied to the index (see issue #19).
--
-- The scanner reads the .xmp files it finds next to each photo and projects
-- their tags and favourite flag onto the asset row. Storing the etag of the
-- applied sidecar avoids downloading it on every scan: if the etag of the
-- listing matches, the file is not requested and the database is not touched.
--
-- Empty = no sidecar has ever been applied to this asset (or the column was
-- dropped during a rebuild). It is not cleared when the sidecar disappears:
-- the local metadata stays there until another change replaces it.

ALTER TABLE assets ADD COLUMN sidecar_etag TEXT NOT NULL DEFAULT '';
