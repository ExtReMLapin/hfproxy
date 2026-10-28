package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"github.com/nicolas/hfproxy/internal/db"
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
		if !validLocalUsername(input.Username) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid username", "Use 1 to 64 letters, digits, dots, underscores, or hyphens.")
			return
		}
		if len(input.Email) > 254 || (input.Email != "" && !validEmail(input.Email)) || len(input.DisplayName) > 200 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid profile", "Check the email address and keep the display name under 200 characters.")
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
		_, err = s.db.SQL.ExecContext(r.Context(), `INSERT INTO users(id,username,email,display_name,password_hash,role) VALUES($1,$2,$3,$4,$5,$6)`, id, input.Username, input.Email, input.DisplayName, hash, input.Role)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "unique") || strings.Contains(strings.ToLower(err.Error()), "duplicate") {
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
	args := []any{limit + 1}
	where := ""
	if cursor := r.URL.Query().Get("cursor"); cursor != "" {
		parts, ok := decodeListCursor(cursor, 2)
		if !ok {
			writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The user cursor is invalid.")
			return
		}
		where = "WHERE (username,id) > ($1,$2)"
		args = []any{parts[0], parts[1], limit + 1}
	}
	rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT id,username,email,display_name,role,status,created_at FROM users `+where+` ORDER BY username,id LIMIT $`+strconv.Itoa(len(args)), args...)
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
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	defer tx.Rollback()
	// Serialize changes that could remove an active administrator before
	// locking the target row. Locking only that row lets concurrent requests
	// each observe the other administrator and both demote successfully.
	if _, err := tx.ExecContext(r.Context(), `SELECT pg_advisory_xact_lock(7242)`); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to validate administrator safety.")
		return
	}
	var currentRole, currentStatus string
	if err := tx.QueryRowContext(r.Context(), `SELECT role,status FROM users WHERE id=$1 FOR UPDATE`, id).Scan(&currentRole, &currentStatus); errors.Is(err, sql.ErrNoRows) {
		writeProblem(w, r, 404, "User not found", "The user does not exist.")
		return
	} else if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	if currentRole == "admin" && currentStatus == "active" && (input.Role == "reader" || input.Role == "operator" || input.Status == "disabled") {
		var activeAdmins int
		if err := tx.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM users WHERE role='admin' AND status='active'`).Scan(&activeAdmins); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to validate administrator safety.")
			return
		}
		if activeAdmins <= 1 {
			writeProblem(w, r, http.StatusConflict, "Last administrator", "At least one active administrator must remain.")
			return
		}
	}
	result, err := tx.ExecContext(r.Context(), `UPDATE users SET role=COALESCE(NULLIF($2,''),role),status=COALESCE(NULLIF($3,''),status),updated_at=now() WHERE id=$1`, id, input.Role, input.Status)
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
		_, _ = tx.ExecContext(r.Context(), `DELETE FROM sessions WHERE user_id=$1`, id)
		_, _ = tx.ExecContext(r.Context(), `UPDATE access_tokens SET revoked_at=now() WHERE user_id=$1 AND revoked_at IS NULL`, id)
	}
	if err := tx.Commit(); err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to update user.")
		return
	}
	s.audit(r.Context(), p.ID, "user.update", "user", id, map[string]any{"role": input.Role, "status": input.Status})
	writeJSON(w, 200, map[string]string{"id": id, "role": input.Role, "status": input.Status})
}
