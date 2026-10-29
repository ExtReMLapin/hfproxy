package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/hfproxy/hfproxy/internal/db"
)

func (s *Server) apiRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		p, err := s.authenticate(r)
		if err != nil {
			writeProblem(w, r, 401, "Authentication required", "Sign in to view repository policies.")
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		var after *[3]string
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 3)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The repository cursor is invalid.")
				return
			}
			after = &[3]string{parts[0], parts[1], parts[2]}
		}
		repos, err := s.db.ListRepositories(r.Context(), p.Role, p.ID, after, limit+1)
		if err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to list repositories.")
			return
		}
		result := make([]map[string]any, 0, len(repos))
		for _, repo := range repos {
			result = append(result, map[string]any{"id": repo.ID, "repo_type": repo.Type, "repo_id": repo.RepoID, "access_policy": repo.Policy, "origin": repo.Origin, "created_at": repo.CreatedAt, "updated_at": repo.UpdatedAt})
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
	id, err := s.db.UpsertRepository(r.Context(), input.RepoType, input.RepoID, input.AccessPolicy, p.ID)
	if errors.Is(err, db.ErrExists) {
		writeProblem(w, r, http.StatusConflict, "Name taken", "A model or dataset published on this proxy already has this name.")
		return
	}
	if err != nil {
		s.fail(w, r, err, 500, "Database error", "Unable to save repository policy.")
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
		repo, err := s.db.GetRepository(r.Context(), id)
		if errors.Is(err, db.ErrNotFound) {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		if err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to load repository.")
			return
		}
		writeJSON(w, 200, map[string]any{"id": id, "repo_type": repo.Type, "repo_id": repo.RepoID, "access_policy": repo.Policy, "origin": repo.Origin, "created_at": repo.CreatedAt, "updated_at": repo.UpdatedAt})
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
		if err := s.db.SetRepositoryPolicy(r.Context(), id, input.AccessPolicy); err != nil {
			if errors.Is(err, db.ErrNotFound) {
				writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			} else {
				s.fail(w, r, err, 500, "Database error", "Unable to update policy.")
			}
			return
		}
		s.audit(r.Context(), p.ID, "repository.policy.update", "repository", id, map[string]any{"access_policy": input.AccessPolicy})
		writeJSON(w, 200, map[string]string{"id": id, "access_policy": input.AccessPolicy})
	case http.MethodDelete:
		deleteRepository := s.db.DeleteRepository
		if repo, err := s.db.GetRepository(r.Context(), id); err == nil && repo.Origin == "local" {
			// A published repository is removed with its history, not hidden.
			deleteRepository = s.db.DeleteLocalRepository
			s.forgetCredentials(r.Context())
		}
		if err := deleteRepository(r.Context(), id); err != nil {
			var stage *db.StageError
			switch {
			case errors.Is(err, db.ErrNotFound):
				writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			case errors.As(err, &stage) && stage.Stage == "grants":
				s.fail(w, r, err, 500, "Database error", "Unable to remove repository grants.")
			default:
				s.fail(w, r, err, 500, "Database error", "Unable to delete repository.")
			}
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
		grants, err := s.db.ListGrants(r.Context(), repoID)
		if err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to list grants.")
			return
		}
		items := []map[string]string{}
		for _, g := range grants {
			items = append(items, map[string]string{"subject_type": g.SubjectType, "subject_id": g.SubjectID, "permission": g.Permission})
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
		if (input.SubjectType != "user" && input.SubjectType != "group") || input.SubjectID == "" || input.Permission != "read" {
			writeProblem(w, r, 400, "Invalid grant", "Provide a user or group subject; the only permission is \"read\" (the proxy never writes to a repository).")
			return
		}
		repositoryExists, err := s.db.RepositoryExists(r.Context(), repoID)
		if err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to validate the repository.")
			return
		}
		if !repositoryExists {
			writeProblem(w, r, 404, "Repository not found", "The repository does not exist.")
			return
		}
		subjectExists, err := s.db.GrantSubjectExists(r.Context(), input.SubjectType, input.SubjectID)
		if err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to validate the grant subject.")
			return
		}
		if !subjectExists {
			writeProblem(w, r, 404, "Grant subject not found", "The selected user or group does not exist.")
			return
		}
		if err := s.db.UpsertGrant(r.Context(), repoID, input.SubjectType, input.SubjectID, input.Permission); err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to save grant.")
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
		if err := s.db.DeleteGrant(r.Context(), repoID, subjectType, subjectID); err != nil {
			s.fail(w, r, err, 500, "Database error", "Unable to remove grant.")
			return
		}
		s.audit(r.Context(), p.ID, "repository.grant.delete", "repository", repoID, map[string]any{"subject_type": subjectType, "subject_id": subjectID})
		w.WriteHeader(204)
		return
	}
	methodNotAllowed(w)
}
