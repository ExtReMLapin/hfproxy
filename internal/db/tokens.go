package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrTokenLimit is returned when a user already holds the maximum number of live tokens.
var ErrTokenLimit = errors.New("token limit reached")

// ErrInactiveUser is returned when the user is unknown or not active.
var ErrInactiveUser = errors.New("user is not active")

// AccessToken is a token's catalog entry; the secret is never stored.
type AccessToken struct {
	ID, Name, Prefix string
	Scopes           []string
	CreatedAt        time.Time
	ExpiresAt        *time.Time
	LastUsedAt       *time.Time
}

// RevokeToken revokes one of a user's live tokens and reports whether it existed.
func (s *Store) RevokeToken(ctx context.Context, userID, tokenID string) (bool, error) {
	result, err := s.SQL.ExecContext(ctx, `UPDATE access_tokens SET revoked_at=now() WHERE id=$1 AND user_id=$2 AND revoked_at IS NULL`, tokenID, userID)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	return err == nil && n > 0, err
}

// ListTokens returns a user's live tokens, newest first.
func (s *Store) ListTokens(ctx context.Context, userID string) ([]AccessToken, error) {
	rows, err := s.SQL.QueryContext(ctx, `SELECT id,name,prefix,scopes,created_at,expires_at,last_used_at FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL ORDER BY created_at DESC,id DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tokens []AccessToken
	for rows.Next() {
		var t AccessToken
		if err := rows.Scan(&t.ID, &t.Name, &t.Prefix, &t.Scopes, &t.CreatedAt, &t.ExpiresAt, &t.LastUsedAt); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// CreateToken stores a new token for an active user, refusing past maxLive live
// tokens (ErrTokenLimit) or an inactive user (ErrInactiveUser). The user row is
// locked so concurrent requests cannot bypass the limit. It fills t.CreatedAt.
func (s *Store) CreateToken(ctx context.Context, userID string, t *AccessToken, tokenHash []byte, keyID string, maxLive int) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id string
	if err := tx.QueryRowContext(ctx, `SELECT id FROM users WHERE id=$1 AND status='active' FOR UPDATE`, userID).Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInactiveUser
		}
		return err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM access_tokens WHERE user_id=$1 AND revoked_at IS NULL`, userID).Scan(&count); err != nil {
		return err
	}
	if count >= maxLive {
		return ErrTokenLimit
	}
	if err := tx.QueryRowContext(ctx, `INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,expires_at,key_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING created_at`,
		t.ID, userID, tokenHash, t.Prefix, t.Name, t.Scopes, t.ExpiresAt, keyID).Scan(&t.CreatedAt); err != nil {
		return err
	}
	return tx.Commit()
}
