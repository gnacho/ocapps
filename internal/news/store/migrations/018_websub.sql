-- WebSub (PubSubHubbub) (#44): subscription to hubs to receive items in
-- real time. A feed with rel=hub can push content to the callback.

CREATE TABLE IF NOT EXISTS websub (
  feed_id     INTEGER PRIMARY KEY REFERENCES feeds(id) ON DELETE CASCADE,
  hub         TEXT NOT NULL,
  topic       TEXT NOT NULL,
  secret      TEXT NOT NULL DEFAULT '',
  callback    TEXT NOT NULL DEFAULT '',
  lease_until INTEGER NOT NULL DEFAULT 0,
  status      TEXT NOT NULL DEFAULT 'pending'
);
