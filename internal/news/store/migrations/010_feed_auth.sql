-- 010: per-feed HTTP Basic credentials (auth_pass encrypted AES-GCM, see internal/cred).
ALTER TABLE feeds ADD COLUMN auth_user TEXT NOT NULL DEFAULT '';
ALTER TABLE feeds ADD COLUMN auth_pass TEXT NOT NULL DEFAULT '';
