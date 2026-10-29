package main

import (
	"bytes"
	"context"
	"database/sql"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/keyring"
)

func rotationDatabase(t *testing.T) *sql.DB {
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
	name := "rotate_cli_" + strings.ReplaceAll(db.NewID(), "-", "")[:20]
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

func TestRotateSecretReportsWhatStillUsesThePreviousSecret(t *testing.T) {
	const oldSecret = "cli-old-secret-0123456789abcdef0123456789ab"
	const newSecret = "cli-new-secret-0123456789abcdef0123456789ab"
	sqlDB := rotationDatabase(t)
	ctx := context.Background()
	if _, err := config.SavePersisted(ctx, sqlDB, config.Config{SessionSecret: oldSecret, HFBaseURL: "https://huggingface.co", S3Bucket: "bucket"}, ""); err != nil {
		t.Fatal(err)
	}
	userID := db.NewID()
	if _, err := sqlDB.Exec(`INSERT INTO users(id,username,role) VALUES($1,'rotator','reader')`, userID); err != nil {
		t.Fatal(err)
	}
	current := keyring.NewRing(newSecret)
	insertToken := func(id string, hash []byte, keyID string) {
		t.Helper()
		if _, err := sqlDB.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,key_id) VALUES($1,$2,$3,'p',$4)`, id, userID, hash, keyID); err != nil {
			t.Fatal(err)
		}
	}
	insertToken("fresh", current.Hash("fresh-token"), current.KeyID())
	insertToken("stale", keyring.LegacyHash("stale-token", oldSecret), "")
	if _, err := sqlDB.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,key_id,revoked_at) VALUES('revoked',$1,$2,'p','',now())`, userID, keyring.LegacyHash("revoked-token", oldSecret)); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.Exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at,key_id) VALUES($1,$2,$1,now()+interval '1 hour','')`, keyring.LegacyHash("old-session", oldSecret), userID); err != nil {
		t.Fatal(err)
	}

	rotated := config.Config{SessionSecret: newSecret, PreviousSessionSecrets: []string{oldSecret}}
	var out bytes.Buffer
	if err := rotateSecret(ctx, &out, rotated, sqlDB); err != nil {
		t.Fatal(err)
	}
	report := out.String()
	for _, want := range []string{"re-encrypted", "older secret or scheme: 1\n", "Keep HFPROXY_SESSION_SECRET_PREVIOUS"} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q:\n%s", want, report)
		}
	}
	if strings.Count(report, "older secret or scheme: 1") != 2 {
		t.Errorf("expected one stale token and one stale session:\n%s", report)
	}
	if _, err := config.LoadPersisted(ctx, sqlDB, config.Config{SessionSecret: newSecret}); err != nil {
		t.Errorf("settings are not readable with the new secret alone after rotation: %v", err)
	}

	out.Reset()
	if err := rotateSecret(ctx, &out, rotated, sqlDB); err != nil || !strings.Contains(out.String(), "already protected") {
		t.Errorf("second run: %v\n%s", err, out.String())
	}

	out.Reset()
	if err := rotateSecret(ctx, &out, config.Config{SessionSecret: newSecret}, sqlDB); err != nil || !strings.Contains(out.String(), "can no longer be verified") {
		t.Errorf("without the previous secret: %v\n%s", err, out.String())
	}

	if _, err := sqlDB.Exec(`DELETE FROM access_tokens WHERE id='stale'; DELETE FROM sessions`); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := rotateSecret(ctx, &out, rotated, sqlDB); err != nil || !strings.Contains(out.String(), "remove HFPROXY_SESSION_SECRET_PREVIOUS") {
		t.Errorf("when nothing depends on the old secret: %v\n%s", err, out.String())
	}
}
