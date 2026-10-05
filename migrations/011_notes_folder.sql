-- Drive folder holding the user's notes (see docs/google-notes.md).
ALTER TABLE integrations_google ADD COLUMN IF NOT EXISTS notes_folder_id TEXT;
