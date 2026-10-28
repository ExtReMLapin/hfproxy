DELETE FROM repo_grants g USING repositories r WHERE g.repository_id=r.id AND r.deleted_at IS NOT NULL;
