package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

func eventually(t *testing.T, within time.Duration, what string, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s did not happen within %v", what, within)
}

// twoInstances returns two servers that share one database, each caching
// credentials for a minute, with their listeners running.
func twoInstances(t *testing.T) (*db.Store, *Server, *Server) {
	t.Helper()
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthCacheTTL = time.Minute
	a, b := auditAPINewServer(t, store, cfg), auditAPINewServer(t, store, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.startAuthInvalidation(ctx)
	b.startAuthInvalidation(ctx)
	return store, a, b
}

func issueToken(t *testing.T, s *Server, store *db.Store, userID string) (token, tokenID string) {
	t.Helper()
	token, tokenID = randomSecret(32), db.NewID()
	ring := s.keyRing()
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['read','write'],$4)`, tokenID, userID, ring.Hash(token), ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	return token, tokenID
}

func lookup(s *Server, token string) error {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	_, err := s.authenticateCredentials(r)
	return err
}

func TestRevokingOnOneInstanceReachesTheOthersAtOnce(t *testing.T) {
	store, a, b := twoInstances(t)
	admin := settingsAdmin(t, store)
	token, tokenID := issueToken(t, a, store, admin.ID)

	if err := lookup(b, token); err != nil {
		t.Fatalf("first lookup on the other instance: %v", err)
	}
	// Revoked on instance A, as the API does: in the database, then forgotten.
	if revoked, err := store.RevokeToken(context.Background(), admin.ID, tokenID); err != nil || !revoked {
		t.Fatalf("revoke = %v %v", revoked, err)
	}
	a.forgetCredentials(context.Background())
	// The cache of B (a minute) must not be what keeps the token alive.
	eventually(t, 5*time.Second, "the other instance refusing the revoked token", func() bool { return lookup(b, token) != nil })
	if lookup(a, token) == nil {
		t.Fatal("the instance that revoked still accepts the token")
	}
}

func TestDisablingAUserReachesTheOtherInstances(t *testing.T) {
	store, a, b := twoInstances(t)
	admin := settingsAdmin(t, store)
	user := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'victim','reader')`, user); err != nil {
		t.Fatal(err)
	}
	token, _ := issueToken(t, a, store, user)
	if err := lookup(b, token); err != nil {
		t.Fatalf("lookup before disabling: %v", err)
	}
	if rec := auditAPIDoAs(a, admin, http.MethodPatch, "/api/v1/users/"+user, `{"status":"disabled"}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	eventually(t, 5*time.Second, "the other instance refusing a disabled user's token", func() bool { return lookup(b, token) != nil })
}

func TestTheListenerComesBackAfterTheConnectionDrops(t *testing.T) {
	store, a, b := twoInstances(t)
	admin := settingsAdmin(t, store)
	// Wait for both listeners to be subscribed.
	eventually(t, 5*time.Second, "two listening connections", func() bool {
		var n int
		store.SQL.QueryRow(`SELECT count(*) FROM pg_stat_activity WHERE query LIKE 'LISTEN hfproxy_auth%' AND pid <> pg_backend_pid()`).Scan(&n)
		return n >= 2
	})
	// The database drops every listener (a failover, a restart, a proxy timeout).
	store.SQL.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE query LIKE 'LISTEN hfproxy_auth%' AND pid <> pg_backend_pid()`)

	token, tokenID := issueToken(t, a, store, admin.ID)
	eventually(t, 10*time.Second, "the listeners reconnecting and a revocation being heard again", func() bool {
		if lookup(b, token) != nil {
			// Not yet revoked: this is the warm-up that fills B's cache.
			return false
		}
		return true
	})
	store.SQL.Exec(`UPDATE access_tokens SET revoked_at=now() WHERE id=$1`, tokenID)
	eventually(t, 10*time.Second, "a notification being heard after the reconnection", func() bool {
		a.forgetCredentials(context.Background())
		return lookup(b, token) != nil
	})
}

func TestForgettingCredentialsWorksWithoutAListener(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthCacheTTL = time.Minute
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)
	token, tokenID := issueToken(t, s, store, admin.ID)
	if err := lookup(s, token); err != nil {
		t.Fatal(err)
	}
	store.SQL.Exec(`UPDATE access_tokens SET revoked_at=now() WHERE id=$1`, tokenID)
	if lookup(s, token) != nil {
		t.Fatal("the cache is meant to hold the entry within its time")
	}
	s.forgetCredentials(context.Background()) // no listener, still empties the local cache
	if lookup(s, token) == nil {
		t.Fatal("forgetCredentials left the entry in this instance's cache")
	}
}
