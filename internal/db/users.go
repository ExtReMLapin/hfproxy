package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ErrNotFound is returned when the row asked for does not exist.
var ErrNotFound = sql.ErrNoRows

// ErrExists is returned when a unique value (a username, a group name) is taken.
var ErrExists = errors.New("already exists")

// ErrLastAdmin is returned when a change would leave no active administrator.
var ErrLastAdmin = errors.New("at least one active administrator must remain")

// StageError says which step of a multi-statement operation failed, so the
// caller can word its error response.
type StageError struct {
	Stage string
	Err   error
}

func (e *StageError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *StageError) Unwrap() error { return e.Err }

// User is a row of the users table, without credentials.
type User struct {
	ID, Username, Email, DisplayName, Role, Status string
	CreatedAt                                      time.Time
	// DailyQuotaBytes overrides the default download quota: nil uses the
	// default, 0 means unlimited, a positive value is the limit.
	DailyQuotaBytes *int64
}

// CreateUser inserts a local user. It returns ErrExists for a taken username.
func (s *Store) CreateUser(ctx context.Context, id, username, email, displayName, passwordHash, role string) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO users(id,username,email,display_name,password_hash,role) VALUES($1,$2,$3,$4,$5,$6)`, id, username, email, displayName, passwordHash, role)
	if IsUniqueViolation(err) {
		return ErrExists
	}
	return err
}

// ListUsers returns up to limit users ordered by username then id, starting
// after the given keyset when after is not nil.
func (s *Store) ListUsers(ctx context.Context, after *[2]string, limit int) ([]User, error) {
	query := `SELECT id,username,email,display_name,role,status,created_at,daily_quota_bytes FROM users `
	args := []any{}
	if after != nil {
		query += `WHERE (username,id) > ($1,$2) `
		args = append(args, after[0], after[1])
	}
	query += `ORDER BY username,id LIMIT $` + itoa(len(args)+1)
	args = append(args, limit)
	rows, err := s.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.Role, &u.Status, &u.CreatedAt, &u.DailyQuotaBytes); err != nil {
			continue
		}
		users = append(users, u)
	}
	return users, rows.Err()
}

// UpdateUser changes a user's role and/or status ("" leaves a field alone).
// It refuses to remove the last active administrator (ErrLastAdmin), returns
// ErrNotFound for an unknown user, and when it disables a user ends their
// sessions and revokes their tokens in the same transaction: if that cannot be
// done nothing changes.
func (s *Store) UpdateUser(ctx context.Context, id, role, status string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return &StageError{"begin", err}
	}
	defer tx.Rollback()
	// Serialize changes that could remove an active administrator before
	// locking the target row. Locking only that row lets concurrent requests
	// each observe the other administrator and both demote successfully.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(7242)`); err != nil {
		return &StageError{"lock", err}
	}
	var currentRole, currentStatus string
	if err := tx.QueryRowContext(ctx, `SELECT role,status FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&currentRole, &currentStatus); errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return &StageError{"read", err}
	}
	if currentRole == "admin" && currentStatus == "active" && (role == "reader" || role == "operator" || status == "disabled") {
		var activeAdmins int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&activeAdmins); err != nil {
			return &StageError{"count_admins", err}
		}
		if activeAdmins <= 1 {
			return ErrLastAdmin
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE users SET role=COALESCE(NULLIF($2,''),role),status=COALESCE(NULLIF($3,''),status),updated_at=now() WHERE id=$1`, id, role, status)
	if err != nil {
		return &StageError{"update", err}
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	if status == "disabled" {
		if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id=$1`, id); err != nil {
			return &StageError{"end_sessions", err}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE access_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id); err != nil {
			return &StageError{"revoke_tokens", err}
		}
	}
	if err := tx.Commit(); err != nil {
		return &StageError{"commit", err}
	}
	return nil
}

// ErrSetupDone is returned by CreateFirstAdmin when an administrator exists.
var ErrSetupDone = errors.New("an administrator already exists")

// HasActiveAdmin reports whether at least one active administrator exists.
func (s *Store) HasActiveAdmin(ctx context.Context) (bool, error) {
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE role='admin' AND status='active')`).Scan(&exists)
	return exists, err
}

// CreateFirstAdmin creates an administrator only if none exists. The check and
// the insert run under one lock, so several simultaneous setups, on this or on
// another replica, produce exactly one administrator and the others get
// ErrSetupDone.
func (s *Store) CreateFirstAdmin(ctx context.Context, id, username, email, displayName, passwordHash string) error {
	tx, err := s.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('hfproxy:first-admin', 0))`); err != nil {
		return err
	}
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE role='admin' AND status='active')`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return ErrSetupDone
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,email,display_name,password_hash,role) VALUES($1,$2,$3,$4,$5,'admin')`, id, username, email, displayName, passwordHash)
	if IsUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// SetUserDailyQuota sets or clears (nil) the user's own download quota. It
// returns ErrNotFound for an unknown user.
func (s *Store) SetUserDailyQuota(ctx context.Context, id string, quota *int64) error {
	result, err := s.SQL.ExecContext(ctx, `UPDATE users SET daily_quota_bytes=$2,updated_at=now() WHERE id=$1`, id, quota)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// AnyUserDailyQuota reports whether some user has a quota of their own.
func (s *Store) AnyUserDailyQuota(ctx context.Context) (bool, error) {
	var exists bool
	err := s.SQL.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE daily_quota_bytes IS NOT NULL AND daily_quota_bytes > 0)`).Scan(&exists)
	return exists, err
}

// DownloadUsage returns the user's own quota (nil when none is set) and the
// bytes downloaded over the last 24 hours.
func (s *Store) DownloadUsage(ctx context.Context, userID string) (quota *int64, usedBytes int64, err error) {
	err = s.SQL.QueryRowContext(ctx, `SELECT u.daily_quota_bytes,
		COALESCE((SELECT SUM(d.bytes) FROM downloads d WHERE d.user_id=u.id AND d.started_at > now()-interval '24 hours'),0)
		FROM users u WHERE u.id=$1`, userID).Scan(&quota, &usedBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, 0, nil
	}
	return quota, usedBytes, err
}
