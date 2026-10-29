package config

import "testing"

func TestPrivateNetsMustLieInsidePrivateRanges(t *testing.T) {
	good := []string{"10.96.0.0/12", "10.0.0.0/8", "172.20.1.0/24", "192.168.1.5/32", "100.64.0.0/10", "fd12:3456::/32", "198.18.4.0/24"}
	nets, err := ParsePrivateNets(good)
	if err != nil || len(nets) != len(good) {
		t.Fatalf("valid ranges = %v %v", nets, err)
	}
	if nets[0].String() != "10.96.0.0/12" {
		t.Fatalf("not masked: %s", nets[0])
	}
	for _, bad := range []string{
		"0.0.0.0/0", "::/0", // everything
		"8.8.8.0/24", "13.33.0.0/16", // public: reachable already, so a typo
		"10.0.0.0/7", "172.0.0.0/8", "192.0.0.0/8", // wider than the private range
		"169.254.0.0/16", "169.254.169.254/32", "fe80::/10", // link-local: metadata services
		"127.0.0.0/8", "::1/128", // loopback
		"10.0.0.1", "not-a-cidr", "",
	} {
		if nets, err := ParsePrivateNets([]string{bad}); err == nil {
			t.Errorf("%q was accepted: %v", bad, nets)
		}
	}
	if nets, err := ParsePrivateNets(nil); err != nil || len(nets) != 0 {
		t.Fatalf("an empty list = %v %v", nets, err)
	}
}

func TestPrivateCIDRsComeFromTheEnvironment(t *testing.T) {
	t.Setenv("HFPROXY_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("HFPROXY_DATABASE_URL", "postgres://u:p@db/app")
	t.Setenv("HFPROXY_HF_ALLOWED_PRIVATE_CIDRS", "10.20.0.0/16, 192.168.7.0/24")
	cfg, err := Load()
	if err != nil || len(cfg.HFAllowedPrivateNets) != 2 {
		t.Fatalf("Load = %v nets, %v", cfg.HFAllowedPrivateNets, err)
	}
	t.Setenv("HFPROXY_HF_ALLOWED_PRIVATE_CIDRS", "0.0.0.0/0")
	if _, err := Load(); err == nil {
		t.Fatal("a catch-all CIDR was accepted at startup")
	}
}
