package db

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

type Store struct{ SQL *sql.DB }

type migration struct {
	version int64
	script  string
}

// migrations is the ordered schema history. Each entry must stay compatible
// with the previous release of the code (add columns and tables first, drop or
// rename only in a later release) so an image rollback after a migration keeps
// working.
var migrations = []migration{
	{1, schemaV1}, {2, schemaV2}, {3, schemaV3}, {4, schemaV4}, {5, schemaV5}, {6, schemaV6}, {7, schemaV7},
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
		if version < m.version {
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

const schemaV1 = `
CREATE TABLE IF NOT EXISTS users (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL UNIQUE,
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',
    role TEXT NOT NULL DEFAULT 'reader' CHECK (role IN ('admin','operator','reader')),
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','disabled')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS external_identities (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider TEXT NOT NULL,
    issuer TEXT NOT NULL,
    subject TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(issuer, subject)
);
CREATE TABLE IF NOT EXISTS groups (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL UNIQUE,
    source TEXT NOT NULL DEFAULT 'local',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS group_members (
    group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    PRIMARY KEY(group_id, user_id)
);
CREATE TABLE IF NOT EXISTS access_tokens (
    id TEXT PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash BYTEA NOT NULL UNIQUE,
    prefix TEXT NOT NULL,
    name TEXT NOT NULL DEFAULT '',
    scopes TEXT[] NOT NULL DEFAULT ARRAY['read']::TEXT[],
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS repositories (
    id TEXT PRIMARY KEY,
    repo_type TEXT NOT NULL CHECK (repo_type IN ('models','datasets','spaces')),
    repo_id TEXT NOT NULL,
    access_policy TEXT NOT NULL DEFAULT 'admin_only' CHECK (access_policy IN ('public','allowlist','admin_only')),
    deleted_at TIMESTAMPTZ,
    created_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(repo_type, repo_id)
);
CREATE TABLE IF NOT EXISTS repo_grants (
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user','group')),
    subject_id TEXT NOT NULL,
    permission TEXT NOT NULL DEFAULT 'read' CHECK (permission IN ('read','write','admin')),
    PRIMARY KEY(repository_id, subject_type, subject_id)
);
CREATE TABLE IF NOT EXISTS revisions (
    id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    requested_ref TEXT NOT NULL,
    commit_sha TEXT NOT NULL,
    resolved_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(repository_id, requested_ref)
);
CREATE TABLE IF NOT EXISTS blobs (
    id TEXT PRIMARY KEY,
    sha256 TEXT NOT NULL UNIQUE,
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    storage_key TEXT NOT NULL UNIQUE,
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at TIMESTAMPTZ
);
CREATE TABLE IF NOT EXISTS artifacts (
    id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    path TEXT NOT NULL,
    blob_id TEXT NOT NULL REFERENCES blobs(id),
    etag TEXT NOT NULL DEFAULT '',
    last_modified TIMESTAMPTZ,
    upstream_size BIGINT NOT NULL DEFAULT 0,
    cached_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_accessed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(repository_id, commit_sha, path)
);
CREATE INDEX IF NOT EXISTS artifacts_repository_idx ON artifacts(repository_id, commit_sha);
CREATE INDEX IF NOT EXISTS artifacts_last_access_idx ON artifacts(last_accessed_at);
CREATE TABLE IF NOT EXISTS downloads (
    id TEXT PRIMARY KEY,
    repository_id TEXT REFERENCES repositories(id) ON DELETE SET NULL,
    commit_sha TEXT NOT NULL DEFAULT '',
    path TEXT NOT NULL,
    status TEXT NOT NULL,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    bytes BIGINT NOT NULL DEFAULT 0,
    error_class TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS audit_events (
    id TEXT PRIMARY KEY,
    actor_user_id TEXT REFERENCES users(id) ON DELETE SET NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id TEXT NOT NULL DEFAULT '',
    metadata_json JSONB NOT NULL DEFAULT '{}'::jsonb,
    request_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_created_idx ON audit_events(created_at DESC);
CREATE TABLE IF NOT EXISTS sessions (
    token_hash BYTEA PRIMARY KEY,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_hash BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expires_at);
`

const schemaV2 = `
CREATE TABLE IF NOT EXISTS app_settings (
    setting_key TEXT PRIMARY KEY,
    ciphertext BYTEA NOT NULL,
    updated_by TEXT REFERENCES users(id) ON DELETE SET NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

const schemaV3 = `
CREATE TABLE IF NOT EXISTS download_resumptions (
    id TEXT PRIMARY KEY,
    repository_id TEXT NOT NULL REFERENCES repositories(id) ON DELETE CASCADE,
    commit_sha TEXT NOT NULL,
    path TEXT NOT NULL,
    upstream_etag TEXT NOT NULL DEFAULT '',
    total_size BIGINT NOT NULL CHECK (total_size >= 0),
    content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    chunk_size BIGINT NOT NULL CHECK (chunk_size > 0),
    completed_bytes BIGINT NOT NULL DEFAULT 0 CHECK (completed_bytes >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(repository_id, commit_sha, path)
);
CREATE INDEX IF NOT EXISTS download_resumptions_updated_idx ON download_resumptions(updated_at);
`

const schemaV4 = `
ALTER TABLE blobs ADD COLUMN IF NOT EXISTS orphaned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS blobs_orphaned_idx ON blobs(orphaned_at) WHERE orphaned_at IS NOT NULL;
`

const schemaV5 = `
DELETE FROM repo_grants g USING repositories r WHERE g.repository_id=r.id AND r.deleted_at IS NOT NULL;
`

const schemaV6 = `
CREATE INDEX IF NOT EXISTS artifacts_blob_idx ON artifacts(blob_id);
CREATE INDEX IF NOT EXISTS downloads_started_idx ON downloads(started_at);
CREATE INDEX IF NOT EXISTS access_tokens_user_idx ON access_tokens(user_id);
CREATE INDEX IF NOT EXISTS sessions_user_idx ON sessions(user_id);
CREATE INDEX IF NOT EXISTS group_members_user_idx ON group_members(user_id);
`

const schemaV7 = `
ALTER TABLE downloads ADD COLUMN IF NOT EXISTS user_id TEXT;
ALTER TABLE downloads ADD COLUMN IF NOT EXISTS token_id TEXT;
ALTER TABLE downloads ADD COLUMN IF NOT EXISTS client_ip TEXT;
`
