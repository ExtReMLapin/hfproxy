package server

import (
	"errors"
	"net/http"
	"net/mail"
	"strings"

	"github.com/hfproxy/hfproxy/internal/db"
)

func (s *Server) apiUsers(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodPost {
		var input struct {
			Username    string `json:"username"`
			Email       string `json:"email"`
			DisplayName string `json:"display_name"`
			Password    string `json:"password"`
			Role        string `json:"role"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		input.Username = strings.TrimSpace(input.Username)
		input.Email = strings.TrimSpace(input.Email)
		input.DisplayName = strings.TrimSpace(input.DisplayName)
		if input.Role == "" {
			input.Role = "reader"
		}
		if title, detail := newUserProblem(input.Username, input.Email, input.DisplayName); title != "" {
			writeProblem(w, r, http.StatusBadRequest, title, detail)
			return
		}
		if input.Role != "admin" && input.Role != "operator" && input.Role != "reader" {
			writeProblem(w, r, http.StatusBadRequest, "Invalid role", "role must be admin, operator, or reader.")
			return
		}
		hash, err := HashPassword(input.Password)
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Invalid password", "Password must contain 12 to 1024 bytes.")
			return
		}
		id := db.NewID()
		if err := s.db.CreateUser(r.Context(), id, input.Username, input.Email, input.DisplayName, hash, input.Role); err != nil {
			if errors.Is(err, db.ErrExists) {
				writeProblem(w, r, http.StatusConflict, "Username already exists", "Choose a different username.")
				return
			}
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create user.")
			return
		}
		s.audit(r.Context(), p.ID, "user.create", "user", id, map[string]any{"username": input.Username, "role": input.Role})
		writeJSON(w, http.StatusCreated, map[string]string{"id": id, "username": input.Username, "role": input.Role, "status": "active"})
		return
	}
	if r.Method != http.MethodGet {
		methodNotAllowed(w)
		return
	}
	limit := pageLimit(r.URL.Query().Get("limit"))
	var after *[2]string
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		parts, ok := decodeListCursor(cursor, 2)
		if !ok {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The user cursor is invalid.")
			return
		}
		after = &[2]string{parts[0], parts[1]}
	}
	users, err := s.db.ListUsers(r.Context(), after, limit+1)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to list users.")
		return
	}
	items := make([]map[string]any, 0, len(users))
	for _, u := range users {
		items = append(items, map[string]any{"id": u.ID, "username": u.Username, "email": u.Email, "display_name": u.DisplayName, "role": u.Role, "status": u.Status, "created_at": u.CreatedAt})
	}
	next := ""
	if len(items) > limit {
		last := items[limit-1]
		items = items[:limit]
		next = encodeListCursor(last["username"].(string), last["id"].(string))
	}
	writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
}

func validLocalUsername(username string) bool {
	if username == "" || len(username) > 64 {
		return false
	}
	for _, r := range username {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r)) {
			return false
		}
	}
	return true
}

func validEmail(address string) bool {
	parsed, err := mail.ParseAddress(address)
	return err == nil && parsed.Address == address
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
	if err := s.db.UpdateUser(r.Context(), id, input.Role, input.Status); err != nil {
		var stage *db.StageError
		switch {
		case errors.Is(err, db.ErrNotFound):
			writeProblem(w, r, 404, "User not found", "The user does not exist.")
		case errors.Is(err, db.ErrLastAdmin):
			writeProblem(w, r, http.StatusConflict, "Last administrator", "At least one active administrator must remain.")
		case errors.As(err, &stage) && (stage.Stage == "lock" || stage.Stage == "count_admins"):
			writeProblem(w, r, 500, "Database error", "Unable to validate administrator safety.")
		case errors.As(err, &stage) && stage.Stage == "end_sessions":
			writeProblem(w, r, 500, "Database error", "Unable to end the user's sessions.")
		case errors.As(err, &stage) && stage.Stage == "revoke_tokens":
			writeProblem(w, r, 500, "Database error", "Unable to revoke the user's tokens.")
		default:
			writeProblem(w, r, 500, "Database error", "Unable to update user.")
		}
		return
	}
	s.authLookups().purge()
	s.audit(r.Context(), p.ID, "user.update", "user", id, map[string]any{"role": input.Role, "status": input.Status})
	writeJSON(w, 200, map[string]string{"id": id, "role": input.Role, "status": input.Status})
}

// newUserProblem checks the profile of a new local account and describes the
// first problem found, or returns empty strings when it is acceptable.
func newUserProblem(username, email, displayName string) (title, detail string) {
	if !validLocalUsername(username) {
		return "Invalid username", "Use 1 to 64 letters, digits, dots, underscores, or hyphens."
	}
	if len(email) > 254 || (email != "" && !validEmail(email)) || len(displayName) > 200 {
		return "Invalid profile", "Check the email address and keep the display name under 200 characters."
	}
	return "", ""
}
