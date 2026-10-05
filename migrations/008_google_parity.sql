-- 008_google_parity.sql
-- Run: psql $DATABASE_URL -f migrations/008_google_parity.sql
--
-- Google Calendar parity: location, colour, Meet link, per-event time zone and
-- guests. Idempotent.

ALTER TABLE schedules
    ADD COLUMN IF NOT EXISTS location  TEXT,
    ADD COLUMN IF NOT EXISTS color_id  TEXT,
    ADD COLUMN IF NOT EXISTS meet_link TEXT,
    ADD COLUMN IF NOT EXISTS tz        TEXT NOT NULL DEFAULT 'UTC';

CREATE TABLE IF NOT EXISTS schedule_attendees (
    schedule_id     UUID NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    email           TEXT NOT NULL,
    display_name    TEXT,
    response_status TEXT NOT NULL DEFAULT 'needsAction', -- needsAction | accepted | declined | tentative
    is_organizer    BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (schedule_id, email)
);
