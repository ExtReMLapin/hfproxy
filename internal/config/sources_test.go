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
	cfg := Config{DatabaseMaxConns: 10, CacheFillConcurrency: 8}
	base := Config{
		LogLevel: "info", HFBaseURL: "https://huggingface.co", HFTimeout: time.Second, CacheMaxFileSize: 1, CacheRefTTL: time.Second,
		AuthMode: "local", SessionSecret: "0123456789abcdef0123456789abcdef", SessionIdleTTL: time.Hour, SessionAbsoluteTTL: time.Hour, MetricsPath: "/metrics",
		TracingServiceName: "hfproxy", TracingSamplingRatio: 0.1, DatabaseURL: "postgres://u:p@db/app", S3Bucket: "b", S3Region: "r", ListenAddr: "127.0.0.1:8080",
	}
	base.DatabaseMaxConns, base.CacheFillConcurrency = cfg.DatabaseMaxConns, cfg.CacheFillConcurrency
	if err := base.Validate(); err == nil {
		t.Fatal("a pool smaller than the fill concurrency plus headroom must be rejected")
	}
}
