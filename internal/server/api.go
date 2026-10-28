package server

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func (s *Server) apiV1(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	if isMutation(r.Method) && !hasBearer(r) && !s.checkCSRF(r) {
		writeProblem(w, r, http.StatusForbidden, "CSRF validation failed", "Reload the page and retry the action.")
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	if path == "me/tokens" || strings.HasPrefix(path, "me/tokens/") {
		s.apiPersonalTokens(w, r, strings.TrimPrefix(path, "me/tokens"))
		return
	}
	if isMutation(r.Method) && hasBearer(r) {
		p, err := s.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
			writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "A valid access token is required.")
			return
		}
		if p.TokenID == "" || (!hasScope(p, "write") && !hasScope(p, "admin")) {
			writeProblem(w, r, http.StatusForbidden, "Forbidden", "This token is read-only and cannot modify the administration API.")
			return
		}
	}
	// Match known paths before methods so callers receive 405 with the
	// endpoint's actual Allow header instead of an ambiguous 404.
	switch path {
	case "auth/providers", "me", "healthz", "stats", "downloads/active", "cache/entries", "audit-events", "settings/public":
		if r.Method != http.MethodGet {
			methodNotAllowedWith(w, "GET")
			return
		}
	case "repos", "users", "groups":
		if r.Method != http.MethodGet && r.Method != http.MethodPost {
			methodNotAllowedWith(w, "GET, POST")
			return
		}
	case "cache/purge":
		if r.Method != http.MethodPost {
			methodNotAllowedWith(w, "POST")
			return
		}
	case "settings":
		if r.Method != http.MethodGet && r.Method != http.MethodPut {
			methodNotAllowedWith(w, "GET, PUT")
			return
		}
	case "settings/test-s3", "settings/test-hub":
		if r.Method != http.MethodPost {
			methodNotAllowedWith(w, "POST")
			return
		}
	case "users/", "groups/":
		// Handled below; this case documents the prefix-only routes.
	}
	if strings.HasPrefix(path, "repos/") {
		parts := strings.Split(strings.TrimPrefix(path, "repos/"), "/")
		allow := "GET, PATCH, DELETE"
		if len(parts) == 2 && parts[1] == "grants" {
			allow = "GET, POST, DELETE"
		} else if len(parts) == 2 && parts[1] == "preload" {
			allow = "POST"
		} else if len(parts) == 2 && parts[1] == "pins" {
			allow = "GET, POST, DELETE"
		} else if len(parts) != 1 {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
			return
		}
		if !apiMethodAllowed(allow, r.Method) {
			methodNotAllowedWith(w, allow)
			return
		}
	}
	if strings.HasPrefix(path, "users/") && strings.Count(strings.TrimPrefix(path, "users/"), "/") == 0 && r.Method != http.MethodPatch {
		methodNotAllowedWith(w, http.MethodPatch)
		return
	}
	if strings.HasPrefix(path, "groups/") && strings.Count(strings.TrimPrefix(path, "groups/"), "/") == 0 && r.Method != http.MethodGet && r.Method != http.MethodPatch {
		methodNotAllowedWith(w, "GET, PATCH")
		return
	}
	switch {
	case path == "auth/providers" && r.Method == http.MethodGet:
		providers := map[string]any{"local": s.currentConfig().AuthMode == "local", "oidc": s.currentOIDCConfig() != nil, "ldap": s.currentConfig().LDAPURL != ""}
		writeJSON(w, http.StatusOK, providers)
	case path == "me" && r.Method == http.MethodGet:
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view your profile.")
			return
		}
		writeJSON(w, http.StatusOK, p)
	case path == "healthz" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case path == "stats" && r.Method == http.MethodGet:
		s.apiStats(w, r)
	case path == "downloads/active" && r.Method == http.MethodGet:
		s.apiActiveDownloads(w, r)
	case path == "repos":
		s.apiRepos(w, r)
	case strings.HasPrefix(path, "repos/"):
		s.apiRepo(w, r, strings.TrimPrefix(path, "repos/"))
	case path == "cache/entries" && r.Method == http.MethodGet:
		s.apiCacheEntries(w, r)
	case strings.HasPrefix(path, "cache/entries/"):
		s.apiCacheEntry(w, r, strings.TrimPrefix(path, "cache/entries/"))
	case path == "cache/purge" && r.Method == http.MethodPost:
		s.apiPurge(w, r)
	case path == "users" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.apiUsers(w, r)
	case strings.HasPrefix(path, "users/") && r.Method == http.MethodPatch:
		s.apiUserPatch(w, r, strings.TrimPrefix(path, "users/"))
	case path == "groups" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.apiGroups(w, r)
	case strings.HasPrefix(path, "groups/"):
		if strings.Count(path, "/") != 1 {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
			return
		}
		s.apiGroup(w, r, strings.TrimPrefix(path, "groups/"))
	case path == "audit-events" && r.Method == http.MethodGet:
		s.apiAudit(w, r)
	case path == "settings/public" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"auth_mode": s.currentConfig().AuthMode, "anonymous_public_read": s.currentConfig().AnonymousPublicRead, "oidc_enabled": s.currentOIDCConfig() != nil, "ldap_enabled": s.currentConfig().LDAPURL != "", "ldap_auto_provision": s.currentConfig().LDAPAutoProvision, "s3_enabled": true, "hub_base_url": s.currentConfig().HFBaseURL, "presigned_urls": false})
	case path == "settings":
		s.apiSettings(w, r)
	case path == "settings/test-s3" && r.Method == http.MethodPost:
		s.apiTestS3(w, r)
	case path == "settings/test-hub" && r.Method == http.MethodPost:
		s.apiTestHub(w, r)
	default:
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
	}
}

