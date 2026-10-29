package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BlobTotals counts every blob row and sums their sizes.
func (s *Store) BlobTotals(ctx context.Context) (count, size int64, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT count(*),coalesce(sum(size_bytes),0) FROM blobs`).Scan(&count, &size)
	return count, size, err
}

// LiveBytes is the size of the blobs that artifacts still use or may use (not
// yet marked orphaned).
func (s *Store) LiveBytes(ctx context.Context) (int64, error) {
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(size_bytes),0) FROM blobs WHERE orphaned_at IS NULL`).Scan(&total)
	return total, err
}

// PinnedBytes is the size of the live blobs that eviction must not touch.
func (s *Store) PinnedBytes(ctx context.Context) (int64, error) {
	var bytes int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0) FROM blobs b
		WHERE b.orphaned_at IS NULL AND EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND EXISTS (`+PinnedArtifactSQL+`))`).Scan(&bytes)
	return bytes, err
}

// EvictionCandidate is an unpinned cached file, least recently used first.
type EvictionCandidate struct{ ArtifactID, BlobID string }

// EvictionCandidates returns up to limit unpinned artifacts, oldest access first.
func (s *Store) EvictionCandidates(ctx context.Context, limit int) ([]EvictionCandidate, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT a.id,a.blob_id FROM artifacts a
		WHERE NOT EXISTS (`+PinnedArtifactSQL+`)
		ORDER BY a.last_accessed_at ASC, a.id LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var batch []EvictionCandidate
	for rows.Next() {
		var c EvictionCandidate
		if err := rows.Scan(&c.ArtifactID, &c.BlobID); err != nil {
			return nil, err
		}
		batch = append(batch, c)
	}
	return batch, rows.Err()
}

// EvictArtifact removes an artifact and, when no other artifact uses its blob,
// marks the blob orphaned for the grace-period collector. It returns the bytes
// freed (0 when the blob is still referenced).
func (s *Store) EvictArtifact(ctx context.Context, artifactID, blobID string) (int64, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	// Lock the blob first, the same order publication uses, so a concurrent
	// fill cannot attach a new artifact between the count and the orphan mark.
	var size int64
	if err := tx.QueryRowContext(ctx, `SELECT size_bytes FROM blobs WHERE id=$1 FOR UPDATE`, blobID).Scan(&size); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, artifactID); err != nil {
		return 0, err
	}
	var refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, blobID).Scan(&refs); err != nil {
		return 0, err
	}
	var freed int64
	if refs == 0 {
		result, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1 AND orphaned_at IS NULL`, blobID)
		if err != nil {
			return 0, err
		}
		if n, _ := result.RowsAffected(); n > 0 {
			freed = size
		}
	}
	return freed, tx.Commit()
}

// OrphanedBlobIDs lists blobs that no artifact uses and that have been orphaned
// for longer than grace.
func (s *Store) OrphanedBlobIDs(ctx context.Context, grace time.Duration) ([]string, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT id FROM blobs b
		WHERE b.orphaned_at IS NOT NULL AND b.orphaned_at < now()-$1::interval
		AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id)`, grace.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// CollectOrphanedBlob deletes one orphaned blob: under a row lock it re-checks
// that the blob is still orphaned past grace and unreferenced, calls
// deleteObject with its storage key, then removes the row. A blob that gained a
// reference meanwhile is un-orphaned instead. It reports whether the blob was
// deleted.
func (s *Store) CollectOrphanedBlob(ctx context.Context, id string, grace time.Duration, deleteObject func(ctx context.Context, storageKey string) error) (bool, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var key string
	if err := tx.QueryRowContext(ctx, `SELECT storage_key FROM blobs WHERE id=$1 AND orphaned_at IS NOT NULL AND orphaned_at < now()-$2::interval FOR UPDATE`, id, grace.String()).Scan(&key); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	var refs int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, id).Scan(&refs); err != nil {
		return false, err
	}
	if refs > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, id); err != nil {
			return false, err
		}
		return false, tx.Commit()
	}
	if err := deleteObject(ctx, key); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM blobs WHERE id=$1`, id); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// RetentionJob names a periodic clean-up of old rows.
type RetentionJob string

const (
	RetainSessions           RetentionJob = "sessions"
	RetainLoginAttempts      RetentionJob = "login_attempts"
	RetainDownloads          RetentionJob = "downloads"
	RetainAbandonedDownloads RetentionJob = "downloads_abandoned"
	RetainAudit              RetentionJob = "audit_events"
)

// RunRetention applies one batch of a retention job and returns how many rows it
// changed. olderThan is the age limit for the downloads and audit jobs.
func (s *Store) RunRetention(ctx context.Context, job RetentionJob, batch int, olderThan time.Duration) (int64, error) {
	var query string
	args := []any{batch}
	switch job {
	case RetainSessions:
		query = `DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at < now() LIMIT $1)`
	case RetainLoginAttempts:
		query = `DELETE FROM login_attempts WHERE key IN (SELECT key FROM login_attempts WHERE window_start < now()-interval '1 hour' LIMIT $1)`
	case RetainAbandonedDownloads:
		// A download whose process died, or whose bookkeeping was lost, would stay
		// "in progress" forever and inflate the console's counter.
		query = `UPDATE downloads SET status='failed',error_class='abandoned',completed_at=now() WHERE id IN (SELECT id FROM downloads WHERE status='downloading' AND started_at < now()-interval '12 hours' LIMIT $1)`
	case RetainDownloads:
		query = `DELETE FROM downloads WHERE id IN (SELECT id FROM downloads WHERE started_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`
		args = append(args, olderThan.Seconds())
	case RetainAudit:
		query = `DELETE FROM audit_events WHERE id IN (SELECT id FROM audit_events WHERE created_at < now()-make_interval(secs => $2::double precision) LIMIT $1)`
		args = append(args, olderThan.Seconds())
	default:
		return 0, fmt.Errorf("unknown retention job %q", job)
	}
	result, err := s.SQL.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
