-- 003_reminders.sql
-- Run: psql $DATABASE_URL -f migrations/003_reminders.sql
--
-- Concrete fire time + dismissal state for reminders, so the scheduler can
-- scan "due now" with a plain index instead of recomputing offset arithmetic.

ALTER TABLE reminders ADD COLUMN IF NOT EXISTS fire_at TIMESTAMPTZ;
ALTER TABLE reminders ADD COLUMN IF NOT EXISTS dismissed BOOLEAN NOT NULL DEFAULT FALSE;

-- Backfill existing rows: fire_at = schedule start - offset minutes.
--
-- Guarded against re-running. A recurring schedule's reminders are TEMPLATES
-- (fire_at IS NULL, expanded per occurrence by 004), and a concrete occurrence
-- row can already hold this exact fire_at — the first occurrence equals DTSTART.
-- An unguarded backfill therefore collides with 004's
-- idx_reminders_unique_fire (schedule_id, offset_mins, fire_at) on any database
-- the scheduler has already populated, which is every non-empty one.
UPDATE reminders r
SET fire_at = s.start_time - (r.offset_mins * interval '1 minute')
FROM schedules s
WHERE s.id = r.schedule_id
  AND r.fire_at IS NULL
  AND NOT EXISTS (
      SELECT 1 FROM reminders other
      WHERE other.schedule_id = r.schedule_id
        AND other.offset_mins = r.offset_mins
        AND other.fire_at = s.start_time - (r.offset_mins * interval '1 minute')
  );

CREATE INDEX IF NOT EXISTS idx_reminders_due
    ON reminders (sent, dismissed, fire_at);