func apiMethodAllowed(allow, method string) bool {
	for _, candidate := range strings.Split(allow, ", ") {
		if candidate == method {
			return true
		}
	}
	return false
}

func isMutation(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view cache statistics.")
		return
	}
	var repos, artifacts, totalBytes, hits, misses int64
	visibility := `($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM repositories r WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&repos); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&artifacts, &totalBytes); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FILTER(WHERE d.status='hit'),COUNT(*) FILTER(WHERE d.status='miss') FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&hits, &misses); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	var active int64
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE d.status='downloading' AND r.deleted_at IS NULL AND `+visibility, p.Role, p.ID).Scan(&active)
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos, "artifacts": artifacts, "cache_bytes": totalBytes, "cache_size": formatBytes(totalBytes), "cache_hits": hits, "cache_misses": misses, "hit_ratio": func() float64 {
		if hits+misses == 0 {
			return 0
		}
		return float64(hits) / float64(hits+misses)
	}(), "downloads_in_progress": active, "actor_role": p.Role})
}

func (s *Server) apiActiveDownloads(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view active downloads.")
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT d.id,r.repo_type,r.repo_id,d.path,d.bytes,d.started_at
		FROM downloads d JOIN repositories r ON r.id=d.repository_id
		WHERE d.status='downloading' AND r.deleted_at IS NULL
		AND ($1='admin' OR r.access_policy='public'
			OR (r.access_policy='allowlist' AND (
			EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2)
			OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))
		ORDER BY d.started_at DESC LIMIT 50`, p.Role, p.ID)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, repoType, repoID, path string
		var bytes int64
		var started time.Time
		if err := rows.Scan(&id, &repoType, &repoID, &path, &bytes, &started); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
			return
		}
		items = append(items, map[string]any{"id": id, "repo_type": repoType, "repo_id": repoID, "path": path, "bytes": bytes, "started_at": started})
	}
	if err := rows.Err(); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read active downloads.")
		return
	}
	preloads, err := s.preloadSnapshots(r.Context(), p)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to read preload status.")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "preloads": preloads})
}

func (s *Server) apiCacheEntries(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, 401, "Authentication required", "Sign in to view cache metadata.")
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	if cursor != "" {
		writeProblem(w, r, 400, "Invalid cursor", "Opaque cursors are not yet accepted; omit cursor to list the first page.")
		return
	}
	repoFilter := r.URL.Query().Get("repo_id")
	pathFilter := r.URL.Query().Get("path")
	query := `SELECT a.id,r.repo_type,r.repo_id,a.commit_sha,a.path,b.size_bytes,b.sha256,a.etag,a.cached_at,a.last_accessed_at
		FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id
		WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))
		AND ($3='' OR r.repo_id=$3) AND ($4='' OR strpos(lower(a.path),lower($4))>0) ORDER BY a.cached_at DESC LIMIT $5`
	rows, err := s.db.SQL.QueryContext(r.Context(), query, p.Role, p.ID, repoFilter, pathFilter, limit)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to search cache.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, t, repo, commit, path, sha, etag string
		var size int64
		var cached, accessed time.Time
		if rows.Scan(&id, &t, &repo, &commit, &path, &size, &sha, &etag, &cached, &accessed) == nil {
			items = append(items, map[string]any{"id": id, "repo_type": t, "repo_id": repo, "commit_sha": commit, "path": path, "size_bytes": size, "sha256": sha, "etag": etag, "cached_at": cached, "last_accessed_at": accessed})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "next_cursor": ""})
}

func (s *Server) apiCacheEntry(w http.ResponseWriter, r *http.Request, id string) {
	if !strings.HasSuffix(id, "/purge") {
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowedWith(w, http.MethodPost)
		return
	}
	id = strings.TrimSuffix(id, "/purge")
	p, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	visibility := `r.deleted_at IS NULL AND ($2='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$3) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$3))))`
	var visible bool
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE a.id=$1 AND `+visibility+`)`, id, p.Role, p.ID).Scan(&visible); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to check cache entry visibility.")
		return
	}
	if !visible {
		writeProblem(w, r, 404, "Cache entry not found", "The cache entry does not exist.")
		return
	}
	if err := s.purgeArtifact(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, 404, "Cache entry not found", "The cache entry does not exist.")
			return
		}
		writeProblem(w, r, 500, "Purge failed", "Unable to purge the cache entry.")
		return
	}
	s.audit(r.Context(), p.ID, "cache.entry.purge", "artifact", id, nil)
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) apiPurge(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireOperator(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeProblem(w, r, 400, "Invalid request", "Unable to read purge filters.")
		return
	}
	dry := r.URL.Query().Get("dry_run") == "true" || r.FormValue("dry_run") == "true"
	confirm := r.URL.Query().Get("confirm") == "true" || r.FormValue("confirm") == "true"
	repoID := r.URL.Query().Get("repo_id")
	path := r.URL.Query().Get("path")
	visibility := `r.deleted_at IS NULL AND ($3='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$4) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$4))))`
	var count int64
	var size int64
	err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0) AND `+visibility, repoID, path, p.Role, p.ID).Scan(&count, &size)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to estimate purge impact.")
		return
	}
	if dry || !confirm {
		writeJSON(w, 200, map[string]any{"dry_run": true, "entries": count, "bytes": size, "size": formatBytes(size), "confirmation_required": true})
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT a.id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR strpos(lower(a.path),lower($2))>0) AND `+visibility, repoID, path, p.Role, p.ID)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to find cache entries.")
		return
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := s.purgeArtifact(r.Context(), id); err != nil {
			s.logger.Warn("cache purge entry failed")
		}
	}
	s.audit(r.Context(), p.ID, "cache.purge", "cache", "filtered", map[string]any{"entries": len(ids), "repo_id": repoID, "path_filter": path})
	writeJSON(w, 200, map[string]any{"dry_run": false, "entries_purged": len(ids), "estimated_bytes": size})
}

