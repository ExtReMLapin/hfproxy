-- Per-user daily download quota in bytes. NULL means "use the default from the
-- settings", 0 means unlimited for this user.
ALTER TABLE users ADD COLUMN IF NOT EXISTS daily_quota_bytes BIGINT;
-- The quota check sums what a user downloaded over the last 24 hours.
CREATE INDEX IF NOT EXISTS downloads_user_started_idx ON downloads(user_id, started_at) WHERE user_id IS NOT NULL;
