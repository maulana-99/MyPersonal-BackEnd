-- 004_occurrences.sql
-- Run: psql $DATABASE_URL -f migrations/004_occurrences.sql
--
-- Materialised instances of recurring schedules. The scheduler expands each
-- schedule_recurrences rule into concrete rows over a rolling window so
-- Calendar/Today/reminders can query plain timestamps.

CREATE TABLE IF NOT EXISTS schedule_occurrences (
    id               UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    schedule_id      UUID NOT NULL REFERENCES schedules(id) ON DELETE CASCADE,
    occurrence_start TIMESTAMPTZ NOT NULL,
    occurrence_end   TIMESTAMPTZ,
    status           TEXT NOT NULL DEFAULT 'upcoming',
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (schedule_id, occurrence_start)
);

CREATE INDEX IF NOT EXISTS idx_occurrences_schedule
    ON schedule_occurrences (schedule_id, occurrence_start);

CREATE INDEX IF NOT EXISTS idx_occurrences_start
    ON schedule_occurrences (occurrence_start);

-- Occurrence materialisation inserts concrete reminder rows per occurrence;
-- this keeps that idempotent. Partial: template rows (fire_at IS NULL) for
-- recurring schedules are exempt since NULLs are not comparable.
CREATE UNIQUE INDEX IF NOT EXISTS idx_reminders_unique_fire
    ON reminders (schedule_id, offset_mins, fire_at)
    WHERE fire_at IS NOT NULL;
