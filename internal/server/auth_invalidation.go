package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/hfproxy/hfproxy/internal/db"
)

// Credential lookups are cached for a few seconds (HFPROXY_AUTH_CACHE_TTL) to
// spare the database on busy paths. Revoking a token, ending a session or
// disabling a user must not wait for that time on the other instances, so the
// instance that made the change sends a PostgreSQL notification and every
// instance, listening on a connection of its own, empties its cache at once.
// The cache time stays the upper bound if a notification is ever lost.

// forgetCredentials empties this instance's credential cache and tells the
// others to do the same. A failure to notify is logged: the other instances
// then catch up within the cache time.
func (s *Server) forgetCredentials(ctx context.Context) {
	s.authLookups().purge()
	if s.db == nil || s.db.DSN == "" {
		return
	}
	notifyCtx, cancel := bookkeepingContext(ctx)
	defer cancel()
	if err := s.db.NotifyAuthChange(notifyCtx); err != nil {
		s.logger.Warn("other instances could not be told that credentials changed; they catch up within the cache time",
			"request_id", requestID(ctx), "error", safeLogValue(err.Error(), 200))
	}
}

// startAuthInvalidation listens for those notifications until ctx ends,
// reconnecting with a growing pause when the connection drops.
func (s *Server) startAuthInvalidation(ctx context.Context) {
	if s.db == nil || s.db.DSN == "" {
		return
	}
	go func() {
		pause := time.Second
		for ctx.Err() == nil {
			started := time.Now()
			err := s.listenForAuthChanges(ctx)
			if ctx.Err() != nil {
				return
			}
			// Whatever was missed while disconnected is covered by emptying the cache.
			s.authLookups().purge()
			s.logger.Warn("the listener for credential changes stopped; reconnecting", "error", safeLogValue(errText(err), 200), "retry_in", pause.String())
			select {
			case <-time.After(pause):
			case <-ctx.Done():
				return
			}
			if time.Since(started) > time.Minute {
				pause = time.Second
			} else if pause < 30*time.Second {
				pause *= 2
			}
		}
	}()
}

func (s *Server) listenForAuthChanges(ctx context.Context) error {
	connectCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	conn, err := pgx.Connect(connectCtx, s.db.DSN)
	cancel()
	if err != nil {
		return err
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = conn.Close(closeCtx)
	}()
	if _, err := conn.Exec(ctx, "LISTEN "+db.AuthChannel); err != nil {
		return err
	}
	// Anything that changed before the subscription is not announced.
	s.authLookups().purge()
	s.logger.Debug("listening for credential changes from other instances")
	for {
		if _, err := conn.WaitForNotification(ctx); err != nil {
			return err
		}
		s.authLookups().purge()
		s.logger.Debug("credential cache emptied after a notification")
	}
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
