package server

import (
	"net/http"
	"time"
)

// apiRepoPins manages the pins of one repository: GET lists them, POST adds one
// and DELETE removes one. A pin names either the whole repository (an empty
// commit) or one commit, and the cache never evicts what a pin covers.
func (s *Server) apiRepoPins(w http.ResponseWriter, r *http.Request, repositoryID string) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	var exists bool
	if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT EXISTS(SELECT 1 FROM repositories WHERE id=$1 AND deleted_at IS NULL)`, repositoryID).Scan(&exists); err != nil {
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the repository.")
		return
	}
	if !exists {
		writeProblem(w, r, http.StatusNotFound, "Repository not found", "The repository does not exist.")
		return
	}
	switch r.Method {
	case http.MethodGet:
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT commit_sha,created_at FROM cache_pins WHERE repository_id=$1 ORDER BY commit_sha`, repositoryID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list pins.")
			return
		}
		defer rows.Close()
		type pin struct {
			Commit  string    `json:"commit"`
			Scope   string    `json:"scope"`
			Created time.Time `json:"created_at"`
		}
		pins := []pin{}
		for rows.Next() {
			var item pin
			if err := rows.Scan(&item.Commit, &item.Created); err != nil {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list pins.")
				return
			}
			item.Scope = pinScope(item.Commit)
			pins = append(pins, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
	case http.MethodPost:
		var input struct {
			Commit string `json:"commit"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if !validPinCommit(input.Commit) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid commit", "commit must be a full 40 or 64 character commit SHA, or empty to pin the whole repository.")
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `INSERT INTO cache_pins(repository_id,commit_sha,created_by) VALUES($1,$2,$3) ON CONFLICT DO NOTHING`, repositoryID, input.Commit, p.ID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to pin.")
			return
		}
		if added, _ := result.RowsAffected(); added > 0 {
			s.audit(r.Context(), p.ID, "cache.pin", "repository", repositoryID, map[string]any{"scope": pinScope(input.Commit), "commit": input.Commit})
		}
		writeJSON(w, http.StatusOK, map[string]string{"commit": input.Commit, "scope": pinScope(input.Commit)})
	case http.MethodDelete:
		commit := r.URL.Query().Get("commit")
		if !validPinCommit(commit) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid commit", "commit must be a full 40 or 64 character commit SHA, or empty for the repository-wide pin.")
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM cache_pins WHERE repository_id=$1 AND commit_sha=$2`, repositoryID, commit)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to unpin.")
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			writeProblem(w, r, http.StatusNotFound, "Pin not found", "That pin does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "cache.unpin", "repository", repositoryID, map[string]any{"scope": pinScope(commit), "commit": commit})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowedWith(w, "GET, POST, DELETE")
	}
}

func validPinCommit(commit string) bool { return commit == "" || validCommit(commit) }

func pinScope(commit string) string {
	if commit == "" {
		return "repository"
	}
	return "commit"
}
