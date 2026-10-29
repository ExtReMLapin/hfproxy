package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestIdentityAddressPolicy(t *testing.T) {
	allowed := []string{"10.1.2.3", "192.168.0.5", "172.20.0.9", "100.64.0.1", "13.33.33.33", "2606:4700::1", "fd12::5", "::ffff:10.1.2.3"}
	for _, value := range allowed {
		if !identityAddrAllowed(netip.MustParseAddr(value), false) {
			t.Errorf("%s is a legitimate identity server address but was refused", value)
		}
	}
	refused := []string{
		"169.254.169.254", "169.254.0.1", "::ffff:169.254.169.254", // cloud metadata, link-local
		"fd00:ec2::254", "100.100.100.200", "168.63.129.16", // other metadata services
		"fe80::1", "0.0.0.0", "::", "224.0.0.1", "ff02::1", // link-local, unspecified, multicast
		"127.0.0.1", "::1", "::ffff:127.0.0.1", // loopback outside development
	}
	for _, value := range refused {
		if identityAddrAllowed(netip.MustParseAddr(value), false) {
			t.Errorf("%s was allowed for an identity provider", value)
		}
	}
	if !identityAddrAllowed(netip.MustParseAddr("127.0.0.1"), true) || !identityAddrAllowed(netip.MustParseAddr("::1"), true) {
		t.Error("loopback must work when the configured server is on loopback")
	}
	if identityAddrAllowed(netip.MustParseAddr("169.254.169.254"), true) {
		t.Error("metadata addresses are refused even in development")
	}
}

func TestOIDCClientRefusesWhatItMustNotReach(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) }))
	defer upstream.Close()

	// The issuer is on a public name here, so loopback is refused: a DNS answer
	// or a redirect cannot send the client to a local service.
	client := newOIDCHTTPClient("https://idp.example.com")
	if resp, err := client.Get(upstream.URL); err == nil {
		resp.Body.Close()
		t.Fatal("the client reached a loopback service on behalf of a public issuer")
	} else if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("unexpected error: %v", err)
	}
	started := time.Now()
	if resp, err := client.Get("http://169.254.169.254/latest/meta-data/"); err == nil {
		resp.Body.Close()
		t.Fatal("the client reached the metadata address")
	} else if !strings.Contains(err.Error(), "not allowed") || time.Since(started) > 3*time.Second {
		t.Fatalf("metadata address: %v after %s", err, time.Since(started))
	}

	// A loopback issuer (development) works.
	dev := newOIDCHTTPClient(upstream.URL)
	resp, err := dev.Get(upstream.URL)
	if err != nil {
		t.Fatalf("a loopback issuer must work: %v", err)
	}
	resp.Body.Close()
	if dev.Timeout == 0 {
		t.Error("the client must have a timeout")
	}

	// A redirect from a loopback issuer to the metadata address is refused too.
	redirector := httptest.NewServer(http.RedirectHandler("http://169.254.169.254/", http.StatusFound))
	defer redirector.Close()
	if resp, err := newOIDCHTTPClient(redirector.URL).Get(redirector.URL); err == nil {
		resp.Body.Close()
		t.Fatal("a redirect to the metadata address was followed")
	}
}

func TestLDAPRefusesAMetadataAddress(t *testing.T) {
	cfg := auditAPIConfig()
	cfg.LDAPURL, cfg.LDAPBaseDN = "ldap://169.254.169.254:389", "dc=example,dc=org"
	s := auditAPINewServer(t, auditAPIFreshStore(t), cfg)
	started := time.Now()
	if _, err := s.authenticateLDAP(context.Background(), "alice", "password"); err == nil {
		t.Fatal("authenticateLDAP connected to a metadata address")
	}
	if time.Since(started) > 3*time.Second {
		t.Fatalf("the refusal took %s: the connection was attempted", time.Since(started))
	}
	if !ldapHostIsLoopback("ldap://localhost:389") || !ldapHostIsLoopback("ldaps://127.0.0.1") || ldapHostIsLoopback("ldaps://ldap.example.com") {
		t.Error("ldapHostIsLoopback is wrong")
	}
}