func (s *Server) purgeArtifact(ctx context.Context, id string) error {
	tx, err := s.db.SQL.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var blobID string
	err = tx.QueryRowContext(ctx, `SELECT b.id FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.id=$1 FOR UPDATE OF a`, id).Scan(&blobID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE blobs SET orphaned_at=now() WHERE id=$1`, blobID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Keep the blob row/object as an orphan until a grace-period GC removes it.
	// This prevents a purge racing a re-download from deleting the new object.
	return nil
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	cursor := r.URL.Query().Get("cursor")
	args := []any{limit + 1}
	where := ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		parts := strings.SplitN(string(decoded), "|", 2)
		if len(parts) != 2 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		created, err := time.Parse(time.RFC3339Nano, parts[0])
		if err != nil || parts[1] == "" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The audit cursor is invalid.")
			return
		}
		where = "WHERE (e.created_at,e.id) < ($1,$2)"
		args = []any{created, parts[1], limit + 1}
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT e.id,COALESCE(u.username,''),e.action,e.target_type,e.target_id,e.metadata_json,e.request_id,e.created_at FROM audit_events e LEFT JOIN users u ON u.id=e.actor_user_id `+where+` ORDER BY e.created_at DESC,e.id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read audit events.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, actor, action, targetType, targetID, metadata, requestID string
		var created time.Time
		if rows.Scan(&id, &actor, &action, &targetType, &targetID, &metadata, &requestID, &created) == nil {
			var decoded any
			_ = json.Unmarshal([]byte(metadata), &decoded)
			items = append(items, map[string]any{"id": id, "actor": actor, "action": action, "target_type": targetType, "target_id": targetID, "metadata": decoded, "request_id": requestID, "created_at": created})
		}
	}
	nextCursor := ""
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		if created, ok := last["created_at"].(time.Time); ok {
			nextCursor = base64.RawURLEncoding.EncodeToString([]byte(created.UTC().Format(time.RFC3339Nano) + "|" + last["id"].(string)))
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": cursor, "next_cursor": nextCursor})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeProblem(w, r, 400, "Invalid JSON", "The request body is invalid or too large.")
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		writeProblem(w, r, 400, "Invalid JSON", "Provide exactly one JSON value.")
		return false
	}
	return true
}

func pageLimit(value string) int {
	if value == "" {
		return 50
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 {
		return 50
	}
	if limit > 200 {
		return 200
	}
	return limit
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func encodeListCursor(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	return base64.RawURLEncoding.EncodeToString(encoded)
}

func decodeListCursor(value string, count int) ([]string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return nil, false
	}
	var parts []string
	if err := json.Unmarshal(decoded, &parts); err != nil {
		// Accept cursors emitted by older versions during rolling upgrades.
		parts = strings.Split(string(decoded), "|")
	}
	if len(parts) != count {
		return nil, false
	}
	for _, part := range parts {
		if part == "" || strings.ContainsAny(part, "\r\n") {
			return nil, false
		}
	}
	return parts, true
}

func hasScope(p *Principal, scope string) bool {
	for _, candidate := range p.Scopes {
		if candidate == scope {
			return true
		}
	}
	return false
}
