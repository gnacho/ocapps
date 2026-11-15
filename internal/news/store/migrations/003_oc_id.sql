-- F2: canonical OpenCloud identity (IDM uuid) to unify
-- shadow users created through Basic (app token) and through Bearer (web session).

ALTER TABLE users ADD COLUMN oc_id TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX idx_users_oc_id ON users (oc_id) WHERE oc_id != '';
