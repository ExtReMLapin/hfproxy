package db

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// ExternalAccount is a local user linked to an identity at an external provider.
type ExternalAccount struct {
	UserID, Status string
	identityID     string
}

// FindExternalIdentity returns the local user linked to (issuer, subject), or ErrNotFound.
func (s *Store) FindExternalIdentity(ctx context.Context, issuer, subject string) (ExternalAccount, error) {
	var a ExternalAccount
	err := s.SQL.QueryRowContext(ctx, `SELECT x.id,u.id,u.status FROM external_identities x JOIN users u ON u.id=x.user_id WHERE x.issuer=$1 AND x.subject=$2`, issuer, subject).Scan(&a.identityID, &a.UserID, &a.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return ExternalAccount{}, ErrNotFound
	}
	return a, err
}

// ChangeExternalSubject moves a link to another subject (for example from a DN
// to a stable directory identifier).
func (s *Store) ChangeExternalSubject(ctx context.Context, a ExternalAccount, subject string) error {
	_, err := s.SQL.ExecContext(ctx, "UPDATE external_identities SET subject=$2 WHERE id=$1", a.identityID, subject)
	return err
}

// ProvisionExternalUser creates a reader account and links it to an external
// identity in one transaction.
func (s *Store) ProvisionExternalUser(ctx context.Context, userID, username, email, displayName, provider, issuer, subject string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO users(id,username,email,display_name,role) VALUES($1,$2,$3,$4,'reader')`, userID, username, email, displayName); err != nil {
		return &StageError{"user", err}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO external_identities(id,user_id,provider,issuer,subject) VALUES($1,$2,$3,$4,$5)`, NewID(), userID, provider, issuer, subject); err != nil {
		return &StageError{"identity", err}
	}
	return tx.Commit()
}

// SyncGroups makes a user's membership in groups of the given source match
// names. Groups of other sources are never touched, and a group of another
// source with the same name keeps its own membership.
func (s *Store) SyncGroups(ctx context.Context, userID, source string, names []string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	keep := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || len(name) > 200 {
			continue
		}
		var groupID, groupSource string
		if err := tx.QueryRowContext(ctx, `INSERT INTO groups(id,name,source) VALUES($1,$2,$3) ON CONFLICT(name) DO UPDATE SET name=EXCLUDED.name RETURNING id,source`, NewID(), name, source).Scan(&groupID, &groupSource); err != nil {
			return err
		}
		if groupSource != source {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO group_members(group_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, groupID, userID); err != nil {
			return err
		}
		keep = append(keep, groupID)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM group_members m USING groups g WHERE g.id=m.group_id AND g.source=$3 AND m.user_id=$1 AND NOT (m.group_id = ANY($2))`, userID, keep, source); err != nil {
		return err
	}
	return tx.Commit()
}
