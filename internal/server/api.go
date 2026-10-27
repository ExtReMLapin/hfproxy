package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
)

func (s *Server) apiV1(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/v1/") {
		http.NotFound(w, r)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/v1/")
	switch {
	case path == "auth/providers" && r.Method == http.MethodGet:
		providers := map[string]any{"local": s.config.AuthMode == "local", "oidc": s.oidcConfig != nil, "ldap": s.config.LDAPURL != ""}
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
	case path == "users" && r.Method == http.MethodGet:
		s.apiUsers(w, r)
	case strings.HasPrefix(path, "users/") && r.Method == http.MethodPatch:
		s.apiUserPatch(w, r, strings.TrimPrefix(path, "users/"))
	case path == "groups" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		s.apiGroups(w, r)
	case strings.HasPrefix(path, "groups/"):
		s.apiGroup(w, r, strings.TrimPrefix(path, "groups/"))
	case path == "audit-events" && r.Method == http.MethodGet:
		s.apiAudit(w, r)
	case path == "settings/public" && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"auth_mode": s.config.AuthMode, "anonymous_public_read": s.config.AnonymousPublicRead, "oidc_enabled": s.oidcConfig != nil, "ldap_enabled": s.config.LDAPURL != "", "ldap_auto_provision": s.config.LDAPAutoProvision, "s3_enabled": true, "hub_base_url": s.config.HFBaseURL, "presigned_urls": false})
	default:
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
	}
}

func (s *Server) apiStats(w http.ResponseWriter, r *http.Request) {
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view cache statistics.")
		return
	}
	var repos, artifacts, totalBytes, hits, misses int64
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM repositories WHERE deleted_at IS NULL`).Scan(&repos); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN blobs b ON b.id=a.blob_id`).Scan(&artifacts, &totalBytes); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FILTER(WHERE status='hit'),COUNT(*) FILTER(WHERE status='miss') FROM downloads`).Scan(&hits, &misses); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to read statistics.")
		return
	}
	var active int64
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM downloads WHERE status='downloading'`).Scan(&active)
	writeJSON(w, http.StatusOK, map[string]any{"repositories": repos, "artifacts": artifacts, "cache_bytes": totalBytes, "cache_size": formatBytes(totalBytes), "cache_hits": hits, "cache_misses": misses, "hit_ratio": func() float64 {
		if hits+misses == 0 {
			return 0
		}
		return float64(hits) / float64(hits+misses)
	}(), "downloads_in_progress": active, "actor_role": p.Role})
}

func (s *Server) apiRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, 401, "Authentication required", "Sign in to view repository policies.")
			return
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT r.id,r.repo_type,r.repo_id,r.access_policy,r.created_at,r.updated_at
			FROM repositories r WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))
			ORDER BY r.repo_type,r.repo_id LIMIT 200`, p.Role, p.ID)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list repositories.")
			return
		}
		defer rows.Close()
		result := []map[string]any{}
		for rows.Next() {
			var id, t, repo, policy string
			var created, updated time.Time
			if rows.Scan(&id, &t, &repo, &policy, &created, &updated) == nil {
				result = append(result, map[string]any{"id": id, "repo_type": t, "repo_id": repo, "access_policy": policy, "created_at": created, "updated_at": updated})
			}
		}
		writeJSON(w, 200, map[string]any{"items": result, "limit": 200, "cursor": ""})
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		RepoType     string `json:"repo_type"`
		RepoID       string `json:"repo_id"`
		AccessPolicy string `json:"access_policy"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !validRepoType(input.RepoType) || !validRepoID(input.RepoID) || (input.AccessPolicy != "public" && input.AccessPolicy != "allowlist" && input.AccessPolicy != "admin_only") {
		writeProblem(w, r, 400, "Invalid repository", "Provide a valid repo_type, repo_id, and access_policy.")
		return
	}
	var id string
	err := s.db.SQL.QueryRowContext(r.Context(), `INSERT INTO repositories(id,repo_type,repo_id,access_policy,created_by) VALUES($1,$2,$3,$4,$5)
		ON CONFLICT(repo_type,repo_id) DO UPDATE SET access_policy=EXCLUDED.access_policy,deleted_at=NULL,updated_at=now() RETURNING id`, db.NewID(), input.RepoType, input.RepoID, input.AccessPolicy, p.ID).Scan(&id)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to save repository policy.")
		return
	}
	s.audit(r.Context(), p.ID, "repository.upsert", "repository", id, map[string]any{"repo_type": input.RepoType, "repo_id": input.RepoID, "access_policy": input.AccessPolicy})
	writeJSON(w, 201, map[string]string{"id": id, "repo_type": input.RepoType, "repo_id": input.RepoID, "access_policy": input.AccessPolicy})
}

