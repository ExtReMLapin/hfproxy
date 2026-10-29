package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func setupRequest(s *Server, method, target, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.RemoteAddr = "203.0.113.9:4000"
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func setupBody(code, username, password string) string {
	b, _ := json.Marshal(map[string]string{"setup_code": code, "username": username, "password": password, "display_name": "Root", "email": "root@example.com"})
	return string(b)
}

func setupRequiredNow(t *testing.T, s *Server) bool {
	t.Helper()
	rec := setupRequest(s, http.MethodGet, "/api/v1/setup/status", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
	}
	return apiJSON(t, rec.Body.String())["required"].(bool)
}

func TestFirstAdministratorCanBeCreatedFromTheConsole(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var banner bytes.Buffer
	s.setupOut = &banner

	if !setupRequiredNow(t, s) {
		t.Fatal("a fresh instance must ask for setup")
	}
	s.announceSetup(t.Context())
	code := s.setupCode()
	if !strings.Contains(banner.String(), code) || strings.Count(banner.String(), code) != 1 {
		t.Fatalf("the setup code must be printed once for the operator, got %q", banner.String())
	}
	if len(code) < 20 {
		t.Fatalf("the generated code is too short: %q", code)
	}

	// A wrong code is refused and creates nothing, with no session needed (so no CSRF cookie).
	if rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody("not-the-code", "root", "a-long-enough-password")); rec.Code != http.StatusForbidden {
		t.Fatalf("wrong code = %d %s", rec.Code, rec.Body.String())
	}
	if !setupRequiredNow(t, s) {
		t.Fatal("a refused attempt must not complete setup")
	}
	// A right code with a bad profile or password is refused too.
	for name, body := range map[string]string{
		"bad username":   setupBody(code, "no spaces", "a-long-enough-password"),
		"short password": setupBody(code, "root", "short"),
	} {
		if rec := setupRequest(s, http.MethodPost, "/api/v1/setup", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}

	rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody(code, "root", "a-long-enough-password"))
	if rec.Code != http.StatusCreated {
		t.Fatalf("setup = %d %s", rec.Code, rec.Body.String())
	}
	created := apiJSON(t, rec.Body.String())
	if created["username"] != "root" || created["role"] != "admin" {
		t.Fatalf("created = %v", created)
	}
	var session, csrf bool
	var sessionCookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case "hfproxy_session":
			session, sessionCookie = true, c
		case "hfproxy_csrf":
			csrf = true
		}
	}
	if !session || !csrf {
		t.Fatal("the new administrator must be signed in")
	}
	me := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	me.AddCookie(sessionCookie)
	meRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(meRec, me)
	if meRec.Code != http.StatusOK || apiJSON(t, meRec.Body.String())["role"] != "admin" {
		t.Fatalf("the session of the new administrator = %d %s", meRec.Code, meRec.Body.String())
	}

	var action string
	if err := store.SQL.QueryRow(`SELECT action FROM audit_events WHERE action='setup.first_admin'`).Scan(&action); err != nil {
		t.Fatalf("setup must be audited: %v", err)
	}
	if setupRequiredNow(t, s) {
		t.Fatal("setup must be closed once an administrator exists")
	}
	if rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody(code, "second", "a-long-enough-password")); rec.Code != http.StatusConflict {
		t.Fatalf("a second setup = %d, want 409", rec.Code)
	}
}

func TestSetupCodeFromTheEnvironmentIsNeverPrinted(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.SetupToken = "configured-setup-token-0123456789"
	s := auditAPINewServer(t, store, cfg)
	var banner bytes.Buffer
	s.setupOut = &banner
	s.announceSetup(t.Context())
	if strings.Contains(banner.String(), cfg.SetupToken) || !strings.Contains(banner.String(), "HFPROXY_SETUP_TOKEN") {
		t.Fatalf("the configured token must not be echoed: %q", banner.String())
	}
	if rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody("configured-setup-token-0123456789", "root", "a-long-enough-password")); rec.Code != http.StatusCreated {
		t.Fatalf("setup with the configured token = %d %s", rec.Code, rec.Body.String())
	}
}

func TestNothingIsAnnouncedOnceAnAdministratorExists(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	settingsAdmin(t, store)
	var banner bytes.Buffer
	s.setupOut = &banner
	s.announceSetup(t.Context())
	if banner.Len() != 0 {
		t.Fatalf("a banner was printed: %q", banner.String())
	}
	if setupRequiredNow(t, s) {
		t.Fatal("setup must not be offered when an administrator exists")
	}
	if rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody("anything-at-all-0123456789", "root", "a-long-enough-password")); rec.Code != http.StatusConflict {
		t.Fatalf("setup on a configured instance = %d, want 409", rec.Code)
	}
}

func TestSetupCodeGuessesAreRateLimited(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	refused := 0
	for i := 0; i < 15; i++ {
		rec := setupRequest(s, http.MethodPost, "/api/v1/setup", setupBody("guess-number-"+strings.Repeat("x", i), "root", "a-long-enough-password"))
		if rec.Code == http.StatusTooManyRequests {
			refused++
			if rec.Header().Get("Retry-After") == "" {
				t.Error("a rate-limited setup must say when to retry")
			}
		}
	}
	if refused == 0 {
		t.Fatal("unlimited guesses of the setup code were allowed")
	}
	if !setupRequiredNow(t, s) {
		t.Fatal("guessing must not complete setup")
	}
}

func TestSimultaneousSetupsCreateOneAdministrator(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	code := s.setupCode()
	var wg sync.WaitGroup
	statuses := make(chan int, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/v1/setup", strings.NewReader(setupBody(code, "admin"+strings.Repeat("y", i), "a-long-enough-password")))
			req.RemoteAddr = "198.51.100." + string(rune('1'+i)) + ":4000"
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			statuses <- rec.Code
		}(i)
	}
	wg.Wait()
	close(statuses)
	created := 0
	for status := range statuses {
		if status == http.StatusCreated {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("%d administrators created, want 1", created)
	}
	var admins int
	store.SQL.QueryRow(`SELECT count(*) FROM users WHERE role='admin'`).Scan(&admins)
	if admins != 1 {
		t.Fatalf("%d administrators in the database", admins)
	}
}

func TestSetupEndpointsOnlyAcceptTheirMethods(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	if rec := setupRequest(s, http.MethodGet, "/api/v1/setup", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /setup = %d", rec.Code)
	}
	if rec := setupRequest(s, http.MethodPost, "/api/v1/setup/status", "{}"); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST /setup/status = %d", rec.Code)
	}
	if rec := setupRequest(s, http.MethodGet, "/api/v1/setup/other", ""); rec.Code == http.StatusOK {
		t.Errorf("an unknown setup path answered 200")
	}
}

func TestSetupReopensWhenEveryAdministratorIsGone(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var banner bytes.Buffer
	s.setupOut = &banner
	admin := settingsAdmin(t, store)
	if setupRequiredNow(t, s) {
		t.Fatal("setup offered while an administrator exists")
	}
	store.SQL.Exec(`UPDATE users SET status='disabled' WHERE id=$1`, admin.ID)
	// The answer "an administrator exists" is remembered briefly...
	if setupRequiredNow(t, s) {
		t.Fatal("the remembered answer must be used within its lifetime")
	}
	// ...and checked again once that time has passed.
	s.adminKnownUntil.Store(0)
	if !setupRequiredNow(t, s) {
		t.Fatal("setup must reopen when no active administrator is left")
	}
	if !strings.Contains(banner.String(), s.setupCode()) {
		t.Fatalf("a code must be printed when setup reopens, got %q", banner.String())
	}
}
