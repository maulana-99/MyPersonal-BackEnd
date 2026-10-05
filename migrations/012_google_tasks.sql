-- Import marker for local tasks copied to Google Tasks (see docs/google-tasks.md).
ALTER TABLE tasks ADD COLUMN IF NOT EXISTS google_task_id TEXT;
