package db

import (
	"context"
	"crypto/sha1" //nolint:gosec // Git object names are SHA-1; this is an identifier, not a security boundary.
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var (
	// ErrRevisionNotFound means a branch, tag or commit is not in the repository.
	ErrRevisionNotFound = errors.New("revision not found")
	// ErrParentMismatch means the branch moved since the caller read it.
	ErrParentMismatch = errors.New("the branch moved since the parent commit")
	// ErrPathConflict means a file would also be a directory (or the reverse).
	ErrPathConflict = errors.New("a path is both a file and a directory")
)

// BlobMissingError means a commit names content that was never uploaded.
type BlobMissingError struct{ SHA256 string }

func (e *BlobMissingError) Error() string { return "content " + e.SHA256 + " was not uploaded" }

// LocalCommit is one commit of a local repository.
type LocalCommit struct {
	SHA, Parent, AuthorID, AuthorName, Summary, Description string
	CreatedAt                                               time.Time
}

// LocalFile is a file of a commit with the blob that holds its bytes.
type LocalFile struct {
	ArtifactID, Path, BlobID, SHA256, StorageKey, ContentType string
	// OID is the file's identity as Git clients see it: the Git blob hash for a
	// regular file, the SHA-256 of the content for an LFS file.
	OID          string
	Size         int64
	LFS          bool
	LastModified time.Time
}

// LocalRef is a branch or a tag.
type LocalRef struct {
	Kind, Name, CommitSHA string
	UpdatedAt             time.Time
}

// CommitOpKind is what a commit operation does.
type CommitOpKind int

const (
	OpAdd CommitOpKind = iota
	OpDeleteFile
	OpDeleteFolder
)

// CommitOp is one operation of a commit, applied in order.
type CommitOp struct {
	Kind CommitOpKind
	Path string
	// For OpAdd: the content, already stored (SHA256 names its blob).
	SHA256, OID, ContentType string
	Size                     int64
	LFS                      bool
}

// CommitInput describes a commit to a branch.
type CommitInput struct {
	RepositoryID, Branch, ParentCommit string
	AuthorID, AuthorName               string
	Summary, Description               string
	Ops                                []CommitOp
}

// CreateLocalRepository registers a repository published on this proxy, with an
// empty first commit on the branch main. A repository that was cached from the
// Hub and then removed is taken over; one that is live is ErrExists.
func (s *Store) CreateLocalRepository(ctx context.Context, repoType, repoID, policy, ownerID string) (string, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var id string
	var deleted sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT id,deleted_at FROM repositories WHERE repo_type=$1 AND repo_id=$2 FOR UPDATE`, repoType, repoID).Scan(&id, &deleted)
	switch {
	case err == nil && !deleted.Valid:
		return "", ErrExists
	case err == nil:
		if err := releaseRepositoryBlobs(ctx, tx, id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_commits WHERE repository_id=$1`, id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM local_refs WHERE repository_id=$1`, id); err != nil {
			return "", err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE repositories SET origin='local',owner_user_id=NULLIF($2,''),created_by=NULLIF($2,''),access_policy=$3,deleted_at=NULL,created_at=now(),updated_at=now() WHERE id=$1`, id, ownerID, policy); err != nil {
			return "", err
		}
	case errors.Is(err, sql.ErrNoRows):
		id = NewID()
		if _, err := tx.ExecContext(ctx, `INSERT INTO repositories(id,repo_type,repo_id,access_policy,created_by,origin,owner_user_id) VALUES($1,$2,$3,$4,NULLIF($5,''),'local',NULLIF($5,''))`, id, repoType, repoID, policy, ownerID); err != nil {
			return "", err
		}
	default:
		return "", err
	}
	now := time.Now().UTC()
	sha := commitName("initial commit", id, "", nil, now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_commits(repository_id,sha,parent_sha,author_id,author_name,summary,description,created_at) VALUES($1,$2,'',NULLIF($3,''),'','initial commit','',$4)`, id, sha, ownerID, now); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_refs(repository_id,kind,name,commit_sha) VALUES($1,'branch','main',$2)`, id, sha); err != nil {
		return "", err
	}
	return id, tx.Commit()
}

