package config

import (
	"math"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParsePrefixes(t *testing.T) {
	got, err := parsePrefixes([]string{"10.0.0.7/8", "2001:db8::/32"})
	if err != nil {
		t.Fatal(err)
	}
	want := []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("2001:db8::/32")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("parsePrefixes() = %v, want %v", got, want)
	}
	if _, err := parsePrefixes([]string{"not-a-cidr"}); err == nil {
		t.Fatal("parsePrefixes accepted an invalid CIDR")
	}
}

func TestObservabilityValidation(t *testing.T) {
	c := Config{MetricsPath: "/healthz"}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "METRICS_PATH") {
		t.Fatalf("reserved metrics path error = %v", err)
	}
	c = Config{MetricsPath: "/metrics", TracingSamplingRatio: math.NaN()}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "SAMPLING_RATIO") {
		t.Fatalf("NaN sampling ratio error = %v", err)
	}
}

func TestNormalizeAllowedHosts(t *testing.T) {
	got, err := NormalizeAllowedHosts([]string{" *.HF.co. ", "HF.co"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "*.hf.co" || got[1] != "hf.co" {
		t.Fatalf("normalized hosts = %#v", got)
	}
	for _, value := range []string{"*", "*.co", "foo/bar"} {
		if _, err := NormalizeAllowedHosts([]string{value}); err == nil {
			t.Errorf("NormalizeAllowedHosts(%q) accepted invalid host", value)
		}
	}
}

func TestIssue93RejectsEmptySecretFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "hf-proxy-url")
	if err := os.WriteFile(file, []byte("\n\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HFPROXY_HF_PROXY_URL_FILE", file)
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("empty secret file was accepted")
	}
}

func TestValidateEnvironmentOverridesRejectsInvalidValues(t *testing.T) {
	t.Setenv("HFPROXY_HF_TIMEOUT", "30")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid duration was accepted")
	}
	t.Setenv("HFPROXY_HF_TIMEOUT", "")
	t.Setenv("HFPROXY_CACHE_MAX_FILE_SIZE", "10GiB")
	if err := validateEnvironmentOverrides(); err == nil {
		t.Fatal("invalid cache size was accepted")
	}
}

func TestCheckDeploymentRejectsUnsafeSharedSetups(t *testing.T) {
	const devSecret = "dev-only-session-secret-change-before-deployment-32-bytes"
	cases := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{"dev secret on loopback public URL", Config{SessionSecret: devSecret, PublicURL: "http://localhost:8080", ListenAddr: "0.0.0.0:8080"}, ""},
		{"dev secret on public host", Config{SessionSecret: devSecret, PublicURL: "https://hf.example.com", ListenAddr: "0.0.0.0:8080"}, "development value"},
		{"dev secret without public URL", Config{SessionSecret: devSecret, ListenAddr: "127.0.0.1:8080"}, "development value"},
		{"missing public URL on all interfaces", Config{SessionSecret: strings.Repeat("s", 40), ListenAddr: "0.0.0.0:8080"}, "PUBLIC_URL is required"},
		{"missing public URL on loopback", Config{SessionSecret: strings.Repeat("s", 40), ListenAddr: "127.0.0.1:8080"}, ""},
		{"complete production setup", Config{SessionSecret: strings.Repeat("s", 40), PublicURL: "https://hf.example.com", ListenAddr: "0.0.0.0:8080"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.CheckDeployment()
			if tc.wantErr == "" && err != nil {
				t.Fatalf("CheckDeployment() = %v, want nil", err)
			}
			if tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("CheckDeployment() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateRejectsInvalidOperationalLimits(t *testing.T) {
	base := func() Config {
		return Config{
			LogLevel: "info", DatabaseURL: "postgres://x", HFBaseURL: "https://huggingface.co", HFTimeout: 1, CacheMaxFileSize: 1,
			CacheRefTTL: 1, AuthMode: "local", SessionSecret: strings.Repeat("s", 40), SessionIdleTTL: 1, SessionAbsoluteTTL: 1,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("baseline config invalid: %v", err)
	}
	for name, mutate := range map[string]func(*Config){
		"negative total quota":             func(c *Config) { c.CacheMaxTotalSize = -1 },
		"negative concurrency":             func(c *Config) { c.CacheFillConcurrency = -1 },
		"negative download period":         func(c *Config) { c.RetentionDownloads = -1 },
		"negative audit period":            func(c *Config) { c.RetentionAudit = -1 },
		"metrics address":                  func(c *Config) { c.MetricsAddr = "no-port" },
		"cache file size beyond multipart": func(c *Config) { c.CacheMaxFileSize = MaxCacheFileSize + 1 },
		"shutdown grace":                   func(c *Config) { c.ShutdownGrace = -time.Second },
		"negative staging minimum":         func(c *Config) { c.StagingMinFreeBytes = -1 },
	} {
		c := base()
		mutate(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: Validate() accepted the value", name)
		}
	}
}

func TestOverriddenFieldsNamesOnlyDifferingSettings(t *testing.T) {
	environment := Config{HFBaseURL: "https://huggingface.co", HFToken: "env-secret", S3Bucket: "env-bucket", SessionSecret: strings.Repeat("s", 40), LogLevel: "info"}
	saved := environment
	saved.S3Bucket = "console-bucket"
	saved.HFToken = "console-secret"
	effective := managedFromConfig(saved).apply(environment)
	got := OverriddenFields(environment, effective)
	if strings.Join(got, ",") != "HFToken,S3Bucket" {
		t.Fatalf("OverriddenFields = %v, want [HFToken S3Bucket]", got)
	}
	for _, name := range got {
		if strings.Contains(name, "secret") {
			t.Fatalf("field list leaks a value: %v", got)
		}
	}
	if len(OverriddenFields(environment, environment)) != 0 {
		t.Fatal("identical configurations must report no overrides")
	}
}
