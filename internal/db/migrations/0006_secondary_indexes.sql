CREATE INDEX IF NOT EXISTS artifacts_blob_idx ON artifacts(blob_id);
CREATE INDEX IF NOT EXISTS downloads_started_idx ON downloads(started_at);
CREATE INDEX IF NOT EXISTS access_tokens_user_idx ON access_tokens(user_id);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(user_id);
CREATE INDEX IF NOT EXISTS group_members_user_idx ON group_members(user_id);
