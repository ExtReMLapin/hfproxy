package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Repository is a registered Hub repository and its access policy.
type Repository struct {
	ID, Type, RepoID, Policy string
	// Origin is "hub" for a repository cached from the Hub and "local" for one
	// published on this proxy; OwnerID is who published it.
	Origin, OwnerID      string
	CreatedAt, UpdatedAt time.Time
}

// Grant gives a user or group a permission on a repository.
type Grant struct{ SubjectType, SubjectID, Permission string }

// ListRepositories returns the repositories a user may see (all of them for an
// administrator), ordered by type, name and id, starting after the keyset when
// after is not nil.
func (s *Store) ListRepositories(ctx context.Context, role, userID string, after *[3]string, limit int) ([]Repository, error) {
	args := []any{role, userID}
	cursorWhere := ""
	if after != nil {
		cursorWhere = " AND (r.repo_type,r.repo_id,r.id) > ($3,$4,$5)"
		args = append(args, after[0], after[1], after[2])
	}
	args = append(args, limit)
	rows, err := s.SQL.QueryContext(ctx, `SELECT r.id,r.repo_type,r.repo_id,r.access_policy,r.origin,COALESCE(r.owner_user_id,''),r.created_at,r.updated_at
		FROM repositories r WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`+cursorWhere+`
		ORDER BY r.repo_type,r.repo_id,r.id LIMIT $`+itoa(len(args)), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repos []Repository
	for rows.Next() {
		var r Repository
		if rows.Scan(&r.ID, &r.Type, &r.RepoID, &r.Policy, &r.Origin, &r.OwnerID, &r.CreatedAt, &r.UpdatedAt) == nil {
			repos = append(repos, r)
		}
	}
	return repos, rows.Err()
}

// UpsertRepository registers a repository cached from the Hub or updates its
// policy and restores it when it was deleted. It returns the repository's id.
// A name taken by a repository published on this proxy is ErrExists.
func (s *Store) UpsertRepository(ctx context.Context, repoType, repoID, policy, createdBy string) (string, error) {
	var id string
	err := s.SQL.QueryRowContext(ctx, `INSERT INTO repositories(id,repo_type,repo_id,access_policy,created_by) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy,deleted_at=NULL,updated_at=now() WHERE repositories.origin='hub' RETURNING id`, NewID(), repoType, repoID, policy, createdBy).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrExists
	}
	return id, err
}

// GetRepository returns a live repository by id, or ErrNotFound.
func (s *Store) GetRepository(ctx context.Context, id string) (Repository, error) {
	r := Repository{ID: id}
	err := s.SQL.QueryRowContext(ctx, `SELECT repo_type,repo_id,access_policy,origin,COALESCE(owner_user_id,''),created_at,updated_at FROM repositories WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&r.Type, &r.RepoID, &r.Policy, &r.Origin, &r.OwnerID, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Repository{}, ErrNotFound
	}
	return r, err
}

// SetRepositoryPolicy changes a live repository's policy, or returns ErrNotFound.
func (s *Store) SetRepositoryPolicy(ctx context.Context, id, policy string) error {
	result, err := s.SQL.ExecContext(ctx, `UPDATE repositories SET access_policy=$2,updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id, policy)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteRepository soft-deletes a repository and removes its grants in one
// transaction, or returns ErrNotFound.
func (s *Store) DeleteRepository(ctx context.Context, id string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return &StageError{"begin", err}
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE repositories SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
	if err != nil {
		return &StageError{"delete", err}
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM repo_grants WHERE repository_id=$1`, id); err != nil {
		return &StageError{"grants", err}
	}
	if err := tx.Commit(); err != nil {
		return &StageError{"commit", err}
	}
	return nil
}

// ListGrants returns a repository's grants.
func (s *Store) ListGrants(ctx context.Context, repositoryID string) ([]Grant, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT subject_type,subject_id,permission FROM repo_grants WHERE repository_id=$1 ORDER BY subject_type,subject_id`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var grants []Grant
	for rows.Next() {
		var g Grant
		if rows.Scan(&g.SubjectType, &g.SubjectID, &g.Permission) == nil {
			grants = append(grants, g)
		}
	}
	return grants, rows.Err()
}

// RepositoryExists reports whether a live repository has this id.
func (s *Store) RepositoryExists(ctx context.Context, id string) (bool, error) {
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=$1 AND deleted_at IS NULL)`, id).Scan(&exists)
	return exists, err
}

// GrantSubjectExists reports whether a user ("user") or group ("group") exists.
func (s *Store) GrantSubjectExists(ctx context.Context, subjectType, subjectID string) (bool, error) {
	table := "users"
	if subjectType == "group" {
		table = "groups"
	}
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, subjectID).Scan(&exists)
	return exists, err
}

// UpsertGrant sets a subject's permission on a repository.
func (s *Store) UpsertGrant(ctx context.Context, repositoryID, subjectType, subjectID, permission string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO repo_grants(repository_id,subject_type,subject_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT(repository_id,subject_type,subject_id) DO UPDATE SET permission=EXCLUDED.permission`, repositoryID, subjectType, subjectID, permission)
	return err
}

// DeleteGrant removes a subject's permission on a repository.
func (s *Store) DeleteGrant(ctx context.Context, repositoryID, subjectType, subjectID string) error {
	_, err := s.SQL.ExecContext(ctx, `DELETE FROM repo_grants WHERE repository_id=$1 AND subject_type=$2 AND subject_id=$3`, repositoryID, subjectType, subjectID)
	return err
}
