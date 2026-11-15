-- Subfolders (#41): parent_id allows nesting folders (one level). When deleting
-- a parent folder, its subfolders move to the root (the API does it
-- before the delete, CASCADE is the safety net).

ALTER TABLE folders ADD COLUMN parent_id INTEGER REFERENCES folders(id) ON DELETE SET NULL;
