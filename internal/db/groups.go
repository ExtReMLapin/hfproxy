package db

import (
	"context"
	"database/sql"
	"errors"
)

// ErrInvalidMember is returned when a group member ID is not a user.
var ErrInvalidMember = errors.New("invalid group member")

// Group is a row of the groups table with its member count.
type Group struct {
	ID, Name, Source string
	Members          int
}

// GroupMember is a user listed in a group.
type GroupMember struct{ ID, Username, Role string }

// ListGroups returns up to limit groups ordered by name then id, starting
// after the given keyset when after is not nil.
func (s *Store) ListGroups(ctx context.Context, after *[2]string, limit int) ([]Group, error) {
	query := `SELECT g.id,g.name,g.source,COUNT(m.user_id) FROM groups g LEFT JOIN group_members m ON m.group_id=g.id `
	args := []any{}
	if after != nil {
		query += `WHERE (g.name,g.id) > ($1,$2) `
		args = append(args, after[0], after[1])
	}
	query += `GROUP BY g.id ORDER BY g.name,g.id LIMIT $` + itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		var g Group
		if rows.Scan(&g.ID, &g.Name, &g.Source, &g.Members) == nil {
			groups = append(groups, g)
		}
	}
	return groups, rows.Err()
}

// CreateGroup creates a local group with its members in one transaction. It
// returns ErrExists for a taken name and ErrInvalidMember for an unknown user.
func (s *Store) CreateGroup(ctx context.Context, id, name string, members []string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return &StageError{"begin", err}
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO groups(id,name) VALUES($1,$2)`, id, name); err != nil {
		if IsUniqueViolation(err) {
			return ErrExists
		}
		return &StageError{"create", err}
	}
	seen := make(map[string]struct{}, len(members))
	for _, member := range members {
		if _, dup := seen[member]; dup {
			continue
		}
		seen[member] = struct{}{}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, id, member); err != nil {
			if IsForeignKeyViolation(err) {
				return ErrInvalidMember
			}
			return &StageError{"add_member", err}
		}
	}
	if err := tx.Commit(); err != nil {
		return &StageError{"commit", err}
	}
	return nil
}

// GetGroup returns a group and its members, or ErrNotFound.
func (s *Store) GetGroup(ctx context.Context, id string) (Group, []GroupMember, error) {
	g := Group{ID: id}
	if err := s.SQL.QueryRowContext(ctx, `SELECT name,source FROM groups WHERE id=$1`, id).Scan(&g.Name, &g.Source); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Group{}, nil, ErrNotFound
		}
		return Group{}, nil, &StageError{"load", err}
	}
	rows, err := s.SQL.QueryContext(ctx, `SELECT u.id,u.username,u.role FROM group_members m JOIN users u ON u.id=m.user_id WHERE m.group_id=$1 ORDER BY u.username`, id)
	if err != nil {
		return Group{}, nil, &StageError{"members", err}
	}
	defer rows.Close()
	var members []GroupMember
	for rows.Next() {
		var m GroupMember
		if rows.Scan(&m.ID, &m.Username, &m.Role) == nil {
			members = append(members, m)
		}
	}
	g.Members = len(members)
	return g, members, nil
}

// UpdateGroup renames a group (name != nil) and/or replaces its members
// (members != nil) in one transaction. It returns the resulting name and member
// count, ErrNotFound, ErrExists or ErrInvalidMember.
func (s *Store) UpdateGroup(ctx context.Context, id string, name *string, members *[]string) (string, int, error) {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, &StageError{"begin", err}
	}
	defer tx.Rollback()
	var currentName string
	if err := tx.QueryRowContext(ctx, `SELECT name FROM groups WHERE id=$1 FOR UPDATE`, id).Scan(&currentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, ErrNotFound
		}
		return "", 0, &StageError{"load", err}
	}
	if name != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE groups SET name=$2 WHERE id=$1`, id, *name); err != nil {
			if IsUniqueViolation(err) {
				return "", 0, ErrExists
			}
			return "", 0, &StageError{"rename", err}
		}
		currentName = *name
	}
	if members != nil {
		if _, err := tx.ExecContext(ctx, `DELETE FROM group_members WHERE group_id=$1`, id); err != nil {
			return "", 0, &StageError{"replace_members", err}
		}
		for _, memberID := range *members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, memberID); err != nil {
				if IsForeignKeyViolation(err) {
					return "", 0, ErrInvalidMember
				}
				return "", 0, &StageError{"add_member", err}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return "", 0, &StageError{"commit", err}
	}
	var count int
	_ = s.SQL.QueryRowContext(ctx, `SELECT COUNT(*) FROM group_members WHERE group_id=$1`, id).Scan(&count)
	return currentName, count, nil
}
