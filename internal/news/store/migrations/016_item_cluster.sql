-- Clustering of the same story across several feeds (#42): cluster_key normalizes
-- title+body to group duplicates across subscriptions.

ALTER TABLE items ADD COLUMN cluster_key TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_items_cluster ON items(user_id, cluster_key);
