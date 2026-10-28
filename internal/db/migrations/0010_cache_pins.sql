CREATE TABLE IF NOT EXISTS cache_pins (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL DEFAULT '',
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY(repository_id, commit_sha)
);
