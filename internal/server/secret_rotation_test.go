package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/nicolas/hfproxy/internal/config"
	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/keyring"
)

const (
	rotationOld = auditAPISessionSecret
	rotationNew = "rotated-session-secret-0123456789abcdef0123"
)

func rotationConfig(current string, previous ...string) config.Config {
	cfg := auditAPIConfig()
	cfg.SessionSecret = current
	cfg.PreviousSessionSecrets = previous
	return cfg
}

func rotationUser(t *testing.T, store *db.Store, name string) string {
	t.Helper()
	id := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'reader')`, id, name); err != nil {
		t.Fatal(err)
	}
	return id
}

func bearer(token string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/whoami-v2", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

func TestLegacyTokensKeepWorkingAndMoveToDerivedKeys(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, rotationConfig(rotationOld))
	userID := rotationUser(t, store, "legacy")
	const token = "hfpx_legacy-token-0123456789abcdef"
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix) VALUES('legacy',$1,$2,'hfpx_legacy')`, userID, HashToken(token, rotationOld)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.authenticate(bearer(token)); err != nil {
		t.Fatalf("a token stored under the original scheme was refused: %v", err)
	}
	var stored []byte
	var keyID string
	if err := store.SQL.QueryRow(`SELECT token_hash, key_id FROM access_tokens WHERE id='legacy'`).Scan(&stored, &keyID); err != nil {
		t.Fatal(err)
	}
	ring := keyring.NewRing(rotationOld)
	if !ring.IsCurrent(token, stored) || keyID != ring.KeyID() {
		t.Fatalf("the token was not moved to the derived key: current=%v key_id=%q want %q", ring.IsCurrent(token, stored), keyID, ring.KeyID())
	}
	if _, err := s.authenticate(bearer(token)); err != nil {
		t.Fatalf("the token stopped working after being moved: %v", err)
	}
}

func TestTokensSurviveRotationOnlyWhileThePreviousSecretIsConfigured(t *testing.T) {
	store := auditAPIFreshStore(t)
	before := auditAPINewServer(t, store, rotationConfig(rotationOld))
	userID := rotationUser(t, store, "rotating")
	const token = "hfpx_rotating-token-0123456789abcd"
	oldRing := keyring.NewRing(rotationOld)
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,key_id) VALUES('t1',$1,$2,'hfpx_rotating',$3)`, userID, oldRing.Hash(token), oldRing.KeyID()); err != nil {
		t.Fatal(err)
	}
	if _, err := before.authenticate(bearer(token)); err != nil {
		t.Fatalf("token before rotation: %v", err)
	}

	withoutPrevious := auditAPINewServer(t, store, rotationConfig(rotationNew))
	if _, err := withoutPrevious.authenticate(bearer(token)); err == nil {
		t.Fatal("a token hashed under the old secret was accepted without HFPROXY_SESSION_SECRET_PREVIOUS")
	}

	rotated := auditAPINewServer(t, store, rotationConfig(rotationNew, rotationOld))
	if _, err := rotated.authenticate(bearer(token)); err != nil {
		t.Fatalf("token during rotation: %v", err)
	}
	// The first use moved it to the new key, so the previous secret is no longer needed.
	if _, err := withoutPrevious.authenticate(bearer(token)); err != nil {
		t.Fatalf("after one use the token must verify under the new secret alone: %v", err)
	}
	var keyID string
	if err := store.SQL.QueryRow(`SELECT key_id FROM access_tokens WHERE id='t1'`).Scan(&keyID); err != nil || keyID != keyring.NewRing(rotationNew).KeyID() {
		t.Fatalf("key_id = %q (%v), want the new key", keyID, err)
	}
}

func TestSessionsAndCSRFSurviveRotation(t *testing.T) {
	store := auditAPIFreshStore(t)
	old := auditAPINewServer(t, store, rotationConfig(rotationOld))
	userID := rotationUser(t, store, "session-user")
	rec := httptest.NewRecorder()
	csrf, err := old.createSession(context.Background(), userID, rec, httptest.NewRequest(http.MethodPost, "/login", nil))
	if err != nil {
		t.Fatal(err)
	}
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hfproxy_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil {
		t.Fatal("no session cookie was set")
	}
	var keyID string
	if err := store.SQL.QueryRow(`SELECT key_id FROM sessions WHERE user_id=$1`, userID).Scan(&keyID); err != nil || keyID != keyring.NewRing(rotationOld).KeyID() {
		t.Fatalf("session key_id = %q (%v)", keyID, err)
	}
	request := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
		req.AddCookie(sessionCookie)
		req.AddCookie(&http.Cookie{Name: "hfproxy_csrf", Value: csrf})
		req.Header.Set("X-CSRF-Token", csrf)
		return req
	}

	rotated := auditAPINewServer(t, store, rotationConfig(rotationNew, rotationOld))
	if _, err := rotated.authenticate(request()); err != nil {
		t.Fatalf("session during rotation: %v", err)
	}
	if !rotated.checkCSRF(request()) {
		t.Fatal("CSRF validation failed for a session created under the previous secret")
	}
	forgedToken := request()
	forgedToken.Header.Set("X-CSRF-Token", "not-the-token")
	if rotated.checkCSRF(forgedToken) {
		t.Fatal("CSRF validation accepted a wrong token")
	}

	withoutPrevious := auditAPINewServer(t, store, rotationConfig(rotationNew))
	if _, err := withoutPrevious.authenticate(request()); err == nil {
		t.Fatal("a session from the old secret was accepted once the previous secret was removed")
	}
}

func TestNewTokensAndSessionsUseTheCurrentKey(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, rotationConfig(rotationNew, rotationOld))
	userID := rotationUser(t, store, "fresh")
	if _, err := s.createSession(context.Background(), userID, httptest.NewRecorder(), nil); err != nil {
		t.Fatal(err)
	}
	var session []byte
	if err := store.SQL.QueryRow(`SELECT token_hash FROM sessions WHERE user_id=$1`, userID).Scan(&session); err != nil {
		t.Fatal(err)
	}
	if len(session) != 32 {
		t.Fatalf("session hash length = %d", len(session))
	}
	var keyID string
	if err := store.SQL.QueryRow(`SELECT key_id FROM sessions WHERE user_id=$1`, userID).Scan(&keyID); err != nil || keyID != keyring.NewRing(rotationNew).KeyID() {
		t.Fatalf("new session key_id = %q (%v), want the current key", keyID, err)
	}
	if strings.Contains(keyID, rotationNew) {
		t.Fatal("key id leaks the secret")
	}
}
