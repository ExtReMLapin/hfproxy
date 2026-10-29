package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

func isolatedDatabase(t *testing.T) *sql.DB {
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
	name := "db_upgrade_" + strings.ReplaceAll(NewID(), "-", "")[:20]
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`) })
	dsn, err := url.Parse(baseDSN)
	if err != nil {
		t.Fatal(err)
	}
	dsn.Path = "/" + name
	sqlDB, err := sql.Open("pgx", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// TestUpgradeFromEveryPreviousVersion rebuilds the database as each earlier
// release left it, fills it with data, and checks that the current migrations
// keep that data and finish at the latest version.
func TestUpgradeFromEveryPreviousVersion(t *testing.T) {
	for from := int64(1); from < LatestMigration; from++ {
		t.Run(fmt.Sprintf("from version %d", from), func(t *testing.T) {
			sqlDB := isolatedDatabase(t)
			ctx := context.Background()
			if err := migrateUpTo(ctx, sqlDB, from); err != nil {
				t.Fatalf("build version %d: %v", from, err)
			}
			if version, err := MigrationStatus(ctx, sqlDB); err != nil || version != from {
				t.Fatalf("starting version = %d (%v), want %d", version, err, from)
			}

			// Rows written with the columns every release has had since version 1.
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := sqlDB.ExecContext(ctx, query, args...); err != nil {
					t.Fatalf("seed at version %d: %v\n%s", from, err, query)
				}
			}
			exec(`INSERT INTO users(id,username,role,password_hash) VALUES('u1','alice','admin','hash')`)
			exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES('r1','models','org/model','public')`)
			exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES('b1','sha-1',42,'blobs/sha-1')`)
			exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id) VALUES('a1','r1','c1','config.json','b1')`)
			exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix) VALUES('t1','u1','\x01','hfpx')`)
			exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES('\x02','u1','\x03',now()+interval '1 hour')`)
			exec(`INSERT INTO downloads(id,repository_id,path,status) VALUES('d1','r1','config.json','hit')`)
			exec(`INSERT INTO audit_events(id,action) VALUES('e1','test')`)
			if from < 14 {
				exec(`INSERT INTO repo_grants(repository_id,subject_type,subject_id,permission) VALUES('r1','user','u1','admin')`)
			} else {
				exec(`INSERT INTO repo_grants(repository_id,subject_type,subject_id,permission) VALUES('r1','user','u1','read')`)
			}
			if from >= 10 {
				exec(`INSERT INTO cache_pins(repository_id,commit_sha) VALUES('r1','')`)
			}

			if err := Migrate(ctx, sqlDB); err != nil {
				t.Fatalf("upgrade from version %d: %v", from, err)
			}
			if version, err := MigrationStatus(ctx, sqlDB); err != nil || version != LatestMigration {
				t.Fatalf("version after upgrade = %d (%v), want %d", version, err, LatestMigration)
			}
			if err := Migrate(ctx, sqlDB); err != nil {
				t.Fatalf("running the migrations again must change nothing: %v", err)
			}

			count := func(query string) int {
				t.Helper()
				var n int
				if err := sqlDB.QueryRowContext(ctx, query).Scan(&n); err != nil {
					t.Fatalf("%v\n%s", err, query)
				}
				return n
			}
			for _, table := range []string{"users", "repositories", "blobs", "artifacts", "access_tokens", "sessions", "downloads", "audit_events"} {
				if n := count(`SELECT COUNT(*) FROM ` + table); n != 1 {
					t.Errorf("%s holds %d rows after the upgrade, want the 1 seeded", table, n)
				}
			}
			// The totals of the console start from what the downloads table held,
			// and a grant that promised more than reading now says what it gives.
			// (From version 15 on the counters exist already and rows inserted by hand
			// are not counted; only older databases are filled by the migration.)
			if n := count(`SELECT COALESCE(SUM(hits),0) FROM repository_download_counts WHERE repository_id='r1'`); from < 15 && n != 1 {
				t.Errorf("the hit counter holds %d after the upgrade, want the 1 seeded download", n)
			}
			if n := count(`SELECT COUNT(*) FROM repo_grants WHERE permission='read'`); n != 1 {
				t.Error("a grant of a higher permission must become a read grant")
			}
			// Columns added later get their defaults on old rows.
			if n := count(`SELECT COUNT(*) FROM access_tokens WHERE key_id=''`); n != 1 {
				t.Error("an old token must have the empty key_id of the original scheme")
			}
			if n := count(`SELECT COUNT(*) FROM sessions WHERE key_id=''`); n != 1 {
				t.Error("an old session must have the empty key_id of the original scheme")
			}
			if n := count(`SELECT COUNT(*) FROM downloads WHERE user_id IS NULL AND client_ip IS NULL`); n != 1 {
				t.Error("an old download row must have no caller recorded")
			}
			if from >= 10 {
				if n := count(`SELECT COUNT(*) FROM cache_pins WHERE commit_sha='' AND ref=''`); n != 1 {
					t.Error("a repository-wide pin must survive the change of key in migration 11")
				}
			}
			// Tables added after the starting version exist and work.
			exec(`INSERT INTO login_attempts(key,window_start,attempts) VALUES('k',now(),1)`)
			exec(`INSERT INTO cache_pins(repository_id,commit_sha,ref) VALUES('r1','','main')`)
		})
	}
}
