package db

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// AttemptCount is the number of attempts counted for a key in its current
// window, and the whole seconds left in that window.
type AttemptCount struct {
	Key       string
	Attempts  int
	Remaining int
}

// CountLoginAttempts adds one attempt to each key, restarting a key's window
// when it has expired, and returns the counts after the increment. Counters live
// in PostgreSQL so that every replica enforces the same limits.
func (s *Store) CountLoginAttempts(ctx context.Context, keys []string, window time.Duration) ([]AttemptCount, error) {
	rows, err := s.SQL.QueryContext(ctx, `INSERT INTO login_attempts(key,window_start,attempts)
		SELECT k, now(), 1 FROM unnest($1::text[]) AS k
		ON CONFLICT(key) DO UPDATE SET
			window_start = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN now() ELSE login_attempts.window_start END,
			attempts = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN 1 ELSE login_attempts.attempts+1 END
		RETURNING key, attempts, GREATEST(0, CEIL($2::double precision - EXTRACT(EPOCH FROM (now()-window_start))))::int`,
		keys, window.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var counts []AttemptCount
	for rows.Next() {
		var c AttemptCount
		if err := rows.Scan(&c.Key, &c.Attempts, &c.Remaining); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// LoginAttempts reads a key's count in its current window without changing it.
// found is false when the key has no live window.
func (s *Store) LoginAttempts(ctx context.Context, key string, window time.Duration) (c AttemptCount, found bool, err error) {
	c.Key = key
	err = s.SQL.QueryRowContext(ctx, `SELECT attempts, GREATEST(0, CEIL($2::double precision - EXTRACT(EPOCH FROM (now()-window_start))))::int
		FROM login_attempts WHERE key=$1 AND window_start > now()-make_interval(secs => $2::double precision)`, key, window.Seconds()).Scan(&c.Attempts, &c.Remaining)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

// AddLoginAttempt counts one attempt against a key, like CountLoginAttempts for a single key.
func (s *Store) AddLoginAttempt(ctx context.Context, key string, window time.Duration) error {
	_, err := s.SQL.ExecContext(ctx, `INSERT INTO login_attempts(key,window_start,attempts) VALUES($1, now(), 1)
		ON CONFLICT(key) DO UPDATE SET
			window_start = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN now() ELSE login_attempts.window_start END,
			attempts = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN 1 ELSE login_attempts.attempts+1 END`, key, window.Seconds())
	return err
}