// DeleteLocalRepository removes a local repository with its history. The blobs
// nothing else uses are orphaned for the collector. It returns ErrNotFound for
// an unknown id.
func (s *Store) DeleteLocalRepository(ctx context.Context, id string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var origin string
	if err := tx.QueryRowContext(ctx, `SELECT origin FROM repositories WHERE id=$1 AND deleted_at IS NULL FOR UPDATE`, id).Scan(&origin); errors.Is(err, sql.ErrNoRows) || origin != "local" {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	if err := releaseRepositoryBlobs(ctx, tx, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repositories WHERE id=$1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// releaseRepositoryBlobs deletes a repository's files and orphans the blobs no
// other repository uses. Blobs are locked before artifacts are touched, the order
// publication uses, so that a concurrent fill cannot deadlock with it.
func releaseRepositoryBlobs(ctx context.Context, tx *sql.Tx, repositoryID string) error {
	var blobIDs []string
	rows, err := tx.QueryContext(ctx, `SELECT id FROM blobs WHERE id IN (SELECT blob_id FROM artifacts WHERE repository_id=$1) ORDER BY id FOR UPDATE`, repositoryID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		blobIDs = append(blobIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE repository_id=$1`, repositoryID); err != nil {
		return err
	}
	if len(blobIDs) == 0 {
		return nil
	}
	_, err = tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=ANY($1) AND orphaned_at IS NULL AND NOT EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=blobs.id)`, blobIDs)
	return err
}

// RegisterUploadedBlob records content that was just stored but is not part of
// a commit yet. It is marked orphaned in the future, so the collector keeps it
// for the time given (the commit that names it comes right after the upload)
// and a commit makes it permanent. Known content is left as it is.
func (s *Store) RegisterUploadedBlob(ctx context.Context, sha256, storageKey, contentType string, size int64, keep time.Duration) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO blobs(id,sha256,size_bytes,storage_key,content_type,verified_at,orphaned_at)
		VALUES($1,$2,$3,$4,$5,now(),now()+$6::interval)
		ON CONFLICT(sha256) DO UPDATE SET verified_at=now(),
			orphaned_at=CASE WHEN blobs.orphaned_at IS NOT NULL AND blobs.orphaned_at < now()+$6::interval THEN now()+$6::interval ELSE blobs.orphaned_at END`, NewID(), sha256, size, storageKey, contentType, keep.String())
	return err
}

// BlobBySHA256 returns what the proxy knows of content, or ErrNotFound.
func (s *Store) BlobBySHA256(ctx context.Context, sha256 string) (size int64, storageKey string, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT size_bytes,storage_key FROM blobs WHERE sha256=$1`, sha256).Scan(&size, &storageKey)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", ErrNotFound
	}
	return size, storageKey, err
}

// LocalFilesAt lists the files of a commit, ordered by path.
func (s *Store) LocalFilesAt(ctx context.Context, repositoryID, sha string) ([]LocalFile, error) {
	return localFilesAt(ctx, s.SQL, repositoryID, sha, "")
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func localFilesAt(ctx context.Context, q queryer, repositoryID, sha, lock string) ([]LocalFile, error) {
	rows, err := q.QueryContext(ctx, `SELECT a.id,a.path,a.blob_id,b.sha256,b.storage_key,b.content_type,a.etag,b.size_bytes,a.lfs,COALESCE(a.last_modified,a.cached_at)
		FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 ORDER BY a.path`+lock, repositoryID, sha)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var files []LocalFile
	for rows.Next() {
		var f LocalFile
		if err := rows.Scan(&f.ArtifactID, &f.Path, &f.BlobID, &f.SHA256, &f.StorageKey, &f.ContentType, &f.OID, &f.Size, &f.LFS, &f.LastModified); err != nil {
			return nil, err
		}
		files = append(files, f)
	}
	return files, rows.Err()
}

// ResolveLocalRevision turns a branch, a tag or a commit hash into the commit it
// names, or ErrRevisionNotFound. "refs/heads/x" and "refs/tags/x" are accepted.
func (s *Store) ResolveLocalRevision(ctx context.Context, repositoryID, revision string) (string, error) {
	return resolveLocalRevision(ctx, s.SQL, repositoryID, revision)
}

type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func resolveLocalRevision(ctx context.Context, q rowQueryer, repositoryID, revision string) (string, error) {
	kinds := []string{"branch", "tag"}
	name := revision
	switch {
	case strings.HasPrefix(revision, "refs/heads/"):
		kinds, name = []string{"branch"}, strings.TrimPrefix(revision, "refs/heads/")
	case strings.HasPrefix(revision, "refs/tags/"):
		kinds, name = []string{"tag"}, strings.TrimPrefix(revision, "refs/tags/")
	}
	for _, kind := range kinds {
		var sha string
		err := q.QueryRowContext(ctx, `SELECT commit_sha FROM local_refs WHERE repository_id=$1 AND kind=$2 AND name=$3`, repositoryID, kind, name).Scan(&sha)
		if err == nil {
			return sha, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
	}
	var sha string
	err := q.QueryRowContext(ctx, `SELECT sha FROM local_commits WHERE repository_id=$1 AND sha=$2`, repositoryID, strings.ToLower(revision)).Scan(&sha)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrRevisionNotFound
	}
	return sha, err
}

// LocalCommitBySHA returns one commit, or ErrRevisionNotFound.
func (s *Store) LocalCommitBySHA(ctx context.Context, repositoryID, sha string) (LocalCommit, error) {
	var c LocalCommit
	err := s.SQL.QueryRowContext(ctx, `SELECT sha,parent_sha,COALESCE(author_id,''),author_name,summary,description,created_at FROM local_commits WHERE repository_id=$1 AND sha=$2`, repositoryID, sha).
		Scan(&c.SHA, &c.Parent, &c.AuthorID, &c.AuthorName, &c.Summary, &c.Description, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrRevisionNotFound
	}
	return c, err
}

// LocalHistory lists the commits that lead to sha, newest first.
func (s *Store) LocalHistory(ctx context.Context, repositoryID, sha string, limit int) ([]LocalCommit, error) {
	rows, err := s.SQL.QueryContext(ctx, `WITH RECURSIVE history AS (
			SELECT c.*,1 AS depth FROM local_commits c WHERE c.repository_id=$1 AND c.sha=$2
			UNION ALL
			SELECT c.*,h.depth+1 FROM local_commits c JOIN history h ON c.repository_id=h.repository_id AND c.sha=h.parent_sha WHERE h.depth<$3)
		SELECT sha,parent_sha,COALESCE(author_id,''),author_name,summary,description,created_at FROM history ORDER BY depth`, repositoryID, sha, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var commits []LocalCommit
	for rows.Next() {
		var c LocalCommit
		if err := rows.Scan(&c.SHA, &c.Parent, &c.AuthorID, &c.AuthorName, &c.Summary, &c.Description, &c.CreatedAt); err != nil {
			return nil, err
		}
		commits = append(commits, c)
	}
	return commits, rows.Err()
}

// LocalRefs lists the branches and tags of a repository.
func (s *Store) LocalRefs(ctx context.Context, repositoryID string) ([]LocalRef, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT kind,name,commit_sha,updated_at FROM local_refs WHERE repository_id=$1 ORDER BY kind,name`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []LocalRef
	for rows.Next() {
		var r LocalRef
		if err := rows.Scan(&r.Kind, &r.Name, &r.CommitSHA, &r.UpdatedAt); err != nil {
			return nil, err
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// CreateLocalRef makes a branch or a tag point at the commit a revision names.
func (s *Store) CreateLocalRef(ctx context.Context, repositoryID, kind, name, fromRevision string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM repositories WHERE id=$1 FOR UPDATE`, repositoryID); err != nil {
		return err
	}
	sha, err := resolveLocalRevision(ctx, tx, repositoryID, fromRevision)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO local_refs(repository_id,kind,name,commit_sha) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repositoryID, kind, name, sha)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrExists
	}
	return tx.Commit()
}

