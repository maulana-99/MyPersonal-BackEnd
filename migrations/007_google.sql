-- 007_google.sql
-- Run: psql $DATABASE_URL -f migrations/007_google.sql
--
-- Google Calendar sync: one integration per user plus link columns on schedules.
-- Idempotent.

CREATE TABLE IF NOT EXISTS integrations_google (
    user_id           UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    google_email      TEXT NOT NULL,
    refresh_token_enc BYTEA NOT NULL,        -- AES-256-GCM, nonce prefixed
    calendar_id       TEXT NOT NULL DEFAULT 'primary',
    sync_token        TEXT,
    status            TEXT NOT NULL DEFAULT 'connected', -- connected | needs_reauth
    last_synced_at    TIMESTAMPTZ,
    last_error        TEXT,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE schedules
    ADD COLUMN IF NOT EXISTS source            TEXT NOT NULL DEFAULT 'local', -- local | google
    ADD COLUMN IF NOT EXISTS google_event_id   TEXT,
    ADD COLUMN IF NOT EXISTS google_etag       TEXT,
    ADD COLUMN IF NOT EXISTS google_sync_state TEXT,   -- NULL | synced | failed
    ADD COLUMN IF NOT EXISTS google_sync_error TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_schedules_google_event
    ON schedules (user_id, google_event_id) WHERE google_event_id IS NOT NULL;
