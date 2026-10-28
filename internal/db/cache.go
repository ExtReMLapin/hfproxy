package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Artifact is a cached file together with the blob that holds its bytes.
type Artifact struct {
	ID          string
	Repository  string
	Commit      string
	Path        string
	BlobID      string
	StorageKey  string
	SHA256      string
	Size        int64
	ETag        string
	ContentType string
	LastMod     sql.NullTime
}

// Caller identifies who triggered a download, for the download log. Empty
// fields are stored as NULL.
type Caller struct {
	UserID   string
	TokenID  string
	ClientIP string
}

// FindArtifact returns the cached file for a commit and path, or sql.ErrNoRows.
func (s *Store) FindArtifact(ctx context.Context, repoID, commit, file string) (Artifact, error) {
	var a Artifact
	err := s.SQL.QueryRowContext(ctx, `SELECT a.id,a.repository_id,a.commit_sha,a.path,a.blob_id,b.storage_key,b.sha256,b.size_bytes,a.etag,b.content_type,a.last_modified FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 AND a.path=$3`, repoID, commit, file).
		Scan(&a.ID, &a.Repository, &a.Commit, &a.Path, &a.BlobID, &a.StorageKey, &a.SHA256, &a.Size, &a.ETag, &a.ContentType, &a.LastMod)
	return a, err
}

// TouchArtifact records an access for LRU eviction. It writes at most once a
// minute per artifact.
func (s *Store) TouchArtifact(ctx context.Context, id string) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1 AND last_accessed_at < now()-interval '1 minute'`, id)
	return err
}

// FreshRevision returns the commit a branch or tag resolved to within maxAge,
// or "" when there is none.
func (s *Store) FreshRevision(ctx context.Context, repoID, ref string, maxAge time.Duration) (string, error) {
	var commit string
	err := s.SQL.QueryRowContext(ctx, `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2 AND resolved_at>now()-($3 * interval '1 second')`, repoID, ref, maxAge.Seconds()).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// LastKnownRevision returns the commit a branch or tag last resolved to,
// however old, or "".
func (s *Store) LastKnownRevision(ctx context.Context, repoID, ref string) (string, error) {
	var commit string
	err := s.SQL.QueryRowContext(ctx, `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2`, repoID, ref).Scan(&commit)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// SaveRevision remembers which commit a branch or tag resolved to.
func (s *Store) SaveRevision(ctx context.Context, repoID, ref, commit string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO revisions(id,repository_id,requested_ref,commit_sha,resolved_at) VALUES($1,$2,$3,$4,now())
		ON CONFLICT(repository_id,requested_ref) DO UPDATE SET commit_sha=EXCLUDED.commit_sha,resolved_at=now()`, NewID(), repoID, ref, commit)
	return err
}

// StartDownload logs a download that is in progress.
func (s *Store) StartDownload(ctx context.Context, id, repoID, commit, path string, caller Caller) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO downloads(id,repository_id,commit_sha,path,status,bytes,user_id,token_id,client_ip) VALUES($1,$2,$3,$4,'downloading',0,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''))`, id, repoID, commit, path, caller.UserID, caller.TokenID, caller.ClientIP)
	return err
}

// RecordDownload logs a finished download in a single statement.
func (s *Store) RecordDownload(ctx context.Context, repoID, commit, path, status string, bytes int64, errorClass string, caller Caller) error {
	_, err := s.SQL.ExecContext(ctx, "INSERT INTO downloads(id,repository_id,commit_sha,path,status,completed_at,bytes,error_class,user_id,token_id,client_ip) VALUES($1,$2,$3,$4,$5,now(),$6,$7,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''))", NewID(), repoID, commit, path, status, bytes, errorClass, caller.UserID, caller.TokenID, caller.ClientIP)
	return err
}

// FinishDownload closes a download started with StartDownload.
func (s *Store) FinishDownload(ctx context.Context, id string, bytes int64, errorClass string) error {
	status := "miss"
	if errorClass != "" {
		status = "failed"
	}
	_, err := s.SQL.ExecContext(ctx, "UPDATE downloads SET status=$2,completed_at=now(),bytes=$3,error_class=$4 WHERE id=$1", id, status, bytes, errorClass)
	return err
}

// BlobExists reports whether a blob with this SHA-256 is already recorded.
func (s *Store) BlobExists(ctx context.Context, sha256 string) (bool, error) {
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256=$1)`, sha256).Scan(&exists)
	return exists, err
}

// PublishInput describes a file that has been downloaded and stored.
type PublishInput struct {
	RepoID      string
	Commit      string
	Path        string
	SHA256      string
	Size        int64
	StorageKey  string
	ContentType string
	ETag        string
	LastMod     sql.NullTime
}

// PublishError says which step of PublishArtifact failed.
type PublishError struct {
	Stage string // begin, read_blob, insert_blob, restore_blob, insert_artifact, commit
	Err   error
}

func (e *PublishError) Error() string {
	return fmt.Sprintf("publish artifact (%s): %v", e.Stage, e.Err)
}
func (e *PublishError) Unwrap() error { return e.Err }

// PublishArtifact records the blob and the artifact that points to it in one
// transaction. When the blob is already known, ensureObject is called with its
// storage key so the caller can check or restore the object; its error is
// returned as is (not wrapped in a PublishError).
func (s *Store) PublishArtifact(ctx context.Context, in PublishInput, ensureObject func(ctx context.Context, storageKey string) error) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return &PublishError{"begin", err}
	}
	defer tx.Rollback()
	var blobID, existingKey string
	err = tx.QueryRowContext(ctx, `SELECT id,storage_key FROM blobs WHERE sha256=$1 FOR UPDATE`, in.SHA256).Scan(&blobID, &existingKey)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		err = tx.QueryRowContext(ctx, `INSERT INTO blobs(id,sha256,size_bytes,storage_key,content_type,verified_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(sha256) DO UPDATE SET orphaned_at=NULL RETURNING id,storage_key`, NewID(), in.SHA256, in.Size, in.StorageKey, in.ContentType).Scan(&blobID, &existingKey)
		if err != nil {
			return &PublishError{"insert_blob", err}
		}
	case err != nil:
		return &PublishError{"read_blob", err}
	default:
		if err := ensureObject(ctx, existingKey); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, blobID); err != nil {
			return &PublishError{"restore_blob", err}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,etag,last_modified,upstream_size) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(repository_id,commit_sha,path) DO UPDATE SET blob_id=EXCLUDED.blob_id,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,upstream_size=EXCLUDED.upstream_size,cached_at=now(),last_accessed_at=now()`,
		NewID(), in.RepoID, in.Commit, in.Path, blobID, in.ETag, nullTime(in.LastMod), in.Size); err != nil {
		return &PublishError{"insert_artifact", err}
	}
	if err := tx.Commit(); err != nil {
		return &PublishError{"commit", err}
	}
	return nil
}

func nullTime(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}
