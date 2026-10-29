package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// visibleRepository is a condition on a repository aliased "r" that holds for
// the repositories a user may see: all of them for an administrator, public
// ones, and allowlisted ones the user (or one of their groups) is granted.
func visibleRepository(roleParam, userParam int) string {
	return fmt.Sprintf(`($%[1]d='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$%[2]d) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$%[2]d))))`, roleParam, userParam)
}

// Stats are the cache figures a user may see.
type Stats struct {
	Repositories, Artifacts, Bytes, Hits, Misses, Active int64
}

// CacheStats counts repositories, cached files, hits and misses visible to the
// user. Hits and misses are the totals kept per repository since it was first
// served (they do not shrink when old download records are pruned). A failure to count active downloads is not an error: it reads as zero.
func (s *Store) CacheStats(ctx context.Context, role, userID string) (Stats, error) {
	var st Stats
	visible := visibleRepository(1, 2)
	if err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM repositories r WHERE r.deleted_at IS NULL AND `+visible, role, userID).Scan(&st.Repositories); err != nil {
		return st, err
	}
	if err := s.SQL.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE r.deleted_at IS NULL AND `+visible, role, userID).Scan(&st.Artifacts, &st.Bytes); err != nil {
		return st, err
	}
	if err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(c.hits),0),COALESCE(SUM(c.misses),0) FROM repository_download_counts c JOIN repositories r ON r.id=c.repository_id WHERE r.deleted_at IS NULL AND `+visible, role, userID).Scan(&st.Hits, &st.Misses); err != nil {
		return st, err
	}
	_ = s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE d.status='downloading' AND r.deleted_at IS NULL AND `+visible, role, userID).Scan(&st.Active)
	return st, nil
}

// ActiveDownload is a download in progress.
type ActiveDownload struct {
	ID, RepoType, RepoID, Path string
	Bytes                      int64
	StartedAt                  time.Time
}

// ActiveDownloads lists the 50 most recent downloads in progress visible to the user.
func (s *Store) ActiveDownloads(ctx context.Context, role, userID string) ([]ActiveDownload, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT d.id,r.repo_type,r.repo_id,d.path,d.bytes,d.started_at
		FROM downloads d JOIN repositories r ON r.id=d.repository_id
		WHERE d.status='downloading' AND r.deleted_at IS NULL AND `+visibleRepository(1, 2)+`
		ORDER BY d.started_at DESC LIMIT 50`, role, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []ActiveDownload
	for rows.Next() {
		var d ActiveDownload
		if err := rows.Scan(&d.ID, &d.RepoType, &d.RepoID, &d.Path, &d.Bytes, &d.StartedAt); err != nil {
			return nil, err
		}
		list = append(list, d)
	}
	return list, rows.Err()
}

// CacheEntry is a cached file as shown in the console.
type CacheEntry struct {
	ID, RepoType, RepoID, Commit, Path, SHA256, ETag string
	Size                                             int64
	CachedAt, AccessedAt                             time.Time
}

// SearchCache lists cached files visible to the user, newest first, filtered by
// repository name and a case-insensitive path substring ("" matches all).
func (s *Store) SearchCache(ctx context.Context, role, userID, repoFilter, pathFilter string, limit int) ([]CacheEntry, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT a.id,r.repo_type,r.repo_id,a.commit_sha,a.path,b.size_bytes,b.sha256,a.etag,a.cached_at,a.last_accessed_at
		FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id
		WHERE r.deleted_at IS NULL AND `+visibleRepository(1, 2)+`
		AND ($3='' OR r.repo_id=$3) AND ($4='' OR strpos(lower(a.path),lower($4))>0) ORDER BY a.cached_at DESC LIMIT $5`, role, userID, repoFilter, pathFilter, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var list []CacheEntry
	for rows.Next() {
		var e CacheEntry
		if rows.Scan(&e.ID, &e.RepoType, &e.RepoID, &e.Commit, &e.Path, &e.Size, &e.SHA256, &e.ETag, &e.CachedAt, &e.AccessedAt) == nil {
			list = append(list, e)
		}
	}
	return list, rows.Err()
}

