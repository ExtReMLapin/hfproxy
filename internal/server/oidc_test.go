package server

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// fakeIdP is a minimal OpenID Connect provider: discovery, JWKS, an authorize
// step the test drives by hand, and a token endpoint that checks PKCE and signs
// ID tokens. Fields control how it misbehaves.
type fakeIdP struct {
	t        *testing.T
	server   *httptest.Server
	key      *rsa.PrivateKey
	otherKey *rsa.PrivateKey // signs tokens that must be rejected
	mu       sync.Mutex
	codes    map[string]idpGrant

	clientID, clientSecret string
	subject, email, name   string
	emailVerified          bool
	signWithOtherKey       bool
	wrongAudience          bool
	wrongNonce             bool
	expired                bool
}

type idpGrant struct{ nonce, challenge string }

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	other, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &fakeIdP{t: t, key: key, otherKey: other, codes: map[string]idpGrant{}, clientID: "hfproxy", clientSecret: "client-secret",
		subject: "sub-123", email: "dana@example.com", name: "Dana", emailVerified: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		base := idp.server.URL
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": base, "authorization_endpoint": base + "/authorize", "token_endpoint": base + "/token", "jwks_uri": base + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"}, "response_types_supported": []string{"code"}, "subject_types_supported": []string{"public"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/token", idp.token)
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)
	return idp
}

