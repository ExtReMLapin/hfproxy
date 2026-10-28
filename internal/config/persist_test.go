package config

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/hfproxy/hfproxy/internal/db"
)

const (
	persistOld = "persist-old-secret-0123456789abcdef0123456789"
	persistNew = "persist-new-secret-0123456789abcdef0123456789"
)

func persistDatabase(t *testing.T) *sql.DB {
	t.Helper()
	baseDSN := os.Getenv("HFPROXY_TEST_DATABASE_URL")
	if baseDSN == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL is not set")
	}
	admin, err := sql.Open("pgx", baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	name := "cfg_persist_" + strings.ReplaceAll(db.NewID(), "-", "")[:20]
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`) })
	dsn, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/" + name
	store, err := db.Open(context.Background(), dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.SQL.Close() })
	if err := db.Migrate(context.Background(), store.SQL); err != nil {
		t.Fatal(err)
	}
	return store.SQL
}

func persistBase(current string, previous ...string) Config {
	return Config{SessionSecret: current, PreviousSessionSecrets: previous, HFBaseURL: "https://huggingface.co", S3Bucket: "bucket", HFToken: "hf_secret_value"}
}

func TestSavedSettingsFollowASessionSecretRotation(t *testing.T) {
	sqlDB := persistDatabase(t)
	ctx := context.Background()
	saved := persistBase(persistOld)
	saved.S3Bucket = "console-bucket"
	if err := SavePersisted(ctx, sqlDB, saved, ""); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := sqlDB.QueryRow(`SELECT ciphertext FROM app_settings WHERE setting_key='runtime'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "hf_secret_value") || !strings.HasPrefix(string(raw), "HFS2") {
		t.Fatal("saved settings must be sealed in the keyed format and never readable")
	}

	if _, err := LoadPersisted(ctx, sqlDB, persistBase(persistNew)); err == nil || !strings.Contains(err.Error(), "SESSION_SECRET_PREVIOUS") {
		t.Fatalf("loading under a new secret alone = %v, want an error naming the previous-secret setting", err)
	}
	rotated := persistBase(persistNew, persistOld)
	loaded, err := LoadPersisted(ctx, sqlDB, rotated)
	if err != nil || loaded.S3Bucket != "console-bucket" || loaded.HFToken != "hf_secret_value" {
		t.Fatalf("loading during rotation: bucket=%q token ok=%v err=%v", loaded.S3Bucket, loaded.HFToken == "hf_secret_value", err)
	}

	changed, err := ReencryptPersisted(ctx, sqlDB, rotated)
	if err != nil || !changed {
		t.Fatalf("first re-encryption = %v, %v; want it to change the stored value", changed, err)
	}
	if again, err := ReencryptPersisted(ctx, sqlDB, rotated); err != nil || again {
		t.Fatalf("second re-encryption = %v, %v; want a no-op", again, err)
	}
	loaded, err = LoadPersisted(ctx, sqlDB, persistBase(persistNew))
	if err != nil || loaded.S3Bucket != "console-bucket" {
		t.Fatalf("after re-encryption the new secret alone must work: %v", err)
	}
}

func TestSettingsSavedByEarlierVersionsAreMigrated(t *testing.T) {
	sqlDB := persistDatabase(t)
	ctx := context.Background()
	legacyKey := sha256.Sum256([]byte("hfproxy settings encryption v1:" + persistOld))
	block, err := aes.NewCipher(legacyKey[:])
	if err != nil {
		t.Fatal(err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, gcm.NonceSize())
	_, _ = rand.Read(nonce)
	plaintext := []byte(`{"s3_bucket":"legacy-bucket","hf_base_url":"https://huggingface.co"}`)
	legacy := gcm.Seal(nonce, nonce, plaintext, []byte("hfproxy-runtime-settings-v1"))
	if _, err := sqlDB.Exec(`INSERT INTO app_settings(setting_key,ciphertext) VALUES('runtime',$1)`, legacy); err != nil {
		t.Fatal(err)
	}
	cfg := persistBase(persistOld)
	loaded, err := LoadPersisted(ctx, sqlDB, cfg)
	if err != nil || loaded.S3Bucket != "legacy-bucket" {
		t.Fatalf("settings from an earlier version did not load: bucket=%q err=%v", loaded.S3Bucket, err)
	}
	if changed, err := ReencryptPersisted(ctx, sqlDB, cfg); err != nil || !changed {
		t.Fatalf("re-encryption of the old format = %v, %v; want it upgraded", changed, err)
	}
	if again, _ := ReencryptPersisted(ctx, sqlDB, cfg); again {
		t.Fatal("upgraded settings were re-encrypted again")
	}
}

func TestReencryptWithNothingSavedIsANoOp(t *testing.T) {
	sqlDB := persistDatabase(t)
	if changed, err := ReencryptPersisted(context.Background(), sqlDB, persistBase(persistNew, persistOld)); err != nil || changed {
		t.Fatalf("no saved settings: changed=%v err=%v", changed, err)
	}
}

func TestPreviousSessionSecretsParsingAndValidation(t *testing.T) {
	if got := splitSecrets("a\nb, c\r\n\td"); strings.Join(got, "|") != "a|b|c|d" {
		t.Fatalf("splitSecrets = %v", got)
	}
	if len(splitSecrets("")) != 0 {
		t.Fatal("an empty value must give no secrets")
	}
	base := Config{
		LogLevel: "info", DatabaseURL: "postgres://x", HFBaseURL: "https://huggingface.co", HFTimeout: 1, CacheMaxFileSize: 1,
		CacheRefTTL: 1, AuthMode: "local", SessionSecret: strings.Repeat("s", 40), SessionIdleTTL: 1, SessionAbsoluteTTL: 1,
	}
	ok := base
	ok.PreviousSessionSecrets = []string{strings.Repeat("p", 40)}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid previous secret rejected: %v", err)
	}
	short := base
	short.PreviousSessionSecrets = []string{"too-short"}
	if err := short.Validate(); err == nil {
		t.Fatal("a short previous secret was accepted")
	}
	many := base
	many.PreviousSessionSecrets = []string{strings.Repeat("1", 40), strings.Repeat("2", 40), strings.Repeat("3", 40), strings.Repeat("4", 40), strings.Repeat("5", 40)}
	if err := many.Validate(); err == nil {
		t.Fatal("an unbounded list of previous secrets was accepted")
	}
}

func TestSavedSettingsWithoutTheCacheLimitKeepTheEnvironmentValue(t *testing.T) {
	base := Config{CacheMaxTotalSize: 500}
	if got := (managedSettings{}).apply(base).CacheMaxTotalSize; got != 500 {
		t.Fatalf("settings saved before the limit existed reset it to %d, want the environment's 500", got)
	}
	zero := int64(0)
	if got := (managedSettings{CacheMaxTotalSize: &zero}).apply(base).CacheMaxTotalSize; got != 0 {
		t.Fatalf("an explicit 0 (unlimited) from the console was ignored: %d", got)
	}
	saved := managedFromConfig(Config{CacheMaxTotalSize: 123})
	if saved.CacheMaxTotalSize == nil || *saved.CacheMaxTotalSize != 123 {
		t.Fatal("the limit is not saved with the other settings")
	}
	if got := saved.apply(Config{CacheMaxTotalSize: 9}).CacheMaxTotalSize; got != 123 {
		t.Fatalf("saved limit = %d, want 123", got)
	}
}
