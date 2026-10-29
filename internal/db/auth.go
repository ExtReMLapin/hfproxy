package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Account is the part of a user row the authentication layer needs.
type Account struct {
	ID, Username, Email, DisplayName, Role, Status string
}

// TokenLookup is the owner of an access token and what the token may do.
type TokenLookup struct {
	Account
	TokenID    string
	Scopes     []string
	StoredHash []byte // the hash the token is stored under
}

// SessionLookup is the owner of a browser session.
type SessionLookup struct {
	Account
	Hash []byte
}

// LookupToken finds a live access token by any of its candidate hashes, or
// returns ErrNotFound for an unknown, revoked or expired one.
func (s *Store) LookupToken(ctx context.Context, candidates [][]byte) (TokenLookup, error) {
	var t TokenLookup
	err := s.SQL.QueryRowContext(ctx, `SELECT u.id,u.username,u.email,u.display_name,u.role,u.status,t.id,t.scopes,t.token_hash
		FROM access_tokens t JOIN users u ON u.id=t.user_id
		WHERE t.token_hash = ANY($1) AND t.revoked_at IS NULL AND (t.expires_at IS NULL OR t.expires_at>now())`, candidates).
		Scan(&t.ID, &t.Username, &t.Email, &t.DisplayName, &t.Role, &t.Status, &t.TokenID, &t.Scopes, &t.StoredHash)
	if errors.Is(err, sql.ErrNoRows) {
		return TokenLookup{}, ErrNotFound
	}
	return t, err
}

// RehashToken moves a token stored under an older hash scheme to the current one.
func (s *Store) RehashToken(ctx context.Context, tokenID string, oldHash, newHash []byte, keyID string) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE access_tokens SET token_hash=$3, key_id=$4 WHERE id=$1 AND token_hash=$2`, tokenID, oldHash, newHash, keyID)
	return err
}

// TouchToken records a token use, at most once a minute.
func (s *Store) TouchToken(ctx context.Context, tokenID string) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE access_tokens SET last_used_at=now() WHERE id=$1 AND (last_used_at IS NULL OR last_used_at < now()-interval '1 minute')`, tokenID)
	return err
}

// LookupSession finds an unexpired session by any of its candidate hashes, or
// returns ErrNotFound.
func (s *Store) LookupSession(ctx context.Context, candidates [][]byte) (SessionLookup, error) {
	var l SessionLookup
	err := s.SQL.QueryRowContext(ctx, `SELECT u.id,u.username,u.email,u.display_name,u.role,u.status,ss.token_hash
		FROM sessions ss JOIN users u ON u.id=ss.user_id
		WHERE ss.token_hash = ANY($1) AND ss.expires_at>now()`, candidates).
		Scan(&l.ID, &l.Username, &l.Email, &l.DisplayName, &l.Role, &l.Status, &l.Hash)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionLookup{}, ErrNotFound
	}
	return l, err
}

// TouchSession slides a session's idle expiry, capped by its absolute lifetime,
// at most once a minute.
func (s *Store) TouchSession(ctx context.Context, hash []byte, idle, absolute time.Duration) error {
	_, err := s.SQL.ExecContext(ctx, `UPDATE sessions SET last_seen_at=now(), expires_at=LEAST(created_at + $2::interval, now() + $1::interval) WHERE token_hash=$3 AND last_seen_at < now()-interval '1 minute'`,
		idle.String(), absolute.String(), hash)
	return err
}

// CreateSession stores a new session.
func (s *Store) CreateSession(ctx context.Context, hash []byte, userID string, csrfHash []byte, expires time.Time, keyID string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,key_id) VALUES ($1,$2,$3,$4,$5)`, hash, userID, csrfHash, expires, keyID)
	return err
}

// DeleteSessions ends the sessions stored under any of the hashes.
func (s *Store) DeleteSessions(ctx context.Context, candidates [][]byte) error {
	_, err := s.SQL.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ANY($1)`, candidates)
	return err
}

// SessionOwner returns the user a session belongs to, or "" when unknown.
func (s *Store) SessionOwner(ctx context.Context, candidates [][]byte) string {
	var id string
	_ = s.SQL.QueryRowContext(ctx, `SELECT user_id FROM sessions WHERE token_hash = ANY($1)`, candidates).Scan(&id)
	return id
}

// SessionCSRFHash returns the stored CSRF hash of an unexpired session.
func (s *Store) SessionCSRFHash(ctx context.Context, candidates [][]byte) ([]byte, error) {
	var stored []byte
	err := s.SQL.QueryRowContext(ctx, `SELECT csrf_hash FROM sessions WHERE token_hash = ANY($1) AND expires_at>now()`, candidates).Scan(&stored)
	return stored, err
}

// PasswordLogin returns what a local sign-in checks: the user's id, password
// hash and status. ErrNotFound means no such user.
func (s *Store) PasswordLogin(ctx context.Context, username string) (id, passwordHash, status string, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT id,password_hash,status FROM users WHERE username=$1`, username).Scan(&id, &passwordHash, &status)
	if errors.Is(err, sql.ErrNoRows) {
		err = ErrNotFound
	}
	return id, passwordHash, status, err
}

// InsertAudit appends an event to the audit log. actorID may be "".
func (s *Store) InsertAudit(ctx context.Context, actorID, action, targetType, targetID, metadataJSON, requestID string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO audit_events(id, actor_user_id, action, target_type, target_id, metadata_json, request_id)
		VALUES ($1, NULLIF($2,''), $3, $4, $5, $6::jsonb, $7)`, NewID(), actorID, action, targetType, targetID, metadataJSON, requestID)
	return err
}