func (s *Server) apiRepo(w http.ResponseWriter, r *http.Request, path string) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 2 && parts[1] == "grants" {
		s.apiRepoGrants(w, r, parts[0])
		return
	}
	if len(parts) != 1 {
		http.NotFound(w, r)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	id := parts[0]
	switch r.Method {
	case http.MethodGet:
		var repoType, repoID, policy string
		var created, updated time.Time
		err := s.db.SQL.QueryRowContext(r.Context(), `SELECT repo_type,repo_id,access_policy,created_at,updated_at FROM repositories WHERE id=$1 AND deleted_at IS NULL`, id).Scan(&repoType, &repoID, &policy, &created, &updated)
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to load repository.")
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "repo_type": repoType, "repo_id": repoID, "access_policy": policy, "created_at": created, "updated_at": updated})
	case http.MethodPatch:
		var input struct {
			AccessPolicy string `json:"access_policy"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if input.AccessPolicy != "public" && input.AccessPolicy != "allowlist" && input.AccessPolicy != "admin_only" {
			writeProblem(w, r, 400, "Invalid policy", "access_policy must be public, allowlist, or admin_only.")
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE repositories SET access_policy=$2,updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id, input.AccessPolicy)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to update policy.")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.policy.update", "repository", id, map[string]any{"access_policy": input.AccessPolicy})
		writeJSON(w, 200, map[string]string{"id": id, "access_policy": input.AccessPolicy})
	case http.MethodDelete:
		result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE repositories SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.delete", "repository", id, nil)
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowed(w)
	}
}

func (s *Server) apiRepoGrants(w http.ResponseWriter, r *http.Request, repoID string) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT subject_type,subject_id,permission FROM repo_grants WHERE repository_id=$1 ORDER BY subject_type,subject_id`, repoID)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list grants.")
			return
		}
		defer rows.Close()
		items := []map[string]string{}
		for rows.Next() {
			var a, b, c string
			if rows.Scan(&a, &b, &c) == nil {
				items = append(items, map[string]string{"subject_type": a, "subject_id": b, "permission": c})
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			SubjectType string `json:"subject_type"`
			SubjectID   string `json:"subject_id"`
			Permission  string `json:"permission"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if (input.SubjectType != "user" && input.SubjectType != "group") || input.SubjectID == "" || (input.Permission != "read" && input.Permission != "write" && input.Permission != "admin") {
			writeProblem(w, r, 400, "Invalid grant", "Provide a user or group subject and a valid permission.")
			return
		}
		_, err := s.db.SQL.ExecContext(r.Context(), `INSERT INTO repo_grants(repository_id,subject_type,subject_id,permission) VALUES($1,$2,$3,$4) ON CONFLICT(repository_id,subject_type,subject_id) DO UPDATE SET permission=EXCLUDED.permission`, repoID, input.SubjectType, input.SubjectID, input.Permission)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to save grant.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.grant.upsert", "repository", repoID, map[string]any{"subject_type": input.SubjectType, "subject_id": input.SubjectID, "permission": input.Permission})
		writeJSON(w, 201, input)
		return
	}
	if r.Method == http.MethodDelete {
		subjectType := r.URL.Query().Get("subject_type")
		subjectID := r.URL.Query().Get("subject_id")
		if subjectType != "user" && subjectType != "group" || subjectID == "" {
			writeProblem(w, r, 400, "Invalid grant", "subject_type and subject_id are required.")
			return
		}
		_, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM repo_grants WHERE repository_id=$1 AND subject_type=$2 AND subject_id=$3`, repoID, subjectType, subjectID)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to remove grant.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.grant.delete", "repository", repoID, map[string]any{"subject_type": subjectType, "subject_id": subjectID})
		w.WriteHeader(204)
		return
	}
	methodNotAllowed(w)
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
		WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))
		AND ($3='' OR r.repo_id=$3) AND ($4='' OR a.path ILIKE '%'||$4||'%') ORDER BY a.cached_at DESC LIMIT $5`
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
	if !strings.HasSuffix(id, "/purge") || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	id = strings.TrimSuffix(id, "/purge")
	p, ok := s.requireAdmin(w, r)
	if !ok {
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
	p, ok := s.requireAdmin(w, r)
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
	var count int64
	var size int64
	err := s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*),COALESCE(SUM(b.size_bytes),0) FROM artifacts a JOIN repositories r ON r.id=a.repository_id JOIN blobs b ON b.id=a.blob_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR a.path ILIKE '%'||$2||'%')`, repoID, path).Scan(&count, &size)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to estimate purge impact.")
		return
	}
	if dry || !confirm {
		writeJSON(w, 200, map[string]any{"dry_run": true, "entries": count, "bytes": size, "size": formatBytes(size), "confirmation_required": true})
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT a.id FROM artifacts a JOIN repositories r ON r.id=a.repository_id WHERE ($1='' OR r.repo_id=$1) AND ($2='' OR a.path ILIKE '%'||$2||'%')`, repoID, path)
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
	var key, blobID string
	err = tx.QueryRowContext(ctx, `SELECT b.storage_key,b.id FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.id=$1 FOR UPDATE OF a`, id).Scan(&key, &blobID)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id=$1`, id); err != nil {
		return err
	}
	var refs int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts WHERE blob_id=$1`, blobID).Scan(&refs); err != nil {
		return err
	}
	if refs == 0 {
		if _, err = tx.ExecContext(ctx, `DELETE FROM blobs WHERE id=$1`, blobID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if refs == 0 {
		return s.storage.Delete(ctx, key)
	}
	return nil
}

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,username,email,display_name,role,status,created_at FROM users ORDER BY username LIMIT 200`)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to list users.")
		return
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var id, user, email, name, role, status string
		var created time.Time
		if rows.Scan(&id, &user, &email, &name, &role, &status, &created) == nil {
			items = append(items, map[string]any{"id": id, "username": user, "email": email, "display_name": name, "role": role, "status": status, "created_at": created})
		}
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": 200, "cursor": ""})
}

