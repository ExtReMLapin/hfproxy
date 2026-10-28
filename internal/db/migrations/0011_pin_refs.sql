-- Lets a pin name a branch or tag. A pin is now one of:
--   commit_sha='' and ref=''   every revision of the repository
--   commit_sha=X  and ref=''   one commit
--   commit_sha='' and ref=R    whatever commit R points to now
-- Rows written by the previous release have ref='' and keep their meaning.
ALTER TABLE cache_pins ADD COLUMN IF NOT EXISTS ref TEXT NOT NULL DEFAULT '';
ALTER TABLE cache_pins DROP CONSTRAINT IF EXISTS cache_pins_pkey;
ALTER TABLE cache_pins ADD PRIMARY KEY (repository_id, commit_sha, ref);
