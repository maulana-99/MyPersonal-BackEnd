-- 005_v1_backlog.sql
-- Run: psql $DATABASE_URL -f migrations/005_v1_backlog.sql
--
-- V1 backlog: routine time link + routine completion tracking.

-- Routine → time link: a routine can be anchored to a start time of day.
ALTER TABLE routines ADD COLUMN IF NOT EXISTS start_time TIMESTAMPTZ;

-- Routine completion: per-item, per-day tracking. Idempotent toggle.
CREATE TABLE IF NOT EXISTS routine_completions (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    routine_id  UUID NOT NULL REFERENCES routines(id) ON DELETE CASCADE,
    item_id     UUID NOT NULL REFERENCES routine_items(id) ON DELETE CASCADE,
    completed_on DATE NOT NULL DEFAULT CURRENT_DATE,
    completed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (routine_id, item_id, completed_on)
);

CREATE INDEX IF NOT EXISTS idx_routine_completions_routine
    ON routine_completions (routine_id, completed_on);
