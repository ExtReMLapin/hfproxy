-- Hits and misses per repository, kept up to date as downloads finish. The
-- console used to count them by reading the whole downloads table, which grows
-- with every request and is only pruned by retention.
CREATE TABLE IF NOT EXISTS repository_download_counts (
    repository_id TEXT PRIMARY KEY REFERENCES repositories(id) ON DELETE CASCADE,
    hits BIGINT NOT NULL DEFAULT 0,
    misses BIGINT NOT NULL DEFAULT 0
);
INSERT INTO repository_download_counts(repository_id,hits,misses)
SELECT repository_id, COUNT(*) FILTER (WHERE status='hit'), COUNT(*) FILTER (WHERE status='miss')
FROM downloads GROUP BY repository_id
ON CONFLICT (repository_id) DO NOTHING;
