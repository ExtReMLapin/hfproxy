package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct{ SQL *sql.DB }

type migration struct {
	version int64
	script  string
}

//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrations is the ordered schema history, one embedded file per version
// (migrations/0001_name.sql, ...). Each must stay compatible with the previous
// release of the code (add columns and tables first, drop or rename only in a
// later release) so an image rollback after a migration keeps working; see
// migrations/README.md.
var migrations = mustLoadMigrations()

func mustLoadMigrations() []migration {
	entries, err := migrationFiles.ReadDir("migrations")
	if err != nil {
		panic("embedded migrations unreadable: " + err.Error())
	}
	var list []migration
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		prefix, _, ok := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !ok || err != nil {
			panic("migration file " + name + " must start with a version number and an underscore")
		}
		script, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			panic("migration " + name + " unreadable: " + err.Error())
		}
		list = append(list, migration{version: version, script: string(script)})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].version < list[j].version })
	return list
}

// LatestMigration is the schema version this build expects. /readyz reports
// not ready while the database is behind it.
var LatestMigration = migrations[len(migrations)-1].version

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	sqlDB, err := sql.Open("pgx", databaseURL)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	sqlDB.SetMaxOpenConns(20)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxLifetime(30 * time.Minute)
	if err := sqlDB.PingContext(ctx); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	return &Store{SQL: sqlDB}, nil
}

func Migrate(ctx context.Context, sqlDB *sql.DB) error {
	return migrateUpTo(ctx, sqlDB, LatestMigration)
}

// migrateUpTo applies every migration newer than the database's version, up to
// and including limit. Production code always goes to LatestMigration; tests
// stop earlier to reproduce a database left by an older release.
func migrateUpTo(ctx context.Context, sqlDB *sql.DB, limit int64) error {
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return fmt.Errorf("reserve migration connection: %w", err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("start migration transaction: %w", err)
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('hfproxy:schema-migrations'))`); err != nil {
		return fmt.Errorf("lock migrations: %w", err)
	}
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return fmt.Errorf("create migration table: %w", err)
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version); err != nil {
		return fmt.Errorf("read migration version: %w", err)
	}
	for _, m := range migrations {
		if version < m.version && m.version <= limit {
			if err := applyMigrationTx(ctx, tx, m.version, m.script); err != nil {
				return err
			}
			version = m.version
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migrations: %w", err)
	}
	return nil
}

func applyMigrationTx(ctx context.Context, tx *sql.Tx, version int64, schema string) error {
	for _, statement := range splitStatements(schema) {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("apply schema migration %d: %w", version, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES ($1) ON CONFLICT DO NOTHING`, version); err != nil {
		return fmt.Errorf("record migration %d: %w", version, err)
	}
	return nil
}

// splitStatements splits a migration script on semicolons that end a statement,
// ignoring those inside single-quoted strings, double-quoted identifiers,
// dollar-quoted bodies and comments.
func splitStatements(script string) []string {
	var statements []string
	var current strings.Builder
	flush := func() {
		if strings.TrimSpace(current.String()) != "" {
			statements = append(statements, current.String())
		}
		current.Reset()
	}
	for i := 0; i < len(script); {
		c := script[i]
		switch {
		case c == '-' && strings.HasPrefix(script[i:], "--"):
			end := strings.IndexByte(script[i:], '\n')
			if end < 0 {
				end = len(script) - i
			}
			current.WriteString(script[i : i+end])
			i += end
		case c == '/' && strings.HasPrefix(script[i:], "/*"):
			end := strings.Index(script[i+2:], "*/")
			if end < 0 {
				end = len(script) - i - 2
			} else {
				end += 2
			}
			current.WriteString(script[i : i+2+end])
			i += 2 + end
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(script) {
				if script[j] == c {
					if j+1 < len(script) && script[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			if j < len(script) {
				j++
			}
			current.WriteString(script[i:j])
			i = j
		case c == '$':
			tag := dollarTag(script[i:])
			if tag == "" {
				current.WriteByte(c)
				i++
				break
			}
			end := strings.Index(script[i+len(tag):], tag)
			if end < 0 {
				current.WriteString(script[i:])
				i = len(script)
				break
			}
			stop := i + len(tag) + end + len(tag)
			current.WriteString(script[i:stop])
			i = stop
		case c == ';':
			flush()
			i++
		default:
			current.WriteByte(c)
			i++
		}
	}
	flush()
	return statements
}

// dollarTag returns the opening "$tag$" of a dollar-quoted string at the start
// of s, or "" when s does not start one.
func dollarTag(s string) string {
	for j := 1; j < len(s); j++ {
		switch r := s[j]; {
		case r == '$':
			return s[:j+1]
		case r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || (j > 1 && r >= '0' && r <= '9'):
		default:
			return ""
		}
	}
	return ""
}

func MigrationStatus(ctx context.Context, sqlDB *sql.DB) (int64, error) {
	var exists bool
	if err := sqlDB.QueryRowContext(ctx, `SELECT to_regclass('public.schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, err
	}
	if !exists {
		return 0, nil
	}
	var version int64
	err := sqlDB.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&version)
	return version, err
}

func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	hexValue := hex.EncodeToString(b[:])
	return hexValue[:8] + "-" + hexValue[8:12] + "-" + hexValue[12:16] + "-" + hexValue[16:20] + "-" + hexValue[20:]
}

// PinnedArtifactSQL is a correlated subquery, true for an artifact aliased "a"
// that some pin covers: its repository is pinned, its commit is pinned, or a
// pinned branch or tag currently points to its commit.
const PinnedArtifactSQL = `SELECT 1 FROM cache_pins p WHERE p.repository_id=a.repository_id AND (
		(p.commit_sha='' AND p.ref='')
		OR (p.commit_sha<>'' AND p.commit_sha=a.commit_sha)
		OR (p.ref<>'' AND EXISTS (SELECT 1 FROM revisions v WHERE v.repository_id=p.repository_id AND v.requested_ref=p.ref AND v.commit_sha=a.commit_sha)))`
