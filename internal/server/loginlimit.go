package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// Login throttling is shared through PostgreSQL so every instance sees the same
// counters. Each limit is a fixed one-minute window.
//
//   - per address: attempts of any kind from one client address;
//   - per address and username: attempts by one client against one account;
//   - per username: only attempts that failed, from all addresses together.
//
// The username limit counts failures only and is much higher than the others.
// A successful login never uses it up, and locking an account out on purpose
// takes failed attempts from several addresses at once, each of which is
// itself limited.
const (
	loginIPLimit          = 20
	loginPairLimit        = 10
	loginUserFailureLimit = 50
	loginWindow           = time.Minute
)

func loginKey(kind string, parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return kind + ":" + hex.EncodeToString(sum[:16])
}

func normalizeLoginName(username string) string {
	username = strings.ToLower(strings.TrimSpace(username))
	if len(username) > 256 {
		username = username[:256]
	}
	return username
}

// allowLoginAttemptShared counts one attempt and reports whether it may go on,
// and otherwise how many seconds remain in the offending window.
func (s *Server) allowLoginAttemptShared(ctx context.Context, clientIP, username string) (int, bool, error) {
	name := normalizeLoginName(username)
	keys := []string{loginKey("ip", clientIP), loginKey("pair", clientIP, name)}
	limits := map[string]int{keys[0]: loginIPLimit, keys[1]: loginPairLimit}

	rows, err := s.db.SQL.QueryContext(ctx, `INSERT INTO login_attempts(key,window_start,attempts)
		SELECT k, now(), 1 FROM unnest($1::text[]) AS k
		ON CONFLICT(key) DO UPDATE SET
			window_start = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN now() ELSE login_attempts.window_start END,
			attempts = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN 1 ELSE login_attempts.attempts+1 END
		RETURNING key, attempts, GREATEST(0, CEIL($2::double precision - EXTRACT(EPOCH FROM (now()-window_start))))::int`,
		keys, loginWindow.Seconds())
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	retryAfter, allowed := 0, true
	for rows.Next() {
		var key string
		var attempts, remaining int
		if err := rows.Scan(&key, &attempts, &remaining); err != nil {
			return 0, false, err
		}
		if attempts > limits[key] {
			allowed = false
			if remaining > retryAfter {
				retryAfter = remaining
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	if !allowed {
		return max(retryAfter, 1), false, nil
	}

	var failures, remaining int
	err = s.db.SQL.QueryRowContext(ctx, `SELECT attempts, GREATEST(0, CEIL($2::double precision - EXTRACT(EPOCH FROM (now()-window_start))))::int
		FROM login_attempts WHERE key=$1 AND window_start > now()-make_interval(secs => $2::double precision)`,
		loginKey("fail", name), loginWindow.Seconds()).Scan(&failures, &remaining)
	if err != nil && !isNoRows(err) {
		return 0, false, err
	}
	if err == nil && failures >= loginUserFailureLimit {
		return max(remaining, 1), false, nil
	}
	return 0, true, nil
}

// recordLoginFailure counts a failed attempt against the account.
func (s *Server) recordLoginFailure(ctx context.Context, username string) {
	if s.db == nil || s.db.SQL == nil {
		return
	}
	_, err := s.db.SQL.ExecContext(ctx, `INSERT INTO login_attempts(key,window_start,attempts) VALUES($1, now(), 1)
		ON CONFLICT(key) DO UPDATE SET
			window_start = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN now() ELSE login_attempts.window_start END,
			attempts = CASE WHEN login_attempts.window_start <= now()-make_interval(secs => $2::double precision) THEN 1 ELSE login_attempts.attempts+1 END`,
		loginKey("fail", normalizeLoginName(username)), loginWindow.Seconds())
	if err != nil && s.logger != nil {
		s.logger.Warn("could not record a failed login", "class", fmt.Sprintf("%T", err))
	}
}
