-- Downloads in progress are few, but the console counts them and the retention
-- job closes abandoned ones; a partial index keeps both from scanning the table.
CREATE INDEX IF NOT EXISTS downloads_active_idx ON downloads(started_at DESC) WHERE status='downloading';
