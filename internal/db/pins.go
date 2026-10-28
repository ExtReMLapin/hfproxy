package db

import (
	"context"
	"time"
)

// Pin keeps cached files out of eviction. One of Commit and Ref is set for a
// commit or branch/tag pin; both empty pins the whole repository.
type Pin struct {
	Commit, Ref string
	CreatedAt   time.Time
}

// ListPins returns a repository's pins.
func (s *Store) ListPins(ctx context.Context, repositoryID string) ([]Pin, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT commit_sha,ref,created_at FROM cache_pins WHERE repository_id=$1 ORDER BY ref,commit_sha`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pins []Pin
	for rows.Next() {
		var p Pin
		if err := rows.Scan(&p.Commit, &p.Ref, &p.CreatedAt); err != nil {
			return nil, err
		}
		pins = append(pins, p)
	}
	return pins, rows.Err()
}

// AddPin records a pin and reports whether it was new.
func (s *Store) AddPin(ctx context.Context, repositoryID, commit, ref, createdBy string) (bool, error) {
	result, err := s.SQL.ExecContext(ctx, `INSERT INTO cache_pins(repository_id,commit_sha,ref,created_by) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repositoryID, commit, ref, createdBy)
	if err != nil {
		return false, err
	}
	added, _ := result.RowsAffected()
	return added > 0, nil
}

// RemovePin deletes a pin and reports whether it existed.
func (s *Store) RemovePin(ctx context.Context, repositoryID, commit, ref string) (bool, error) {
	result, err := s.SQL.ExecContext(ctx, `DELETE FROM cache_pins WHERE repository_id=$1 AND commit_sha=$2 AND ref=$3`, repositoryID, commit, ref)
	if err != nil {
		return false, err
	}
	n, _ := result.RowsAffected()
	return n > 0, nil
}
