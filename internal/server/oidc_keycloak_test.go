package server

import (
	"context"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
)

// These tests sign in through a real Keycloak. deploy/ci/keycloak-up.sh starts
// one with a realm, a client and two users and prints the variables below;
// without them the tests are skipped (the "oidc" CI job runs them).

type keycloakEnv struct{ issuer, clientID, clientSecret string }

func keycloakFromEnv(t *testing.T) keycloakEnv {
	t.Helper()
	k := keycloakEnv{os.Getenv("HFPROXY_TEST_OIDC_ISSUER"), os.Getenv("HFPROXY_TEST_OIDC_CLIENT_ID"), os.Getenv("HFPROXY_TEST_OIDC_CLIENT_SECRET")}
	if k.issuer == "" || k.clientID == "" || k.clientSecret == "" {
		t.Skip("HFPROXY_TEST_OIDC_ISSUER, _CLIENT_ID and _CLIENT_SECRET are not set (see deploy/ci/keycloak-up.sh)")
	}
	return k
}

func newKeycloakServer(t *testing.T, k keycloakEnv, secret string, autoProvision bool) (*Server, http.Handler, func(string) int) {
	t.Helper()
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthMode = "oidc"
	cfg.PublicURL = "http://localhost:8080"
	cfg.OIDCIssuer, cfg.OIDCClientID, cfg.OIDCClientSecret = k.issuer, k.clientID, secret
	cfg.OIDCRedirectURL = "http://localhost:8080/auth/callback"
	cfg.OIDCAutoProvision = autoProvision
	s := auditAPINewServer(t, store, cfg)
	state, err := makeRuntimeState(t.Context(), cfg, nil, nil)
	if err != nil {
		t.Fatalf("OIDC discovery against Keycloak: %v", err)
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

var (
	keycloakFormTag = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*>`)
	keycloakAction  = regexp.MustCompile(`action="([^"]+)"`)
)

// keycloakSignIn plays the browser against Keycloak: it opens the authorization
// URL, submits the login form and returns Keycloak's answer. A successful sign-in
// answers with a redirect to the application; a refused one renders the form
// again, in which case the redirect URL is empty and the page is returned.
func keycloakSignIn(t *testing.T, authURL, username, password string) (redirect, page string) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	read := func(resp *http.Response) string {
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}
	resp, err := client.Get(authURL)
	if err != nil {
		t.Fatalf("open the authorization URL: %v", err)
	}
	form := read(resp)
	tag := keycloakFormTag.FindString(form)
	action := keycloakAction.FindStringSubmatch(tag)
	if resp.StatusCode != http.StatusOK || action == nil {
		t.Fatalf("Keycloak did not offer a login form (status %d): %.300s", resp.StatusCode, form)
	}
	post, err := client.PostForm(html.UnescapeString(action[1]), url.Values{"username": {username}, "password": {password}, "credentialId": {""}})
	if err != nil {
		t.Fatalf("submit the login form: %v", err)
	}
	body := read(post)
	if post.StatusCode == http.StatusFound || post.StatusCode == http.StatusSeeOther {
		return post.Header.Get("Location"), body
	}
	return "", body
}

