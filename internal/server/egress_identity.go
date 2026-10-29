package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"syscall"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
)

// The OIDC issuer and the LDAP server are set by an administrator and are often
// on the internal network, so private addresses are allowed. What is never a
// legitimate identity server is checked on the address actually connected to,
// after name resolution (so a DNS answer cannot redirect the connection):
// cloud instance metadata services, link-local, unspecified and multicast
// addresses, and loopback unless the configured server itself is on loopback
// (development).
var specialServiceAddrs = []netip.Prefix{
	netip.MustParsePrefix("169.254.0.0/16"),     // AWS, GCP, Azure, DigitalOcean... metadata (also link-local)
	netip.MustParsePrefix("fd00:ec2::254/128"),  // AWS metadata over IPv6
	netip.MustParsePrefix("100.100.100.200/32"), // Alibaba Cloud metadata
	netip.MustParsePrefix("168.63.129.16/32"),   // Azure wire server
}

func identityAddrAllowed(ip netip.Addr, allowLoopback bool) bool {
	ip = ip.Unmap()
	if ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsInterfaceLocalMulticast() {
		return false
	}
	for _, prefix := range specialServiceAddrs {
		if prefix.Contains(ip) {
			return false
		}
	}
	if ip.IsLoopback() {
		return allowLoopback
	}
	return true
}

// identityDialControl is a net.Dialer.Control hook: it runs once the address is
// resolved, just before connecting.
func identityDialControl(allowLoopback bool) func(network, address string, _ syscall.RawConn) error {
	return func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip, err := netip.ParseAddr(host)
		if err != nil || !identityAddrAllowed(ip, allowLoopback) {
			return fmt.Errorf("connection to %s is not allowed for an identity provider", host)
		}
		return nil
	}
}

func newIdentityDialer(timeout time.Duration, allowLoopback bool) *net.Dialer {
	return &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: identityDialControl(allowLoopback)}
}

// newOIDCHTTPClient is the client used to discover the issuer, fetch its keys
// and exchange authorization codes. It has a timeout (the default client has
// none) and follows only a few redirects.
func newOIDCHTTPClient(issuer string) *http.Client {
	host := ""
	if u, err := url.Parse(issuer); err == nil {
		host = u.Hostname()
	}
	dialer := newIdentityDialer(10*time.Second, config.IsLoopbackHost(host))
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, address)
	}
	return &http.Client{Transport: transport, Timeout: 20 * time.Second, CheckRedirect: func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return http.ErrUseLastResponse
		}
		return nil
	}}
}
