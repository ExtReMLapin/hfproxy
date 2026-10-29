package config

import (
	"testing"
	"time"
)

func TestDescribeSourcesMarksOverridesAndHidesSecrets(t *testing.T) {
	environment := Config{S3Bucket: "env-bucket", S3SecretKey: "topsecret", DatabaseURL: "postgres://user:pw@db/app", LogLevel: "info"}
	effective := environment
	effective.S3Bucket = "console-bucket"
	got := map[string]SettingSource{}
	for _, entry := range DescribeSources(environment, effective) {
		got[entry.Name] = entry
	}
	if got["S3Bucket"].Source != "console" || got["S3Bucket"].Value != "console-bucket" {
		t.Fatalf("S3Bucket = %+v", got["S3Bucket"])
	}
	if got["LogLevel"].Source != "environment" {
		t.Fatalf("LogLevel = %+v", got["LogLevel"])
	}
	if got["S3SecretKey"].Value != "(set, hidden)" {
		t.Fatalf("secret leaked: %+v", got["S3SecretKey"])
	}
	if v := got["DatabaseURL"].Value; v != "postgres://%2A%2A%2A@db/app" {
		t.Fatalf("database URL not redacted: %q", v)
	}
}

func TestCheckDeploymentRejectsDevCredentials(t *testing.T) {
	base := Config{ListenAddr: "0.0.0.0:8080", PublicURL: "https://hub.example.com", SessionSecret: "unique-secret-unique-secret-unique-1"}
	if err := base.CheckDeployment(); err != nil {
		t.Fatalf("clean config rejected: %v", err)
	}
	withDB := base
	withDB.DatabaseURL = "postgres://hfproxy:hfproxy_dev_password@db/hfproxy"
	withS3 := base
	withS3.S3AccessKey = "GK0123456789abcdef0123456789abcdef"
	for name, cfg := range map[string]Config{"database": withDB, "s3": withS3} {
		if err := cfg.CheckDeployment(); err == nil {
			t.Errorf("%s: development credentials accepted on a public deployment", name)
		}
		cfg.PublicURL = "http://localhost:8080"
		cfg.ListenAddr = "127.0.0.1:8080"
		if err := cfg.CheckDeployment(); err != nil {
			t.Errorf("%s: rejected on loopback: %v", name, err)
		}
	}
}

