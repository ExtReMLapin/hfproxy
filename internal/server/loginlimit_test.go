package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func loginRequest(ip string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.RemoteAddr = ip + ":40000"
	return req
}

func TestLoginLimitsAreSharedBetweenInstances(t *testing.T) {
	store := auditAPIFreshStore(t)
	first := auditAPINewServer(t, store, auditAPIConfig())
	second := auditAPINewServer(t, store, auditAPIConfig())
	for i := 1; i <= loginPairLimit; i++ {
		instance := first
		if i%2 == 0 {
			instance = second
		}
		if retry, ok := instance.allowLoginAttempt(loginRequest("198.51.100.7"), "alice"); !ok {
			t.Fatalf("attempt %d was refused (retry after %ds)", i, retry)
		}
	}
	retry, ok := second.allowLoginAttempt(loginRequest("198.51.100.7"), "alice")
	if ok {
		t.Fatal("the attempt over the per-account limit was allowed on the other instance")
	}
	if retry < 1 || retry > 60 {
		t.Fatalf("retry after %ds, want between 1 and 60", retry)
	}
	if _, ok := first.allowLoginAttempt(loginRequest("198.51.100.8"), "alice"); !ok {
		t.Fatal("another address must not be blocked by the first one's attempts")
	}
}

func TestLoginLimitPerAddressAcrossUsernames(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	for i := 1; i <= loginIPLimit; i++ {
		if retry, ok := s.allowLoginAttempt(loginRequest("203.0.113.9"), fmt.Sprintf("user%d", i)); !ok {
			t.Fatalf("attempt %d was refused (retry after %ds)", i, retry)
		}
	}
	if _, ok := s.allowLoginAttempt(loginRequest("203.0.113.9"), "one-more"); ok {
		t.Fatal("an address guessing many usernames was not limited")
	}
}

func TestLoginWindowExpires(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	for i := 0; i <= loginPairLimit; i++ {
		s.allowLoginAttempt(loginRequest("198.51.100.20"), "bob")
	}
	if _, ok := s.allowLoginAttempt(loginRequest("198.51.100.20"), "bob"); ok {
		t.Fatal("the limit was not enforced before the window expired")
	}
	if _, err := store.SQL.Exec(`UPDATE login_attempts SET window_start = now() - interval '2 minutes'`); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.allowLoginAttempt(loginRequest("198.51.100.20"), "bob"); !ok {
		t.Fatal("the window did not reset after a minute")
	}
}

func TestAccountLimitCountsOnlyFailuresAcrossAddresses(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	// Many addresses logging in successfully never use up the account's budget.
	for i := 0; i < loginUserFailureLimit+10; i++ {
		if _, ok := s.allowLoginAttempt(loginRequest(fmt.Sprintf("192.0.2.%d", i%250+1)), "carol"); !ok && i < 200 {
			// the per-address limits are not reached here: each address is used at most twice
			t.Fatalf("attempt %d from a distinct address was refused although no attempt failed", i)
		}
	}
	for i := 0; i < loginUserFailureLimit; i++ {
		s.recordLoginFailure(t.Context(), "Carol ")
	}
	retry, ok := s.allowLoginAttempt(loginRequest("203.0.113.200"), "carol")
	if ok {
		t.Fatal("an account under a distributed guessing attack was not limited")
	}
	if retry < 1 {
		t.Fatalf("retry after %ds, want at least 1", retry)
	}
	if _, ok := s.allowLoginAttempt(loginRequest("203.0.113.200"), "dave"); !ok {
		t.Fatal("other accounts must not be affected")
	}
}

func TestLoginLimitsFallBackToLocalCountersWhenTheDatabaseFails(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	store.SQL.Close()
	for i := 1; i <= 10; i++ {
		if _, ok := s.allowLoginAttempt(loginRequest("198.51.100.30"), "erin"); !ok {
			t.Fatalf("local fallback refused attempt %d", i)
		}
	}
	if _, ok := s.allowLoginAttempt(loginRequest("198.51.100.30"), "erin"); ok {
		t.Fatal("the local fallback did not enforce a limit")
	}
}

func TestFailedLoginsFeedTheAccountLimit(t *testing.T) {
	s := newAuditAuthServer(t)
	auditAuthCreateUser(t, s, "frank", "reader", "a-long-enough-password")
	post := func(password string) *httptest.ResponseRecorder {
		get := httptest.NewRecorder()
		s.Handler().ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/login", nil))
		var csrf string
		for _, c := range get.Result().Cookies() {
			if c.Name == "hfproxy_login_csrf" {
				csrf = c.Value
			}
		}
		form := url.Values{"username": {"frank"}, "password": {password}, "csrf_token": {csrf}}
		req := httptest.NewRequest(http.MethodPost, "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.AddCookie(&http.Cookie{Name: "hfproxy_login_csrf", Value: csrf})
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	for i := 0; i < 3; i++ {
		if rec := post("wrong-password-value"); rec.Code != http.StatusUnauthorized {
			t.Fatalf("wrong password = %d, want 401", rec.Code)
		}
	}
	var failures int
	if err := s.db.SQL.QueryRow(`SELECT attempts FROM login_attempts WHERE key=$1`, loginKey("fail", "frank")).Scan(&failures); err != nil || failures != 3 {
		t.Fatalf("recorded failures = %d (%v), want 3", failures, err)
	}
	if rec := post("a-long-enough-password"); rec.Code != http.StatusSeeOther {
		t.Fatalf("correct password = %d, want a redirect", rec.Code)
	}
	if err := s.db.SQL.QueryRow(`SELECT attempts FROM login_attempts WHERE key=$1`, loginKey("fail", "frank")).Scan(&failures); err != nil || failures != 3 {
		t.Fatalf("a successful login changed the failure count to %d (%v)", failures, err)
	}
}