// authorize plays the browser's visit to the IdP: it records what the proxy
// asked for and returns the callback URL the IdP would redirect to.
func (idp *fakeIdP) authorize(authURL, redirectState string) string {
	u, err := url.Parse(authURL)
	if err != nil {
		idp.t.Fatal(err)
	}
	q := u.Query()
	if q.Get("code_challenge_method") != "S256" || q.Get("code_challenge") == "" || q.Get("nonce") == "" || q.Get("state") == "" || q.Get("client_id") != idp.clientID {
		idp.t.Fatalf("authorization request is missing PKCE, nonce, state or client_id: %v", q)
	}
	code := randomSecret(12)
	idp.mu.Lock()
	idp.codes[code] = idpGrant{nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	idp.mu.Unlock()
	state := q.Get("state")
	if redirectState != "" {
		state = redirectState
	}
	return q.Get("redirect_uri") + "?code=" + code + "&state=" + url.QueryEscape(state)
}

func (idp *fakeIdP) token(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	idp.mu.Lock()
	grant, ok := idp.codes[r.Form.Get("code")]
	delete(idp.codes, r.Form.Get("code")) // a code works once
	idp.mu.Unlock()
	user, pass, _ := r.BasicAuth()
	if !ok || (user != idp.clientID && r.Form.Get("client_id") != idp.clientID) || (pass != idp.clientSecret && r.Form.Get("client_secret") != idp.clientSecret) {
		http.Error(w, `{"error":"invalid_grant"}`, http.StatusBadRequest)
		return
	}
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != grant.challenge {
		http.Error(w, `{"error":"invalid_grant","error_description":"PKCE verifier mismatch"}`, http.StatusBadRequest)
		return
	}
	key := idp.key
	if idp.signWithOtherKey {
		key = idp.otherKey
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	if err != nil {
		idp.t.Fatal(err)
	}
	aud, nonce, expiry := idp.clientID, grant.nonce, time.Now().Add(time.Hour)
	if idp.wrongAudience {
		aud = "someone-else"
	}
	if idp.wrongNonce {
		nonce = "not-the-nonce"
	}
	if idp.expired {
		expiry = time.Now().Add(-time.Hour)
	}
	claims := map[string]any{"email": idp.email, "email_verified": idp.emailVerified, "name": idp.name, "nonce": nonce}
	raw, err := jwt.Signed(signer).Claims(jwt.Claims{Issuer: idp.server.URL, Subject: idp.subject, Audience: jwt.Audience{aud}, Expiry: jwt.NewNumericDate(expiry), IssuedAt: jwt.NewNumericDate(time.Now().Add(-time.Minute))}).Claims(claims).Serialize()
	if err != nil {
		idp.t.Fatal(err)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": raw})
}

func newOIDCServer(t *testing.T, idp *fakeIdP, autoProvision bool) (*Server, http.Handler, func(string) int) {
	t.Helper()
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthMode = "oidc"
	cfg.PublicURL = "http://127.0.0.1:8080"
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = idp.server.URL, idp.clientID, idp.clientSecret
	cfg.OIDCRedirectURL = "http://127.0.0.1:8080/auth/callback"
	cfg.OIDCAutoProvision = autoProvision
	s := auditAPINewServer(t, store, cfg)
	state, err := makeRuntimeState(t.Context(), cfg, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.runtime.Store(state)
	count := func(query string) int {
		var n int
		if err := store.SQL.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	return s, s.Handler(), count
}

// browserLogin runs /auth/login then the IdP then /auth/callback and returns the
// callback response. tamperState, if set, is sent back instead of the real state.
func browserLogin(t *testing.T, idp *fakeIdP, handler http.Handler, tamperState string) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("/auth/login = %d, want a redirect to the IdP", login.Code)
	}
	var stateCookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "hfproxy_oidc" {
			stateCookie = c
		}
	}
	if stateCookie == nil || !stateCookie.HttpOnly || stateCookie.SameSite != http.SameSiteLaxMode {
		t.Fatalf("the OIDC state cookie must be HttpOnly and SameSite=Lax: %+v", stateCookie)
	}
	callback := idp.authorize(login.Header().Get("Location"), tamperState)
	u, _ := url.Parse(callback)
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	req.AddCookie(stateCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func sessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == "hfproxy_session" && c.Value != "" {
			return c
		}
	}
	return nil
}

func TestOIDCLoginProvisionsAVerifiedUserOnceAndReusesIt(t *testing.T) {
	idp := newFakeIdP(t)
	_, handler, count := newOIDCServer(t, idp, true)

	first := browserLogin(t, idp, handler, "")
	if first.Code != http.StatusSeeOther || first.Header().Get("Location") != "/" || sessionCookie(first) == nil {
		t.Fatalf("login = %d to %q with session %v", first.Code, first.Header().Get("Location"), sessionCookie(first))
	}
	if n := count(`SELECT count(*) FROM users u JOIN external_identities x ON x.user_id=u.id WHERE x.provider='oidc' AND u.email='dana@example.com' AND u.role='reader'`); n != 1 {
		t.Fatalf("%d provisioned readers, want 1", n)
	}
	second := browserLogin(t, idp, handler, "")
	if sessionCookie(second) == nil {
		t.Fatal("the second login failed")
	}
	if n := count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("%d users after two logins, want the same one", n)
	}
}

func TestOIDCLoginRefusesWhatItCannotTrust(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fakeIdP)
		state  string
		auto   bool
		want   string
	}{
		{"state that does not match the cookie", func(*fakeIdP) {}, "forged-state", true, "invalid_state"},
		{"ID token signed by another key", func(i *fakeIdP) { i.signWithOtherKey = true }, "", true, "login_failed"},
		{"ID token for another audience", func(i *fakeIdP) { i.wrongAudience = true }, "", true, "login_failed"},
		{"expired ID token", func(i *fakeIdP) { i.expired = true }, "", true, "login_failed"},
		{"nonce that was not issued", func(i *fakeIdP) { i.wrongNonce = true }, "", true, "login_failed"},
		{"unverified email cannot provision", func(i *fakeIdP) { i.emailVerified = false }, "", true, "provisioning_disabled"},
		{"unknown identity without auto-provisioning", func(*fakeIdP) {}, "", false, "provisioning_disabled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idp := newFakeIdP(t)
			tc.mutate(idp)
			_, handler, count := newOIDCServer(t, idp, tc.auto)
			rec := browserLogin(t, idp, handler, tc.state)
			if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "error="+tc.want) {
				t.Fatalf("callback = %d to %q, want a redirect with error=%s", rec.Code, rec.Header().Get("Location"), tc.want)
			}
			if sessionCookie(rec) != nil {
				t.Fatal("a session was issued")
			}
			if n := count(`SELECT count(*) FROM users`); n != 0 {
				t.Fatalf("%d users were created", n)
			}
		})
	}
}

func TestOIDCCallbackWithoutTheStateCookieIsRefused(t *testing.T) {
	idp := newFakeIdP(t)
	_, handler, _ := newOIDCServer(t, idp, true)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/callback?code=x&state=y", nil))
	if rec.Code != http.StatusSeeOther || !strings.Contains(rec.Header().Get("Location"), "invalid_state") {
		t.Fatalf("callback without cookie = %d %q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestOIDCDisabledAccountCannotSignIn(t *testing.T) {
	idp := newFakeIdP(t)
	s, handler, count := newOIDCServer(t, idp, true)
	if sessionCookie(browserLogin(t, idp, handler, "")) == nil {
		t.Fatal("the first login failed")
	}
	if _, err := s.db.SQL.Exec(`UPDATE users SET status='disabled'`); err != nil {
		t.Fatal(err)
	}
	rec := browserLogin(t, idp, handler, "")
	if sessionCookie(rec) != nil || !strings.Contains(rec.Header().Get("Location"), "error=") {
		t.Fatalf("a disabled account got in: %d %q", rec.Code, rec.Header().Get("Location"))
	}
	if n := count(`SELECT count(*) FROM users`); n != 1 {
		t.Fatalf("%d users, want 1", n)
	}
}