// keycloakLogin runs /auth/login, Keycloak's form and /auth/callback.
func keycloakLogin(t *testing.T, handler http.Handler, username, password, tamperState string) *httptest.ResponseRecorder {
	t.Helper()
	login := httptest.NewRecorder()
	handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if login.Code != http.StatusFound {
		t.Fatalf("/auth/login = %d, want a redirect to Keycloak", login.Code)
	}
	var stateCookie *http.Cookie
	for _, c := range login.Result().Cookies() {
		if c.Name == "hfproxy_oidc" {
			stateCookie = c
		}
	}
	if stateCookie == nil {
		t.Fatal("no OIDC state cookie")
	}
	redirect, page := keycloakSignIn(t, login.Header().Get("Location"), username, password)
	if redirect == "" {
		t.Fatalf("Keycloak refused %s: %.200s", username, page)
	}
	u, err := url.Parse(redirect)
	if err != nil || u.Query().Get("code") == "" {
		t.Fatalf("Keycloak redirected to %q, want a code", redirect)
	}
	if tamperState != "" {
		q := u.Query()
		q.Set("state", tamperState)
		u.RawQuery = q.Encode()
	}
	req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
	req.AddCookie(stateCookie)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func TestOIDCKeycloak(t *testing.T) {
	k := keycloakFromEnv(t)

	t.Run("a verified user is provisioned once and signs in again", func(t *testing.T) {
		_, handler, count := newKeycloakServer(t, k, k.clientSecret, true)
		first := keycloakLogin(t, handler, "dana", "dana-password", "")
		if first.Code != http.StatusSeeOther || first.Header().Get("Location") != "/" || sessionCookie(first) == nil {
			t.Fatalf("login = %d to %q with session %v", first.Code, first.Header().Get("Location"), sessionCookie(first))
		}
		if n := count(`SELECT count(*) FROM users u JOIN external_identities x ON x.user_id=u.id WHERE x.provider='oidc' AND u.email='dana@example.com' AND u.display_name='Dana Smith' AND u.role='reader'`); n != 1 {
			t.Fatalf("%d provisioned readers with Keycloak's email and name, want 1", n)
		}
		second := keycloakLogin(t, handler, "dana", "dana-password", "")
		if sessionCookie(second) == nil {
			t.Fatal("the second login failed")
		}
		if n := count(`SELECT count(*) FROM users`); n != 1 {
			t.Fatalf("%d users after two logins, want the same one", n)
		}
	})

	t.Run("a user whose email is not verified is not provisioned", func(t *testing.T) {
		_, handler, count := newKeycloakServer(t, k, k.clientSecret, true)
		rec := keycloakLogin(t, handler, "erin", "erin-password", "")
		if !strings.Contains(rec.Header().Get("Location"), "/login?error=") || sessionCookie(rec) != nil {
			t.Fatalf("unverified email: %d to %q with session %v", rec.Code, rec.Header().Get("Location"), sessionCookie(rec))
		}
		if n := count(`SELECT count(*) FROM users`); n != 0 {
			t.Fatalf("%d users were created for an unverified email", n)
		}
	})

	t.Run("without auto-provisioning an unknown user is refused", func(t *testing.T) {
		_, handler, count := newKeycloakServer(t, k, k.clientSecret, false)
		rec := keycloakLogin(t, handler, "dana", "dana-password", "")
		if sessionCookie(rec) != nil || count(`SELECT count(*) FROM users`) != 0 {
			t.Fatalf("an unknown user signed in without auto-provisioning (%d)", rec.Code)
		}
	})

	t.Run("a wrong password never reaches the application", func(t *testing.T) {
		_, handler, _ := newKeycloakServer(t, k, k.clientSecret, true)
		login := httptest.NewRecorder()
		handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		redirect, page := keycloakSignIn(t, login.Header().Get("Location"), "dana", "not-her-password")
		if redirect != "" || !strings.Contains(strings.ToLower(page), "invalid") {
			t.Fatalf("Keycloak accepted a wrong password: redirect %q", redirect)
		}
	})

	t.Run("a tampered state is refused", func(t *testing.T) {
		_, handler, count := newKeycloakServer(t, k, k.clientSecret, true)
		rec := keycloakLogin(t, handler, "dana", "dana-password", "not-the-state")
		if rec.Header().Get("Location") != "/login?error=invalid_state" || sessionCookie(rec) != nil || count(`SELECT count(*) FROM users`) != 0 {
			t.Fatalf("tampered state: %d to %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("an authorization code works only once", func(t *testing.T) {
		_, handler, _ := newKeycloakServer(t, k, k.clientSecret, true)
		login := httptest.NewRecorder()
		handler.ServeHTTP(login, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
		var stateCookie *http.Cookie
		for _, c := range login.Result().Cookies() {
			if c.Name == "hfproxy_oidc" {
				stateCookie = c
			}
		}
		redirect, _ := keycloakSignIn(t, login.Header().Get("Location"), "dana", "dana-password")
		u, err := url.Parse(redirect)
		if err != nil || u.Query().Get("code") == "" {
			t.Fatalf("no code in %q", redirect)
		}
		call := func() *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodGet, u.RequestURI(), nil)
			req.AddCookie(stateCookie)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			return rec
		}
		if first := call(); sessionCookie(first) == nil {
			t.Fatalf("the first use of the code failed: %q", first.Header().Get("Location"))
		}
		if replay := call(); sessionCookie(replay) != nil || replay.Header().Get("Location") != "/login?error=login_failed" {
			t.Fatalf("a replayed code was accepted: %d to %q", replay.Code, replay.Header().Get("Location"))
		}
	})

	t.Run("a wrong client secret fails the code exchange", func(t *testing.T) {
		_, handler, count := newKeycloakServer(t, k, "wrong-secret", true)
		rec := keycloakLogin(t, handler, "dana", "dana-password", "")
		if rec.Header().Get("Location") != "/login?error=login_failed" || sessionCookie(rec) != nil || count(`SELECT count(*) FROM users`) != 0 {
			t.Fatalf("wrong client secret: %d to %q", rec.Code, rec.Header().Get("Location"))
		}
	})

	t.Run("a disabled local account cannot sign in", func(t *testing.T) {
		s, handler, count := newKeycloakServer(t, k, k.clientSecret, true)
		if rec := keycloakLogin(t, handler, "dana", "dana-password", ""); sessionCookie(rec) == nil {
			t.Fatal("the first login failed")
		}
		if _, err := s.db.SQL.ExecContext(context.Background(), `UPDATE users SET status='disabled'`); err != nil {
			t.Fatal(err)
		}
		if rec := keycloakLogin(t, handler, "dana", "dana-password", ""); sessionCookie(rec) != nil {
			t.Fatal("a disabled account signed in")
		}
		if n := count(`SELECT count(*) FROM users`); n != 1 {
			t.Fatalf("%d users, want the disabled one only", n)
		}
	})
}