func (s *Server) apiUserPatch(w http.ResponseWriter, r *http.Request, id string) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Role   string `json:"role"`
		Status string `json:"status"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Role != "" && input.Role != "admin" && input.Role != "operator" && input.Role != "reader" {
		writeProblem(w, r, 400, "Invalid role", "role must be admin, operator, or reader.")
		return
	}
	if input.Status != "" && input.Status != "active" && input.Status != "disabled" {
		writeProblem(w, r, 400, "Invalid status", "status must be active or disabled.")
		return
	}
	if input.Role == "" && input.Status == "" {
		writeProblem(w, r, 400, "Empty update", "Set role or status.")
		return
	}
	result, err := s.db.SQL.ExecContext(r.Context(), `UPDATE users SET role=COALESCE(NULLIF($2,''),role),status=COALESCE(NULLIF($3,''),status),updated_at=now() WHERE id=$1`, id, input.Role, input.Status)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		writeProblem(w, r, 404, "User not found", "The user does not exist.")
		return
	}
	if input.Status == "disabled" {
		_, _ = s.db.SQL.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id)
		_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE access_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
	}
	s.audit(r.Context(), p.ID, "user.update", "user", id, map[string]any{"role": input.Role, "status": input.Status})
	writeJSON(w, 200, map[string]string{"id": id, "role": input.Role, "status": input.Status})
}

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.requireAdmin(w, r); !ok {
			return
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT g.id,g.name,g.source,COUNT(m.user_id) FROM groups g LEFT JOIN group_members m ON m.group_id=g.id GROUP BY g.id ORDER BY g.name LIMIT 200`)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list groups.")
			return
		}
		defer rows.Close()
		items := []map[string]any{}
		for rows.Next() {
			var id, name, source string
			var members int
			if rows.Scan(&id, &name, &source, &members) == nil {
				items = append(items, map[string]any{"id": id, "name": name, "source": source, "members": members})
			}
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		writeProblem(w, r, 400, "Invalid group", "A group name is required (maximum 100 characters).")
		return
	}
	id := db.NewID()
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to start group update.")
		return
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), `INSERT INTO groups(id,name) VALUES($1,$2)`, id, input.Name); err != nil {
		writeProblem(w, r, 409, "Group exists", "A group with this name already exists.")
		return
	}
	for _, member := range input.Members {
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, id, member); err != nil {
			writeProblem(w, r, 400, "Invalid member", "A member ID is invalid.")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to save group.")
		return
	}
	s.audit(r.Context(), p.ID, "group.create", "group", id, map[string]any{"name": input.Name, "member_count": len(input.Members)})
	writeJSON(w, 201, map[string]string{"id": id, "name": input.Name})
}

