package server

import (
	"net/http"
	"strings"
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
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT commit_sha,ref,created_at FROM cache_pins WHERE repository_id=$1 ORDER BY ref,commit_sha`, repositoryID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list pins.")
			return
		}
		defer rows.Close()
		type pin struct {
			Commit  string    `json:"commit"`
			Ref     string    `json:"ref"`
			Scope   string    `json:"scope"`
			Created time.Time `json:"created_at"`
		}
		pins := []pin{}
		for rows.Next() {
			var item pin
			if err := rows.Scan(&item.Commit, &item.Ref, &item.Created); err != nil {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list pins.")
				return
			}
			item.Scope = pinScope(item.Commit, item.Ref)
			pins = append(pins, item)
		}
		writeJSON(w, http.StatusOK, map[string]any{"pins": pins})
	case http.MethodPost:
		var input struct {
			Commit string `json:"commit"`
			Ref    string `json:"ref"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		if problem := pinTargetProblem(input.Commit, input.Ref); problem != "" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid pin", problem)
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `INSERT INTO cache_pins(repository_id,commit_sha,ref,created_by) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, repositoryID, input.Commit, input.Ref, p.ID)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to pin.")
			return
		}
		if added, _ := result.RowsAffected(); added > 0 {
			s.audit(r.Context(), p.ID, "cache.pin", "repository", repositoryID, map[string]any{"scope": pinScope(input.Commit, input.Ref), "commit": input.Commit, "ref": input.Ref})
		}
		writeJSON(w, http.StatusOK, map[string]string{"commit": input.Commit, "ref": input.Ref, "scope": pinScope(input.Commit, input.Ref)})
	case http.MethodDelete:
		commit, ref := r.URL.Query().Get("commit"), r.URL.Query().Get("ref")
		if problem := pinTargetProblem(commit, ref); problem != "" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid pin", problem)
			return
		}
		result, err := s.db.SQL.ExecContext(r.Context(), `DELETE FROM cache_pins WHERE repository_id=$1 AND commit_sha=$2 AND ref=$3`, repositoryID, commit, ref)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to unpin.")
			return
		}
		if n, _ := result.RowsAffected(); n == 0 {
			writeProblem(w, r, http.StatusNotFound, "Pin not found", "That pin does not exist.")
			return
		}
		s.audit(r.Context(), p.ID, "cache.unpin", "repository", repositoryID, map[string]any{"scope": pinScope(commit, ref), "commit": commit, "ref": ref})
		w.WriteHeader(http.StatusNoContent)
	default:
		methodNotAllowedWith(w, "GET, POST, DELETE")
	}
}

// pinTargetProblem explains why a commit/ref pair cannot name a pin, or returns
// "" when it can. Both empty means the whole repository; otherwise exactly one
// of them is given.
func pinTargetProblem(commit, ref string) string {
	switch {
	case commit != "" && ref != "":
		return "Give either a commit or a ref, not both."
	case commit != "" && !validCommit(commit):
		return "commit must be a full 40 or 64 character commit SHA."
	case ref != "" && validCommit(ref):
		return "That looks like a commit SHA: pin it with the commit field."
	case ref != "" && !validRefName(ref):
		return "ref must be a branch or tag name."
	}
	return ""
}

// validRefName accepts what Git allows in a branch or tag name: no whitespace,
// control characters or any of ~ ^ : ? * [ \, no "..", "@{" or ".lock" ending,
// and no empty path segment.
func validRefName(ref string) bool {
	if !validRevision(ref) || len(ref) > 255 || strings.ContainsAny(ref, " \t~^:?*[") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.HasSuffix(ref, ".lock") || strings.HasSuffix(ref, ".") {
		return false
	}
	return true
}

func pinScope(commit, ref string) string {
	switch {
	case ref != "":
		return "ref"
	case commit != "":
		return "commit"
	}
	return "repository"
}
