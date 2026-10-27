package config

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr             string
	PublicURL              string
	LogLevel               string
	DatabaseURL            string
	AutoMigrate            bool
	HFBaseURL              string
	HFToken                string
	HFAllowedHosts         []string
	HFTimeout              time.Duration
	S3Endpoint             string
	S3Region               string
	S3Bucket               string
	S3Prefix               string
	S3ForcePathStyle       bool
	S3AccessKey            string
	S3SecretKey            string
	S3ServerSideEncryption bool
	AuthMode               string
	AnonymousPublicRead    bool
	SessionSecret          string
	SessionIdleTTL         time.Duration
	SessionAbsoluteTTL     time.Duration
	CacheMaxFileSize       int64
	CacheRefTTL            time.Duration
	CacheNegativeTTL       time.Duration
	CacheGCGrace           time.Duration
	OIDCIssuer             string
	OIDCClientID           string
	OIDCClientSecret       string
	OIDCRedirectURL        string
	OIDCAutoProvision      bool
	LDAPURL                string
	LDAPStartTLS           bool
	LDAPBindDN             string
	LDAPBindPassword       string
	LDAPBaseDN             string
	LDAPUserFilter         string
	LDAPAutoProvision      bool
}

func Load() (Config, error) {
	var c Config
	c.ListenAddr = value("LISTEN_ADDR", "127.0.0.1:8080")
	c.PublicURL = value("PUBLIC_URL", "")
	c.LogLevel = strings.ToLower(value("LOG_LEVEL", "info"))
	c.DatabaseURL = secret("DATABASE_URL", "")
	c.AutoMigrate = boolean("AUTO_MIGRATE", false)
	c.HFBaseURL = value("HF_BASE_URL", "https://huggingface.co")
	c.HFToken = secret("HF_TOKEN", "")
	c.HFAllowedHosts = split(value("HF_ALLOWED_REDIRECT_HOSTS", "huggingface.co,hf.co,*.huggingface.co,*.hf.co,*.xethub.hf.co"))
	c.HFTimeout = duration("HF_TIMEOUT", 2*time.Minute)
	c.S3Endpoint = value("S3_ENDPOINT", "")
	c.S3Region = value("S3_REGION", "us-east-1")
	c.S3Bucket = value("S3_BUCKET", "")
	c.S3Prefix = strings.Trim(value("S3_PREFIX", "hfproxy"), "/")
	c.S3ForcePathStyle = boolean("S3_FORCE_PATH_STYLE", true)
	c.S3AccessKey = secret("S3_ACCESS_KEY", "")
	c.S3SecretKey = secret("S3_SECRET_KEY", "")
	c.S3ServerSideEncryption = boolean("S3_SERVER_SIDE_ENCRYPTION", true)
	c.AuthMode = strings.ToLower(value("AUTH_MODE", "local"))
	c.AnonymousPublicRead = boolean("ANONYMOUS_PUBLIC_READ", false)
	c.SessionSecret = secret("SESSION_SECRET", "")
	c.SessionIdleTTL = duration("SESSION_IDLE_TTL", 8*time.Hour)
	c.SessionAbsoluteTTL = duration("SESSION_ABSOLUTE_TTL", 24*time.Hour)
	c.CacheMaxFileSize = int64(number("CACHE_MAX_FILE_SIZE", 100<<30))
	c.CacheRefTTL = duration("CACHE_REF_TTL", time.Minute)
	c.CacheNegativeTTL = duration("CACHE_NEGATIVE_TTL", 30*time.Second)
	c.CacheGCGrace = duration("CACHE_GC_GRACE", 24*time.Hour)
	c.OIDCIssuer = value("OIDC_ISSUER", "")
	c.OIDCClientID = value("OIDC_CLIENT_ID", "")
	c.OIDCClientSecret = secret("OIDC_CLIENT_SECRET", "")
	c.OIDCRedirectURL = value("OIDC_REDIRECT_URL", "")
	c.OIDCAutoProvision = boolean("OIDC_AUTO_PROVISION", false)
	c.LDAPURL = value("LDAP_URL", "")
	c.LDAPStartTLS = boolean("LDAP_STARTTLS", true)
	c.LDAPBindDN = value("LDAP_BIND_DN", "")
	c.LDAPBindPassword = secret("LDAP_BIND_PASSWORD", "")
	c.LDAPBaseDN = value("LDAP_BASE_DN", "")
	c.LDAPUserFilter = value("LDAP_USER_FILTER", "(uid={username})")
	c.LDAPAutoProvision = boolean("LDAP_AUTO_PROVISION", false)
	if err := c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

func (c Config) Validate() error {
	if c.LogLevel != "debug" && c.LogLevel != "info" && c.LogLevel != "warn" && c.LogLevel != "error" {
		return fmt.Errorf("HFPROXY_LOG_LEVEL must be debug, info, warn, or error")
	}
	if c.PublicURL != "" {
		public, publicErr := url.Parse(c.PublicURL)
		if publicErr != nil || public.Host == "" || public.User != nil || public.RawQuery != "" || public.Fragment != "" || (public.Scheme != "https" && public.Scheme != "http") {
			return fmt.Errorf("HFPROXY_PUBLIC_URL must be an HTTP(S) URL without credentials, query, or fragment")
		}
		if public.Scheme != "https" && !isLoopbackHost(public.Hostname()) {
			return fmt.Errorf("HFPROXY_PUBLIC_URL must use HTTPS outside loopback development")
		}
	}
	base, err := url.Parse(c.HFBaseURL)
	if err != nil || base.Host == "" || (base.Scheme != "https" && base.Scheme != "http") || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return fmt.Errorf("HFPROXY_HF_BASE_URL must be an HTTP(S) URL without user information")
	}
	if base.Scheme != "https" && !isLoopbackHost(base.Hostname()) {
		return fmt.Errorf("HFPROXY_HF_BASE_URL must use HTTPS outside loopback development")
	}
	if c.HFTimeout <= 0 || c.CacheMaxFileSize <= 0 {
		return fmt.Errorf("HFPROXY_HF_TIMEOUT and HFPROXY_CACHE_MAX_FILE_SIZE must be positive")
	}
	if c.CacheRefTTL <= 0 || c.CacheNegativeTTL < 0 || c.CacheGCGrace < 0 {
		return fmt.Errorf("cache TTL and garbage collection grace values are invalid")
	}
	if c.AuthMode != "local" && c.AuthMode != "oidc" {
		return fmt.Errorf("HFPROXY_AUTH_MODE must be local or oidc")
	}
	if c.AuthMode == "oidc" || c.OIDCIssuer != "" {
		if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" || c.OIDCRedirectURL == "" {
			return fmt.Errorf("OIDC requires HFPROXY_OIDC_ISSUER, HFPROXY_OIDC_CLIENT_ID, HFPROXY_OIDC_CLIENT_SECRET, and HFPROXY_OIDC_REDIRECT_URL")
		}
		redirect, parseErr := url.Parse(c.OIDCRedirectURL)
		if parseErr != nil || redirect.Host == "" || (redirect.Scheme != "https" && !isLoopbackHost(redirect.Hostname())) {
			return fmt.Errorf("HFPROXY_OIDC_REDIRECT_URL must use HTTPS outside loopback development")
		}
	}
	if c.SessionSecret == "" {
		return fmt.Errorf("HFPROXY_SESSION_SECRET is required; use a mounted secret or *_FILE")
	}
	if c.LDAPURL != "" {
		ldapURL, ldapErr := url.Parse(c.LDAPURL)
		if ldapErr != nil || ldapURL.Host == "" || ldapURL.User != nil || (ldapURL.Scheme != "ldaps" && ldapURL.Scheme != "ldap") || c.LDAPBaseDN == "" || !strings.Contains(c.LDAPUserFilter, "{username}") {
			return fmt.Errorf("LDAP requires a valid HFPROXY_LDAP_URL, HFPROXY_LDAP_BASE_DN, and a HFPROXY_LDAP_USER_FILTER containing {username}")
		}
		if ldapURL.Scheme == "ldap" && !c.LDAPStartTLS {
			return fmt.Errorf("plain LDAP is disabled; use ldaps:// or enable StartTLS")
		}
		if c.LDAPBindDN != "" && c.LDAPBindPassword == "" {
			return fmt.Errorf("HFPROXY_LDAP_BIND_PASSWORD is required when a bind DN is configured")
		}
	}
	if c.SessionSecret != "" && len(c.SessionSecret) < 32 {
		return fmt.Errorf("HFPROXY_SESSION_SECRET must contain at least 32 bytes")
	}
	if c.DatabaseURL == "" {
		return fmt.Errorf("HFPROXY_DATABASE_URL is required")
	}
	if c.S3Endpoint != "" {
		endpoint, endpointErr := url.Parse(c.S3Endpoint)
		if endpointErr != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" {
			return fmt.Errorf("HFPROXY_S3_ENDPOINT must be an HTTP(S) URL without credentials or query parameters")
		}
	}
	if (c.S3AccessKey == "") != (c.S3SecretKey == "") {
		return fmt.Errorf("HFPROXY_S3_ACCESS_KEY and HFPROXY_S3_SECRET_KEY must be set together")
	}
	if c.SessionIdleTTL <= 0 || c.SessionAbsoluteTTL < c.SessionIdleTTL {
		return fmt.Errorf("session TTLs must be positive and the absolute TTL must be at least the idle TTL")
	}
	return nil
}

func value(key, fallback string) string {
	name := "HFPROXY_" + key
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func secret(key, fallback string) string {
	name := "HFPROXY_" + key
	if file := os.Getenv(name + "_FILE"); file != "" {
		data, err := os.ReadFile(file)
		if err != nil {
			return ""
		}
		return strings.TrimRight(string(data), "\r\n")
	}
	if v, ok := os.LookupEnv(name); ok && v != "" {
		return v
	}
	return fallback
}

func boolean(key string, fallback bool) bool {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := strconv.ParseBool(text)
	if err != nil {
		return fallback
	}
	return v
}

func number(key string, fallback int) int {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := strconv.Atoi(text)
	if err != nil {
		return fallback
	}
	return v
}

func duration(key string, fallback time.Duration) time.Duration {
	name := "HFPROXY_" + key
	text, ok := os.LookupEnv(name)
	if !ok || text == "" {
		return fallback
	}
	v, err := time.ParseDuration(text)
	if err != nil {
		return fallback
	}
	return v
}

func split(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if host := strings.TrimSpace(strings.ToLower(part)); host != "" {
			result = append(result, host)
		}
	}
	return result
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || strings.HasPrefix(host, "127.") || host == "::1"
}