// DeleteLocalRef removes a branch or a tag. The branch main cannot be removed.
func (s *Store) DeleteLocalRef(ctx context.Context, repositoryID, kind, name string) error {
	if kind == "branch" && name == "main" {
		return fmt.Errorf("the branch main cannot be deleted")
	}
	result, err := s.SQL.ExecContext(ctx, `DELETE FROM local_refs WHERE repository_id=$1 AND kind=$2 AND name=$3`, repositoryID, kind, name)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrRevisionNotFound
	}
	return nil
}

// OwnerLocalBytes sums the size of the distinct blobs used by the local
// repositories a user owns, for the storage quota.
func (s *Store) OwnerLocalBytes(ctx context.Context, ownerID string) (int64, error) {
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0) FROM blobs b WHERE b.id IN (
			SELECT a.blob_id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE r.origin='local' AND r.owner_user_id=$1)`, ownerID).Scan(&total)
	return total, err
}

// LocalRepositoryBytes sums the size of the distinct blobs of one repository.
func (s *Store) LocalRepositoryBytes(ctx context.Context, repositoryID string) (int64, error) {
	var total int64
	err := s.SQL.QueryRowContext(ctx, `SELECT COALESCE(SUM(b.size_bytes),0) FROM blobs b WHERE b.id IN (SELECT blob_id FROM artifacts WHERE repository_id=$1)`, repositoryID).Scan(&total)
	return total, err
}

// CommitLocal applies a commit's operations to the tip of a branch and moves the
// branch to the new commit. Nothing is written when it fails.
func (s *Store) CommitLocal(ctx context.Context, in CommitInput) (LocalCommit, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return LocalCommit{}, err
	}
	defer tx.Rollback()
	// One commit at a time per repository.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM repositories WHERE id=$1 AND origin='local' AND deleted_at IS NULL FOR UPDATE`, in.RepositoryID); err != nil {
		return LocalCommit{}, err
	}
	var head string
	err = tx.QueryRowContext(ctx, `SELECT commit_sha FROM local_refs WHERE repository_id=$1 AND kind='branch' AND name=$2`, in.RepositoryID, in.Branch).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalCommit{}, ErrRevisionNotFound
	}
	if err != nil {
		return LocalCommit{}, err
	}
	if in.ParentCommit != "" && !strings.EqualFold(in.ParentCommit, head) {
		return LocalCommit{}, ErrParentMismatch
	}

	// Lock the blobs the commit adds before touching artifacts (see publication).
	var wanted []string
	for _, op := range in.Ops {
		if op.Kind == OpAdd {
			wanted = append(wanted, op.SHA256)
		}
	}
	sort.Strings(wanted)
	blobs := map[string]struct {
		id   string
		size int64
	}{}
	if len(wanted) > 0 {
		rows, err := tx.QueryContext(ctx, `SELECT id,sha256,size_bytes FROM blobs WHERE sha256=ANY($1) ORDER BY id FOR UPDATE`, wanted)
		if err != nil {
			return LocalCommit{}, err
		}
		for rows.Next() {
			var id, sum string
			var size int64
			if err := rows.Scan(&id, &sum, &size); err != nil {
				rows.Close()
				return LocalCommit{}, err
			}
			blobs[sum] = struct {
				id   string
				size int64
			}{id, size}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return LocalCommit{}, err
		}
		rows.Close()
	}

	base, err := localFilesAt(ctx, tx, in.RepositoryID, head, "")
	if err != nil {
		return LocalCommit{}, err
	}
	tree := make(map[string]LocalFile, len(base)+len(in.Ops))
	for _, f := range base {
		tree[f.Path] = f
	}
	now := time.Now().UTC()
	for _, op := range in.Ops {
		switch op.Kind {
		case OpAdd:
			blob, ok := blobs[op.SHA256]
			if !ok {
				return LocalCommit{}, &BlobMissingError{SHA256: op.SHA256}
			}
			if op.Size != blob.size {
				return LocalCommit{}, fmt.Errorf("content %s is %d bytes, not %d", op.SHA256, blob.size, op.Size)
			}
			tree[op.Path] = LocalFile{Path: op.Path, BlobID: blob.id, SHA256: op.SHA256, ContentType: op.ContentType, OID: op.OID, Size: blob.size, LFS: op.LFS, LastModified: now}
		case OpDeleteFile:
			delete(tree, op.Path)
		case OpDeleteFolder:
			prefix := strings.TrimSuffix(op.Path, "/") + "/"
			for path := range tree {
				if strings.HasPrefix(path, prefix) {
					delete(tree, path)
				}
			}
		}
	}
	if err := checkFileDirConflicts(tree); err != nil {
		return LocalCommit{}, err
	}

	paths := make([]string, 0, len(tree))
	for path := range tree {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	manifest := make([]string, 0, len(paths))
	for _, path := range paths {
		f := tree[path]
		manifest = append(manifest, path+"\x00"+f.SHA256)
	}
	sha := commitName(in.Summary+"\n"+in.Description, in.RepositoryID, head, manifest, now)
	if _, err := tx.ExecContext(ctx, `INSERT INTO local_commits(repository_id,sha,parent_sha,author_id,author_name,summary,description,created_at) VALUES($1,$2,$3,NULLIF($4,''),$5,$6,$7,$8)`,
		in.RepositoryID, sha, head, in.AuthorID, in.AuthorName, in.Summary, in.Description, now); err != nil {
		return LocalCommit{}, err
	}
	if len(paths) > 0 {
		ids := make([]string, len(paths))
		blobIDs := make([]string, len(paths))
		etags := make([]string, len(paths))
		lfs := make([]bool, len(paths))
		times := make([]time.Time, len(paths))
		for i, path := range paths {
			f := tree[path]
			ids[i], blobIDs[i], etags[i], lfs[i], times[i] = NewID(), f.BlobID, f.OID, f.LFS, f.LastModified
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,etag,last_modified,upstream_size,lfs)
			SELECT t.id,$1,$2,t.path,t.blob_id,t.etag,t.modified,b.size_bytes,t.lfs
			FROM unnest($3::text[],$4::text[],$5::text[],$6::text[],$7::boolean[],$8::timestamptz[]) AS t(id,path,blob_id,etag,lfs,modified)
			JOIN blobs b ON b.id=t.blob_id`, in.RepositoryID, sha, ids, paths, blobIDs, etags, lfs, times); err != nil {
			return LocalCommit{}, err
		}
	}
	if len(wanted) > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=NULL WHERE sha256=ANY($1) AND orphaned_at IS NOT NULL`, wanted); err != nil {
			return LocalCommit{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE local_refs SET commit_sha=$3,updated_at=now() WHERE repository_id=$1 AND kind='branch' AND name=$2`, in.RepositoryID, in.Branch, sha); err != nil {
		return LocalCommit{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE repositories SET updated_at=now() WHERE id=$1`, in.RepositoryID); err != nil {
		return LocalCommit{}, err
	}
	if err := tx.Commit(); err != nil {
		return LocalCommit{}, err
	}
	return LocalCommit{SHA: sha, Parent: head, AuthorID: in.AuthorID, AuthorName: in.AuthorName, Summary: in.Summary, Description: in.Description, CreatedAt: now}, nil
}

// checkFileDirConflicts refuses a tree where a path is a file and also the
// directory of another path.
func checkFileDirConflicts(tree map[string]LocalFile) error {
	for path := range tree {
		for i := 0; i < len(path); i++ {
			if path[i] == '/' {
				if _, isFile := tree[path[:i]]; isFile {
					return fmt.Errorf("%w: %s", ErrPathConflict, path[:i])
				}
			}
		}
	}
	return nil
}

// commitName derives a Git-looking 40-hex commit name from what a commit holds.
func commitName(message, repositoryID, parent string, manifest []string, at time.Time) string {
	h := sha1.New() //nolint:gosec // see the import.
	fmt.Fprintf(h, "commit %s\nparent %s\ntime %d\n%s\n", repositoryID, parent, at.UnixNano(), message)
	for _, line := range manifest {
		h.Write([]byte(line))
		h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ErrAmbiguousNamespace means a user and a group have the same name.
var ErrAmbiguousNamespace = errors.New("a user and a group have this name")

// Namespace is the owner of a repository name: a user or a group.
type Namespace struct{ Kind, ID, Name string }

// ResolveNamespace finds the user or group called name, or returns ErrNotFound.
func (s *Store) ResolveNamespace(ctx context.Context, name string) (Namespace, error) {
	var found []Namespace
	rows, err := s.SQL.QueryContext(ctx, `SELECT 'user',id,username FROM users WHERE username=$1 AND status='active'
		UNION ALL SELECT 'group',id,name FROM groups WHERE name=$1`, name)
	if err != nil {
		return Namespace{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var n Namespace
		if err := rows.Scan(&n.Kind, &n.ID, &n.Name); err != nil {
			return Namespace{}, err
		}
		found = append(found, n)
	}
	if err := rows.Err(); err != nil {
		return Namespace{}, err
	}
	switch len(found) {
	case 0:
		return Namespace{}, ErrNotFound
	case 1:
		return found[0], nil
	}
	return Namespace{}, ErrAmbiguousNamespace
}

// IsGroupMember reports whether a user belongs to a group.
func (s *Store) IsGroupMember(ctx context.Context, groupID, userID string) (bool, error) {
	var member bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM group_members WHERE group_id=$1 AND user_id=$2)`, groupID, userID).Scan(&member)
	return member, err
}

// UserGroupNames lists the names of the groups a user belongs to.
func (s *Store) UserGroupNames(ctx context.Context, userID string) ([]string, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT g.name FROM groups g JOIN group_members m ON m.group_id=g.id WHERE m.user_id=$1 ORDER BY g.name`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// OwnedContent returns which of the listed contents the owner's published
// repositories already hold: those cost nothing more against the quota.
func (s *Store) OwnedContent(ctx context.Context, ownerID string, sha256s []string) (map[string]bool, error) {
	owned := map[string]bool{}
	if len(sha256s) == 0 {
		return owned, nil
	}
	rows, err := s.SQL.QueryContext(ctx, `SELECT b.sha256 FROM blobs b WHERE b.sha256=ANY($1)
		AND EXISTS (SELECT 1 FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE a.blob_id=b.id AND r.origin='local' AND r.owner_user_id=$2)`, sha256s, ownerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var sum string
		if err := rows.Scan(&sum); err != nil {
			return nil, err
		}
		owned[sum] = true
	}
	return owned, rows.Err()
}

// BlobIsRetained reports whether content is stored and will stay: it is used by
// a file, or was uploaded recently enough that its commit is still to come.
func (s *Store) BlobIsRetained(ctx context.Context, sha256 string) (bool, error) {
	var retained bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256=$1 AND (orphaned_at IS NULL OR orphaned_at > now()))`, sha256).Scan(&retained)
	return retained, err
}

// LocalFileAt returns one file of a commit, or ErrNotFound.
func (s *Store) LocalFileAt(ctx context.Context, repositoryID, sha, filePath string) (LocalFile, error) {
	var f LocalFile
	err := s.SQL.QueryRowContext(ctx, `SELECT a.id,a.path,a.blob_id,b.sha256,b.storage_key,b.content_type,a.etag,b.size_bytes,a.lfs,COALESCE(a.last_modified,a.cached_at)
		FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 AND a.path=$3`, repositoryID, sha, filePath).
		Scan(&f.ArtifactID, &f.Path, &f.BlobID, &f.SHA256, &f.StorageKey, &f.ContentType, &f.OID, &f.Size, &f.LFS, &f.LastModified)
	if errors.Is(err, sql.ErrNoRows) {
		return f, ErrNotFound
	}
	return f, err
}
