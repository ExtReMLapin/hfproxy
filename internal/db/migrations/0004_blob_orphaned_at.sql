ALTER TABLE blobs ADD COLUMN IF NOT EXISTS orphaned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS blobs_orphaned_idx ON blobs(orphaned_at) WHERE orphaned_at IS NOT NULL;
