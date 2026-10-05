-- Google profile picture URL, refreshed on every Google sign-in.
ALTER TABLE users ADD COLUMN IF NOT EXISTS avatar_url TEXT;