// ArtifactVisible reports whether a cached file belongs to a repository the user may see.
func (s *Store) ArtifactVisible(ctx context.Context, id, role, userID string) (bool, error) {
	var visible bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE a.id=$1 AND r.deleted_at IS NULL AND `+visibleRepository(2, 3)+`)`, id, role, userID).Scan(&visible)
	return visible, err
}

// PurgeEstimate counts the cached files and bytes a purge with these filters
// would remove from the repositories visible to the user.
func (s *Store) PurgeEstimate(ctx context.Context, role, userID, repoFilter, pathFilter string) (count, size int64, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0) AND r.deleted_at IS NULL AND `+visibleRepository(3, 4), repoFilter, pathFilter, role, userID).Scan(&count, &size)
	return count, size, err
}

// PurgeCandidates lists the ids of the cached files a purge with these filters would remove.
func (s *Store) PurgeCandidates(ctx context.Context, role, userID, repoFilter, pathFilter string) ([]string, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT a.id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0) AND r.deleted_at IS NULL AND `+visibleRepository(3, 4), repoFilter, pathFilter, role, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// PurgeArtifact removes a cached file's catalog entry and marks its blob
// orphaned. The blob row and object stay until the grace-period GC removes
// them, so a purge racing a re-download cannot delete the new object. It
// returns ErrNotFound for an unknown id.
func (s *Store) PurgeArtifact(ctx context.Context, id string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock the blob before the artifact, the order publication and eviction use:
	// the reverse order deadlocks with a fill publishing the same blob. The
	// artifact can be re-published onto another blob between the lookup and the
	// lock, so the delete names the blob it locked and the lookup is repeated
	// when that no longer matches.
	for attempt := 0; ; attempt++ {
		var blobID string
		if err := tx.QueryRowContext(ctx, `SELECT blob_id FROM artifacts WHERE id=$1`, id).Scan(&blobID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if _, err := tx.ExecContext(ctx, `SELECT 1 FROM blobs WHERE id=$1 FOR UPDATE`, blobID); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1 AND blob_id=$2`, id, blobID)
		if err != nil {
			return err
		}
		if n, _ := result.RowsAffected(); n == 0 {
			if attempt >= 3 {
				return fmt.Errorf("purge artifact %s: it keeps changing", id)
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1`, blobID); err != nil {
			return err
		}
		break
	}
	return tx.Commit()
}

// AuditEvent is a row of the audit log with the actor's username.
type AuditEvent struct {
	ID, Actor, Action, TargetType, TargetID, Metadata, RequestID string
	CreatedAt                                                    time.Time
}

// AuditCursor is the keyset position of an audit event.
type AuditCursor struct {
	At time.Time
	ID string
}

// ListAudit returns up to limit audit events, newest first, starting before the
// (created_at, id) keyset when before is not nil.
func (s *Store) ListAudit(ctx context.Context, before *AuditCursor, limit int) ([]AuditEvent, error) {
	query := `SELECT e.id,COALESCE(u.username,''),e.action,e.target_type,e.target_id,e.metadata_json,e.request_id,e.created_at FROM audit_events e LEFT JOIN users u ON u.id=e.actor_user_id `
	args := []any{}
	if before != nil {
		query += `WHERE (e.created_at,e.id) < ($1,$2) `
		args = append(args, before.At, before.ID)
	}
	query += `ORDER BY e.created_at DESC,e.id DESC LIMIT $` + itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []AuditEvent
	for rows.Next() {
		var e AuditEvent
		if rows.Scan(&e.ID, &e.Actor, &e.Action, &e.TargetType, &e.TargetID, &e.Metadata, &e.RequestID, &e.CreatedAt) == nil {
			events = append(events, e)
		}
	}
	return events, rows.Err()
}
