CREATE TABLE IF NOT EXISTS download_resumptions (
    id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    path TEXT NOT NULL,
    upstream_etag TEXT NOT NULL DEFAULT '',
    total_size BIGINT NOT NULL CHECK (total_size >= 0),
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    chunk_size BIGINT NOT NULL CHECK (chunk_size > 0),
    completed_bytes BIGINT NOT NULL DEFAULT 0 CHECK (completed_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(repository_id, commit_sha, path)
);
CREATE INDEX IF NOT EXISTS download_resumptions_updated_idx ON download_resumptions(updated_at);