func TestLockedSettingsPinToEnvironmentAndRejectChanges(t *testing.T) {
	if err := ValidateLockedSettings([]string{"s3_bucket", "all"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLockedSettings([]string{"s3_buckett"}); err == nil {
		t.Fatal("a misspelled setting name must be rejected")
	}
	environment := Config{S3Bucket: "env-bucket", S3Region: "env-region", LockedSettings: []string{"s3_bucket"}}
	saved := managedFromConfig(Config{S3Bucket: "console-bucket", S3Region: "console-region"})
	saved.pinToEnvironment(environment)
	effective := saved.apply(environment)
	if effective.S3Bucket != "env-bucket" {
		t.Fatalf("locked bucket = %q, want the environment value", effective.S3Bucket)
	}
	if effective.S3Region != "console-region" {
		t.Fatalf("unlocked region = %q, want the console value", effective.S3Region)
	}
	changed := environment
	changed.S3Bucket, changed.S3Region = "other", "other"
	if got := ChangedLocked(environment.LockedSettings, environment, changed); len(got) != 1 || got[0] != "s3_bucket" {
		t.Fatalf("ChangedLocked = %v, want [s3_bucket]", got)
	}
	if got := LockedNames([]string{"all"}); len(got) != len(ManagedSettingNames()) {
		t.Fatalf("all expands to %d names, want %d", len(got), len(ManagedSettingNames()))
	}
}

func TestLDAPGroupSyncNeedsABaseAndAFilterWithDN(t *testing.T) {
	cfg := Config{LDAPURL: "ldaps://directory.example", LDAPBaseDN: "dc=x", LDAPUserFilter: "(uid={username})", LDAPGroupSync: true, LDAPGroupBaseDN: "ou=groups,dc=x", LDAPGroupFilter: "(member={dn})"}
	if err := validateLDAPOnly(cfg); err != nil {
		t.Fatalf("valid group sync rejected: %v", err)
	}
	cfg.LDAPGroupFilter = "(member=nobody)"
	if err := validateLDAPOnly(cfg); err == nil {
		t.Fatal("a group filter without {dn} must be rejected")
	}
}

// validBaseConfig is a configuration that passes Validate.
func validBaseConfig() Config {
	return Config{
		LogLevel: "info", HFBaseURL: "https://huggingface.co", HFTimeout: time.Second, CacheMaxFileSize: 1, CacheRefTTL: time.Second,
		AuthMode: "local", SessionSecret: "0123456789abcdef0123456789abcdef", SessionIdleTTL: time.Hour, SessionAbsoluteTTL: time.Hour, MetricsPath: "/metrics",
		TracingServiceName: "hfproxy", TracingSamplingRatio: 0.1, DatabaseURL: "postgres://u:p@db/app", S3Bucket: "b", S3Region: "r", ListenAddr: "127.0.0.1:8080",
	}
}

// validateLDAPOnly runs Validate on a configuration that is otherwise valid.
func validateLDAPOnly(ldap Config) error {
	base := validBaseConfig()
	base.LDAPURL, base.LDAPStartTLS, base.LDAPBaseDN, base.LDAPUserFilter = ldap.LDAPURL, ldap.LDAPStartTLS, ldap.LDAPBaseDN, ldap.LDAPUserFilter
	base.LDAPGroupSync, base.LDAPGroupBaseDN, base.LDAPGroupFilter = ldap.LDAPGroupSync, ldap.LDAPGroupBaseDN, ldap.LDAPGroupFilter
	return base.Validate()
}

func TestPoolSizeStaysAboveTheFillConcurrency(t *testing.T) {
	if got := (Config{CacheFillConcurrency: 4}).PoolSize(); got != 24 {
		t.Fatalf("default pool for 4 fills = %d, want 24", got)
	}
	if got := (Config{CacheFillConcurrency: 64}).PoolSize(); got < 64+8 {
		t.Fatalf("pool for 64 fills = %d: every fill holds a connection", got)
	}
	if got := (Config{DatabaseMaxConns: 50}).PoolSize(); got != 50 {
		t.Fatalf("explicit pool = %d, want 50", got)
	}
	base := validBaseConfig()
	base.DatabaseMaxConns, base.CacheFillConcurrency = 10, 8
	if err := base.Validate(); err == nil {
		t.Fatal("a pool smaller than the fill concurrency plus headroom must be rejected")
	}
}

func TestLogLevelNamesAndSavedLevel(t *testing.T) {
	for _, name := range []string{"debug", "info", "warn", "warning", "error"} {
		cfg := validBaseConfig()
		cfg.LogLevel = name
		if err := cfg.Validate(); err != nil {
			t.Errorf("level %q rejected: %v", name, err)
		}
	}
	cfg := validBaseConfig()
	cfg.LogLevel = "verbose"
	if err := cfg.Validate(); err == nil {
		t.Fatal("an unknown level was accepted")
	}

	t.Setenv("HFPROXY_LOG_LEVEL", "Warning")
	t.Setenv("HFPROXY_SESSION_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("HFPROXY_DATABASE_URL", "postgres://u:p@db/app")
	loaded, err := Load()
	if err != nil || loaded.LogLevel != "warn" {
		t.Fatalf("HFPROXY_LOG_LEVEL=Warning loaded as %q (%v), want warn", loaded.LogLevel, err)
	}

	// The level saved in the console wins over the environment; settings saved
	// before the level was editable leave it as it is.
	environment := validBaseConfig()
	saved := managedFromConfig(environment)
	saved.LogLevel = "debug"
	if got := saved.apply(environment).LogLevel; got != "debug" {
		t.Fatalf("saved level = %q, want debug", got)
	}
	saved.LogLevel = ""
	if got := saved.apply(environment).LogLevel; got != "info" {
		t.Fatalf("settings saved without a level changed it to %q", got)
	}
	locked := managedFromConfig(environment)
	locked.LogLevel = "debug"
	environment.LockedSettings = []string{"log_level"}
	locked.pinToEnvironment(environment)
	if got := locked.apply(environment).LogLevel; got != "info" {
		t.Fatalf("a locked level followed the saved value: %q", got)
	}
}
