-- 009_google_login.sql
-- Run: psql $DATABASE_URL -f migrations/009_google_login.sql
--
-- Google-first login: users are identified by their Google subject and may have
-- no password. Idempotent.

ALTER TABLE users ADD COLUMN IF NOT EXISTS google_sub TEXT;
CREATE UNIQUE INDEX IF NOT EXISTS idx_users_google_sub
    ON users (google_sub) WHERE google_sub IS NOT NULL;
ALTER TABLE users ALTER COLUMN password_hash DROP NOT NULL;
