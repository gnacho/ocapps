-- 004 - the space (drive) each asset belongs to (see issue #145).
--
-- The scan walks every space visible to the owner (personal, project spaces and
-- shares), so an asset is no longer identifiable by its path alone for the
-- user: two spaces can hold the same relative folder, and the UI needs to tell
-- which space a photo lives in and filter by it.
--
-- The path already carries the space UUID (/dav/spaces/<storage>$<space>/...),
-- but parsing it per row costs on every query and the name and the type of the
-- space do not appear in it. The three columns are written by the scanner from
-- /me/drives.
--
-- Empty = rows indexed before this migration: they are filled by the next scan
-- (the upsert refreshes them) and the API treats an empty spaceId as unknown,
-- not as a different space.

ALTER TABLE assets ADD COLUMN space_id TEXT NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN space_name TEXT NOT NULL DEFAULT '';
ALTER TABLE assets ADD COLUMN space_type TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS assets_owner_space_taken ON assets (owner, space_id, taken_at DESC) WHERE deleted_at IS NULL;
