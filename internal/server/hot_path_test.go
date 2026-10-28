package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

func TestAuthCacheServesRepeatedLookupsAndForgetsOnPurge(t *testing.T) {
	cache := newAuthCache(time.Minute)
	key := authCacheKey("token", "secret-token")
	if _, ok := cache.get(key); ok {
		t.Fatal("an empty cache returned a principal")
	}
	cache.put(key, &Principal{ID: "u1", Role: "reader", Scopes: []string{"read"}})
	got, ok := cache.get(key)
	if !ok || got.ID != "u1" {
		t.Fatalf("get after put = %v %v", got, ok)
	}
	got.Scopes[0] = "admin" // callers must not be able to alter the cached copy
	if again, _ := cache.get(key); again.Scopes[0] != "read" {
		t.Fatal("the cached principal was mutated through a returned copy")
	}
	cache.purge()
	if _, ok := cache.get(key); ok {
		t.Fatal("purge left an entry behind")
	}
	if _, ok := newAuthCache(0).get(key); ok {
		t.Fatal("a zero TTL must disable the cache")
	}
	short := newAuthCache(10 * time.Millisecond)
	short.put(key, &Principal{ID: "u1"})
	time.Sleep(30 * time.Millisecond)
	if _, ok := short.get(key); ok {
		t.Fatal("an expired entry was returned")
	}
}

func TestRevokingATokenTakesEffectAtOnceDespiteTheCache(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthCacheTTL = time.Minute
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)

	token := randomSecret(32)
	tokenID := db.NewID()
	ring := s.keyRing()
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['read'],$4)`, tokenID, admin.ID, ring.Hash(token), ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	request := func() (*Principal, error) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		return s.authenticateCredentials(r)
	}
	if _, err := request(); err != nil {
		t.Fatalf("first lookup: %v", err)
	}
	// Revoked behind the cache's back (another replica or the CLI): still served
	// from the cache, which is the documented bound.
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET revoked_at=now() WHERE id=$1`, tokenID); err != nil {
		t.Fatal(err)
	}
	if _, err := request(); err != nil {
		t.Fatalf("a cached lookup must survive within the TTL, got %v", err)
	}
	// Revoked through this instance's API: forgotten immediately.
	s.authLookups().purge()
	if _, err := request(); err == nil {
		t.Fatal("a revoked token still authenticates after the cache was purged")
	}
}

func TestHitEventsAreBatchedAndFlushedOnShutdown(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.DownloadLogQueue = 64
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/hits")

	ctx, stop := context.WithCancel(context.Background())
	s.startHitLog(ctx)
	for i := 0; i < 250; i++ {
		s.recordDownload(ctx, repo, pinCommitA, "model.bin", "hit", 10, "")
	}
	s.recordDownload(ctx, repo, pinCommitA, "model.bin", "failed", 0, "storage") // not a hit: written in the request
	stop()
	s.waitHitLog()

	var hits, failed int
	if err := store.SQL.QueryRow(`SELECT count(*) FILTER (WHERE status='hit'), count(*) FILTER (WHERE status='failed') FROM downloads WHERE repository_id=$1`, repo).Scan(&hits, &failed); err != nil {
		t.Fatal(err)
	}
	if hits != 250 || failed != 1 {
		t.Fatalf("recorded %d hits and %d failures, want 250 and 1: events must not be lost, even with a full queue", hits, failed)
	}
}

// Disabling a user must cut off a token that is sitting in the cache.
func TestDisablingAUserEndsCachedTokensAtOnce(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthCacheTTL = time.Minute
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)

	reader := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'reader')`, reader, "reader-"+reader[:8]); err != nil {
		t.Fatal(err)
	}
	token := randomSecret(32)
	ring := s.keyRing()
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['read'],$4)`, db.NewID(), reader, ring.Hash(token), ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	authenticated := func() bool {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		_, err := s.authenticateCredentials(r)
		return err == nil
	}
	if !authenticated() {
		t.Fatal("the token should authenticate before the user is disabled")
	}
	if !authenticated() { // served from the cache
		t.Fatal("the second lookup, from the cache, failed")
	}
	rec := auditAPIDoAs(s, admin, http.MethodPatch, "/api/v1/users/"+reader, `{"status":"disabled"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("disable user = %d %s", rec.Code, rec.Body.String())
	}
	if authenticated() {
		t.Fatal("a cached token still works after its user was disabled")
	}
}

// BenchmarkAuthenticateToken measures a token lookup with the cache off and on
// (go test -run xxx -bench AuthenticateToken ./internal/server/).
func BenchmarkAuthenticateToken(b *testing.B) {
	for _, tc := range []struct {
		name string
		ttl  time.Duration
	}{{"no cache", 0}, {"5s cache", 5 * time.Second}} {
		b.Run(tc.name, func(b *testing.B) {
			store := auditAPIFreshStore(b)
			cfg := auditAPIConfig()
			cfg.AuthCacheTTL = tc.ttl
			s := auditAPINewServer(b, store, cfg)
			user := db.NewID()
			if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'reader')`, user, "bench-"+user[:8]); err != nil {
				b.Fatal(err)
			}
			token := randomSecret(32)
			ring := s.keyRing()
			if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['read'],$4)`, db.NewID(), user, ring.Hash(token), ring.KeyID()); err != nil {
				b.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
			r.Header.Set("Authorization", "Bearer "+token)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.authenticateCredentials(r); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
