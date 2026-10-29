package server

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestContentSecurityPolicyHasNoUnsafeInline(t *testing.T) {
	if policy := contentSecurityPolicy(""); strings.Contains(policy, "unsafe-inline") {
		t.Fatalf("policy allows inline styles: %s", policy)
	}
	if policy := contentSecurityPolicy("abc"); !strings.Contains(policy, "style-src 'self' 'nonce-abc'") {
		t.Fatalf("nonce missing from %s", policy)
	}
}

func TestSecurityHeadersCoverIsolationPolicies(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	s.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	for _, header := range []string{"Permissions-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy", "Content-Security-Policy", "X-Content-Type-Options"} {
		if rec.Header().Get(header) == "" {
			t.Errorf("%s missing", header)
		}
	}
	if strings.Contains(rec.Header().Get("Content-Security-Policy"), "unsafe-inline") {
		t.Error("the default policy must not allow inline styles")
	}
}

func TestLoginPageHasNoInlineStyle(t *testing.T) {
	if strings.Contains(loginPage, "<style") || strings.Contains(loginPage, "style=") {
		t.Fatal("the login page must style itself through /assets/app.css only")
	}
}

func TestFrontendIndexGetsAFreshNonce(t *testing.T) {
	s := &Server{}
	nonces := map[string]bool{}
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		s.frontend(rec, httptest.NewRequest(http.MethodGet, "/repositories", nil))
		if rec.Code != http.StatusOK {
			t.Skipf("admin interface not built (status %d)", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(strings.ToLower(body), "ngcspnonce=") {
			t.Skip("built index.html has no ngCspNonce placeholder (rebuild the UI)")
		}
		if strings.Contains(body, cspNoncePlaceholder) {
			t.Fatal("the nonce placeholder was not replaced")
		}
		match := regexp.MustCompile(`(?i)ngcspnonce="([^"]+)"`).FindStringSubmatch(body)
		if match == nil || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "'nonce-"+match[1]+"'") {
			t.Fatalf("the policy %q does not carry the nonce in the page", rec.Header().Get("Content-Security-Policy"))
		}
		nonces[match[1]] = true
	}
	if len(nonces) != 2 {
		t.Fatal("each response needs its own nonce")
	}
}
