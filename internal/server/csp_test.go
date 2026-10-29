package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMetricsOnThePublicPortNeedAnOperator(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.MetricsEnabled, cfg.MetricsPath = true, "/metrics"
	s := auditAPINewServer(t, store, cfg)
	handler := s.Handler()

	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous GET /metrics = %d, want 401", anonymous.Code)
	}

	_, reader := auditAPICreateUser(t, store, "metrics-reader", "reader")
	asReader := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	asReader.Header.Set("Authorization", "Bearer "+reader)
	readerRec := httptest.NewRecorder()
	handler.ServeHTTP(readerRec, asReader)
	if readerRec.Code != http.StatusForbidden {
		t.Fatalf("reader GET /metrics = %d, want 403", readerRec.Code)
	}

	operatorID, _ := auditAPICreateUser(t, store, "metrics-operator", "operator")
	rec := auditAPIDoAs(s, &Principal{ID: operatorID, Role: "operator", Authenticated: true, TokenID: "t", Scopes: []string{"write"}}, http.MethodGet, "/metrics", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "hfproxy_") {
		t.Fatalf("operator GET /metrics = %d", rec.Code)
	}

	// On a dedicated listener the endpoint stays credential-free: that port is
	// meant for an internal network.
	dedicated := httptest.NewRecorder()
	s.metricsHandler().ServeHTTP(dedicated, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if dedicated.Code != http.StatusOK {
		t.Fatalf("dedicated metrics listener = %d, want 200", dedicated.Code)
	}
}

func TestUpstreamPortsAreLimitedToStandardOnesAndTheConfiguredHub(t *testing.T) {
	base, _ := url.Parse("https://mirror.example.com:8443")
	for target, want := range map[string]bool{
		"https://huggingface.co/x":                true,
		"https://huggingface.co:443/x":            true,
		"https://mirror.example.com:8443/x":       true, // the configured Hub keeps its port
		"https://MIRROR.example.com:8443/x":       true,
		"https://cdn.example.com:8443/x":          false, // another host on that port
		"https://mirror.example.com:9000/x":       false, // another port on the Hub host
		"https://huggingface.co:8080/x":           false,
		"http://127.0.0.1:80/x":                   true,
		"http://example.com:80/x":                 false,
		"https://internal.example.com:6379/redis": false,
	} {
		u, _ := url.Parse(target)
		if got := upstreamPortAllowed(u, base); got != want {
			t.Errorf("upstreamPortAllowed(%s) = %v, want %v", target, got, want)
		}
	}
	plain, _ := url.Parse("https://huggingface.co")
	other, _ := url.Parse("https://huggingface.co:8443/x")
	if upstreamPortAllowed(other, plain) {
		t.Error("a Hub URL without a port must not open other ports")
	}
}
