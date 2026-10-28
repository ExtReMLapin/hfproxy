package config

import "testing"

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