func (s *Server) apiGroup(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPatch || id == "" || strings.Contains(id, "/") {
		methodNotAllowed(w)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var input struct {
		Name    *string   `json:"name"`
		Members *[]string `json:"members"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if input.Name == nil && input.Members == nil {
		writeProblem(w, r, http.StatusBadRequest, "Empty update", "Set name or members.")
		return
	}
	name := ""
	if input.Name != nil {
		name = strings.TrimSpace(*input.Name)
		if name == "" || len(name) > 100 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid group", "A group name is required (maximum 100 characters).")
			return
		}
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to start group update.")
		return
	}
	defer tx.Rollback()
	var currentName string
	if err := tx.QueryRowContext(r.Context(), `SELECT name FROM groups WHERE id=$1 FOR UPDATE`, id).Scan(&currentName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
		} else {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the group.")
		}
		return
	}
	if input.Name != nil {
		if _, err := tx.ExecContext(r.Context(), `UPDATE groups SET name=$2 WHERE id=$1`, id, name); err != nil {
			writeProblem(w, r, http.StatusConflict, "Group exists", "A group with this name already exists.")
			return
		}
		currentName = name
	}
	if input.Members != nil {
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM group_members WHERE group_id=$1`, id); err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to replace group members.")
			return
		}
		for _, memberID := range *input.Members {
			if _, err := tx.ExecContext(r.Context(), `INSERT INTO group_members(group_id,user_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, id, memberID); err != nil {
				writeProblem(w, r, http.StatusBadRequest, "Invalid member", "A member ID is invalid.")
				return
			}
		}
	}
	if err := tx.Commit(); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to save group changes.")
		return
	}
	var members int
	_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM group_members WHERE group_id=$1`, id).Scan(&members)
	s.audit(r.Context(), p.ID, "group.update", "group", id, map[string]any{"name": currentName, "members": members})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": currentName, "members": members})
}

func (s *Server) apiAudit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireAdmin(w, r); !ok {
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT e.id,COALESCE(u.username,''),e.action,e.target_type,e.target_id,e.metadata_json,e.request_id,e.created_at FROM audit_events e LEFT JOIN users u ON u.id=e.actor_user_id ORDER BY e.created_at DESC LIMIT $1`, limit)
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
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": ""})
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
