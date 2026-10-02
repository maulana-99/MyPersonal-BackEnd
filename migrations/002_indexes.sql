-- 002_indexes.sql
-- Indexes for the query patterns the API actually uses.
-- Every user-scoped list filters by user_id and orders by time.

CREATE INDEX IF NOT EXISTS idx_schedules_user_start
    ON schedules (user_id, start_time DESC);

CREATE INDEX IF NOT EXISTS idx_schedules_user_status
    ON schedules (user_id, status);

CREATE INDEX IF NOT EXISTS idx_schedules_category
    ON schedules (category_id);

CREATE INDEX IF NOT EXISTS idx_reminders_schedule
    ON reminders (schedule_id);

-- Scheduler scans for reminders still pending on schedules starting soon.
CREATE INDEX IF NOT EXISTS idx_reminders_pending
    ON reminders (sent, offset_mins);

CREATE INDEX IF NOT EXISTS idx_tasks_user_due
    ON tasks (user_id, due_date);

CREATE INDEX IF NOT EXISTS idx_tasks_user_status
    ON tasks (user_id, status);

CREATE INDEX IF NOT EXISTS idx_categories_user
    ON categories (user_id);

CREATE INDEX IF NOT EXISTS idx_routines_user
    ON routines (user_id);

CREATE INDEX IF NOT EXISTS idx_routine_items_routine
    ON routine_items (routine_id, sort_order);

CREATE INDEX IF NOT EXISTS idx_notifications_user_created
    ON notifications (user_id, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_categories_user_name
    ON categories (user_id, lower(name));
