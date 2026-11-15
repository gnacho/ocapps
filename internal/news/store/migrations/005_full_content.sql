-- Detection of feeds that already serve full content: if any new
-- item goes over the text threshold, the feed is flagged and the client does not
-- offer the full extraction (it would be redundant).

ALTER TABLE feeds ADD COLUMN full_content INTEGER NOT NULL DEFAULT 0;
