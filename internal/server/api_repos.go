package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
)

func (s *Server) apiRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, 401, "Authentication required", "Sign in to view repository policies.")
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		args := []any{p.Role, p.ID, limit + 1}
		cursorWhere := ""
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 3)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The repository cursor is invalid.")
				return
			}
			cursorWhere = " AND (r.repo_type,r.repo_id,r.id) > ($3,$4,$5)"
			args = []any{p.Role, p.ID, parts[0], parts[1], parts[2], limit + 1}
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT r.id,r.repo_type,r.repo_id,r.access_policy,r.created_at,r.updated_at
			FROM repositories r WHERE r.deleted_at IS NULL AND ($1='admin' OR r.access_policy='public' OR (r.access_policy='allowlist' AND (EXISTS(SELECT 1 FROM repo_grants g WHERE g.repository_id=r.id AND g.subject_type='user' AND g.subject_id=$2) OR EXISTS(SELECT 1 FROM repo_grants g JOIN group_members m ON m.group_id=g.subject_id WHERE g.repository_id=r.id AND g.subject_type='group' AND m.user_id=$2))))`+cursorWhere+`
			ORDER BY r.repo_type,r.repo_id,r.id LIMIT $`+strconv.Itoa(len(args)), args...)
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
		next := ""
		if len(result) > limit {
			last := result[limit-1]
			next = encodeListCursor(last["repo_type"].(string), last["repo_id"].(string), last["id"].(string))
			result = result[:limit]
		}
		writeJSON(w, 200, map[string]any{"items": result, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
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
	if len(parts) == 2 && parts[1] == "preload" && r.Method == http.MethodPost {
		s.startRepoPreload(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[1] == "pins" {
		s.apiRepoPins(w, r, parts[0])
		return
	}
	if len(parts) != 1 {
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
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
		tx, err := s.db.SQL.BeginTx(r.Context(), nil)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		defer tx.Rollback()
		result, err := tx.ExecContext(r.Context(), `UPDATE repositories SET deleted_at=now(),updated_at=now() WHERE id=$1 AND deleted_at IS NULL`, id)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
			return
		}
		n, _ := result.RowsAffected()
		if n == 0 {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		if _, err := tx.ExecContext(r.Context(), `DELETE FROM repo_grants WHERE repository_id=$1`, id); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to remove repository grants.")
			return
		}
		if err := tx.Commit(); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to delete repository.")
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
		var repositoryExists bool
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=$1 AND deleted_at IS NULL)`, repoID).Scan(&repositoryExists); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate the repository.")
			return
		}
		if !repositoryExists {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		table := "users"
		if input.SubjectType == "group" {
			table = "groups"
		}
		var subjectExists bool
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM `+table+` WHERE id=$1)`, input.SubjectID).Scan(&subjectExists); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate the grant subject.")
			return
		}
		if !subjectExists {
			writeProblem(w, r, 404, "Grant subject not found", "The selected user or group does not exist.")
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
