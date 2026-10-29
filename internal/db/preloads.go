package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PreloadRecord is the last preload event recorded for a repository.
type PreloadRecord struct {
	RepositoryID, RepoType, RepoID, Action, MetadataJSON string
	At                                                   time.Time
	// CachedFiles is how many files are cached for the commit a finished,
	// completed preload covered; -1 for other events.
	CachedFiles int64
}

// LastPreloads returns, for up to 200 live repositories, the most recent
// preload started/finished audit event.
func (s *Store) LastPreloads(ctx context.Context) ([]PreloadRecord, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT r.id,r.repo_type,r.repo_id,e.action,e.metadata_json::text,e.created_at,
		CASE WHEN e.action='repository.preload.finished' AND e.metadata_json->>'status'='completed'
			THEN (SELECT COUNT(*) FROM artifacts a WHERE a.repository_id=r.id
				AND (COALESCE(e.metadata_json->>'commit_sha','')='' OR a.commit_sha=e.metadata_json->>'commit_sha'))
			ELSE -1 END AS cached_files
		FROM repositories r JOIN LATERAL (
			SELECT action,metadata_json,created_at FROM audit_events
			WHERE target_type='repository' AND target_id=r.id AND action IN ('repository.preload.started','repository.preload.finished')
			ORDER BY created_at DESC,id DESC LIMIT 1
		) e ON TRUE
		WHERE r.deleted_at IS NULL ORDER BY e.created_at DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []PreloadRecord
	for rows.Next() {
		var r PreloadRecord
		if err := rows.Scan(&r.RepositoryID, &r.RepoType, &r.RepoID, &r.Action, &r.MetadataJSON, &r.At, &r.CachedFiles); err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, rows.Err()
}

// CachedFilesAt counts the files cached for a repository at a commit.
func (s *Store) CachedFilesAt(ctx context.Context, repoType, repoID, commit string) (int64, error) {
	var n int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE r.repo_type=$1 AND r.repo_id=$2 AND a.commit_sha=$3`, repoType, repoID, commit).Scan(&n)
	return n, err
}

// RepositoryAccess is what the access check needs about a repository.
type RepositoryAccess struct{ ID, Policy, Origin, OwnerID string }

// FindRepository returns a live repository by type and name, or ErrNotFound.
func (s *Store) FindRepository(ctx context.Context, repoType, repoID string) (RepositoryAccess, error) {
	var a RepositoryAccess
	err := s.SQL.QueryRowContext(ctx, `SELECT id,access_policy,origin,COALESCE(owner_user_id,'') FROM repositories WHERE repo_type=$1 AND repo_id=$2 AND deleted_at IS NULL`, repoType, repoID).Scan(&a.ID, &a.Policy, &a.Origin, &a.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return RepositoryAccess{}, ErrNotFound
	}
	return a, err
}

// HasReadGrant reports whether the user, directly or through a group, may read
// the repository.
func (s *Store) HasReadGrant(ctx context.Context, repositoryID, userID string) (bool, error) {
	var allowed bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM repo_grants g WHERE g.repository_id=$1 AND (
		(g.subject_type='user' AND g.subject_id=$2) OR (g.subject_type='group' AND EXISTS(SELECT 1 FROM group_members m WHERE m.group_id=g.subject_id AND m.user_id=$2))))`, repositoryID, userID).Scan(&allowed)
	return allowed, err
}
