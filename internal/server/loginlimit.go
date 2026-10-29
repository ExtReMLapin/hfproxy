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

	counts, err := s.db.CountLoginAttempts(ctx, keys, loginWindow)
	if err != nil {
		return 0, false, err
	}
	retryAfter, allowed := 0, true
	for _, c := range counts {
		if c.Attempts > limits[c.Key] {
			allowed = false
			if c.Remaining > retryAfter {
				retryAfter = c.Remaining
			}
		}
	}
	if !allowed {
		return max(retryAfter, 1), false, nil
	}

	failures, found, err := s.db.LoginAttempts(ctx, loginKey("fail", name), loginWindow)
	if err != nil {
		return 0, false, err
	}
	if found && failures.Attempts >= loginUserFailureLimit {
		return max(failures.Remaining, 1), false, nil
	}
	return 0, true, nil
}

// recordLoginFailure counts a failed attempt against the account.
func (s *Server) recordLoginFailure(ctx context.Context, username string) {
	if s.db == nil || s.db.SQL == nil {
		return
	}
	err := s.db.AddLoginAttempt(ctx, loginKey("fail", normalizeLoginName(username)), loginWindow)
	if err != nil && s.logger != nil {
		s.logger.Warn("could not record a failed login", "class", fmt.Sprintf("%T", err))
	}
}
