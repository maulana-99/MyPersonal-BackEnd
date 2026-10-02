-- 006_v2.sql
-- Run: psql $DATABASE_URL -f migrations/006_v2.sql
--
-- V2 domain: habits, focus sessions, notes, daily reviews, focus rules.
-- Every table is user-scoped and cascades from users, like the V1 tables.
-- Local-day columns (log_date, review_date) are DATE: they are written by the
-- API from the client's local day (or Go local time), never by SQL CURRENT_DATE,
-- because the DB session runs UTC while the user is UTC+7.

-- ---- habits -----------------------------------------------------------------

CREATE TABLE IF NOT EXISTS habits (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category_id   UUID REFERENCES categories(id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    description   TEXT,
    target_type   TEXT NOT NULL DEFAULT 'daily' CHECK (target_type IN ('daily', 'weekly')),
    target_count  INT  NOT NULL DEFAULT 1 CHECK (target_count >= 1),
    repeat_days   TEXT[],          -- ['mon',...]; empty/NULL = every day
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_habits_user ON habits (user_id);

-- One row per habit per local day. `count` allows a daily target above one
-- (e.g. "drink water 8x"); a row is deleted when a decrement reaches zero.
CREATE TABLE IF NOT EXISTS habit_logs (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    habit_id   UUID NOT NULL REFERENCES habits(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    log_date   DATE NOT NULL,
    count      INT  NOT NULL DEFAULT 1 CHECK (count >= 0),
    note       TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (habit_id, log_date)
);

CREATE INDEX IF NOT EXISTS idx_habit_logs_habit_date ON habit_logs (habit_id, log_date);
CREATE INDEX IF NOT EXISTS idx_habit_logs_user_date  ON habit_logs (user_id, log_date);

-- ---- focus sessions ---------------------------------------------------------

CREATE TABLE IF NOT EXISTS focus_sessions (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    category_id    UUID REFERENCES categories(id) ON DELETE SET NULL,
    task_id        UUID REFERENCES tasks(id) ON DELETE SET NULL,
    title          TEXT NOT NULL,
    goal           TEXT,
    planned_minutes INT NOT NULL DEFAULT 25 CHECK (planned_minutes BETWEEN 1 AND 1440),
    started_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at       TIMESTAMPTZ,
    -- Marks when the current pause began, so `paused_seconds` can be settled
    -- on resume. Without it a pause that spans a server restart is unmeasurable.
    paused_at      TIMESTAMPTZ,
    paused_seconds INT NOT NULL DEFAULT 0 CHECK (paused_seconds >= 0),
    actual_minutes INT NOT NULL DEFAULT 0 CHECK (actual_minutes >= 0),
    status         TEXT NOT NULL DEFAULT 'running'
                   CHECK (status IN ('running', 'paused', 'completed', 'cancelled')),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_focus_sessions_user_start
    ON focus_sessions (user_id, started_at DESC);

-- Upgrade path for a table created by an earlier revision of this file:
-- `CREATE TABLE IF NOT EXISTS` skips the ENTIRE statement when the table
-- already exists, so a column added later must be applied separately or an
-- existing database silently keeps the old shape.
--
-- The ALTER is issued ONLY when the column is actually missing: `ADD COLUMN IF
-- NOT EXISTS` would still open an ACCESS EXCLUSIVE lock on every run, and
-- `TestMain.applyMigrations` re-runs this whole file for every package's test
-- binary — in parallel against one database. A lock-free no-op is what keeps
-- concurrent test packages from deadlocking on this table.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM information_schema.columns
        WHERE table_schema = 'public'
          AND table_name = 'focus_sessions'
          AND column_name = 'paused_at'
    ) THEN
        ALTER TABLE focus_sessions ADD COLUMN paused_at TIMESTAMPTZ;
    END IF;
END $$;

-- At most one live session per user, enforced by the database rather than by a
-- read-then-write in the handler (which would race).
CREATE UNIQUE INDEX IF NOT EXISTS idx_focus_sessions_one_live
    ON focus_sessions (user_id)
    WHERE status IN ('running', 'paused');

-- ---- notes ------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS notes (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title       TEXT NOT NULL,
    body        TEXT,
    -- A note outlives the schedule/task it was attached to: deleting the parent
    -- detaches it, matching the ON DELETE SET NULL convention of 001_init.sql.
    schedule_id UUID REFERENCES schedules(id) ON DELETE SET NULL,
    task_id     UUID REFERENCES tasks(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_notes_user_updated ON notes (user_id, updated_at DESC);
CREATE INDEX IF NOT EXISTS idx_notes_schedule ON notes (schedule_id);
CREATE INDEX IF NOT EXISTS idx_notes_task ON notes (task_id);

-- ---- daily reviews ----------------------------------------------------------

CREATE TABLE IF NOT EXISTS daily_reviews (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    review_date DATE NOT NULL,
    content     TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, review_date)
);

CREATE INDEX IF NOT EXISTS idx_daily_reviews_user_date
    ON daily_reviews (user_id, review_date DESC);

-- ---- productivity mode (web rules; enforcement is V3) -----------------------

CREATE TABLE IF NOT EXISTS focus_rules (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    start_time TIME NOT NULL,
    end_time   TIME NOT NULL,
    days       TEXT[],           -- ['mon',...]; empty/NULL = every day
    sites      TEXT[],           -- hostnames to block once enforced (V3)
    enabled    BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_focus_rules_user ON focus_rules (user_id);

-- ---- converge ---------------------------------------------------------------
--
-- `CREATE TABLE IF NOT EXISTS` skips the ENTIRE statement when the table
-- already exists, so a constraint adjusted after a table's first creation would
-- never reach an existing database. Each mutable constraint is therefore
-- reconciled below — but only when it is actually missing or wrong.
--
-- Every ALTER is guarded by a catalog check. This is not mere tidiness: an
-- unguarded `ALTER TABLE` takes ACCESS EXCLUSIVE, and `applyMigrations` re-runs
-- this file from every package's test binary, in parallel, against one database
-- (which deadlocked immediately when these were unconditional). On a database
-- that is already correct, nothing here acquires a table lock.

-- A note must survive its parent: detach on delete, never cascade.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.notes'::regclass
          AND conname = 'notes_schedule_id_fkey'
          AND confdeltype = 'n'          -- 'n' = ON DELETE SET NULL
    ) THEN
        ALTER TABLE notes DROP CONSTRAINT IF EXISTS notes_schedule_id_fkey;
        ALTER TABLE notes ADD CONSTRAINT notes_schedule_id_fkey
            FOREIGN KEY (schedule_id) REFERENCES schedules(id) ON DELETE SET NULL;
    END IF;

    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.notes'::regclass
          AND conname = 'notes_task_id_fkey'
          AND confdeltype = 'n'
    ) THEN
        ALTER TABLE notes DROP CONSTRAINT IF EXISTS notes_task_id_fkey;
        ALTER TABLE notes ADD CONSTRAINT notes_task_id_fkey
            FOREIGN KEY (task_id) REFERENCES tasks(id) ON DELETE SET NULL;
    END IF;
END $$;

-- No cross-midnight focus windows in V2 (documented limitation).
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conrelid = 'public.focus_rules'::regclass
          AND conname = 'focus_rules_window_check'
    ) THEN
        ALTER TABLE focus_rules DROP CONSTRAINT IF EXISTS focus_rules_window_check;
        ALTER TABLE focus_rules ADD CONSTRAINT focus_rules_window_check
            CHECK (end_time > start_time);
    END IF;
END $$;
