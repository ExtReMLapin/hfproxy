package server

import (
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/nicolas/hfproxy/internal/db"
)

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.requireAdmin(w, r); !ok {
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		args := []any{limit + 1}
		where := ""
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 2)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The group cursor is invalid.")
				return
			}
			where = "WHERE (g.name,g.id) > ($1,$2)"
			args = []any{parts[0], parts[1], limit + 1}
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT g.id,g.name,g.source,COUNT(m.user_id) FROM groups g LEFT JOIN group_members m ON m.group_id=g.id `+where+` GROUP BY g.id ORDER BY g.name,g.id LIMIT $`+strconv.Itoa(len(args)), args...)
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
		next := ""
		if len(items) > limit {
			last := items[limit-1]
			items = items[:limit]
			next = encodeListCursor(last["name"].(string), last["id"].(string))
		}
		writeJSON(w, 200, map[string]any{"items": items, "limit": limit, "cursor": r.URL.Query().Get("cursor"), "next_cursor": next})
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
		if isUniqueViolation(err) {
			writeProblem(w, r, 409, "Group exists", "A group with this name already exists.")
		} else {
			writeProblem(w, r, 500, "Database error", "Unable to create group.")
		}
		return
	}
	seenMembers := make(map[string]struct{}, len(input.Members))
	for _, member := range input.Members {
		if _, seen := seenMembers[member]; seen {
			continue
		}
		seenMembers[member] = struct{}{}
		if _, err = tx.ExecContext(r.Context(), `INSERT INTO group_members(group_id,user_id) VALUES($1,$2)`, id, member); err != nil {
			if isForeignKeyViolation(err) {
				writeProblem(w, r, 400, "Invalid member", "A member ID is invalid.")
			} else {
				writeProblem(w, r, 500, "Database error", "Unable to add the group member.")
			}
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
	if (r.Method != http.MethodPatch && r.Method != http.MethodGet) || id == "" || strings.Contains(id, "/") {
		methodNotAllowed(w)
		return
	}
	p, ok := s.requireAdmin(w, r)
	if !ok {
		return
	}
	if r.Method == http.MethodGet {
		var name, source string
		if err := s.db.SQL.QueryRowContext(r.Context(), `SELECT name,source FROM groups WHERE id=$1`, id).Scan(&name, &source); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
			} else {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the group.")
			}
			return
		}
		rows, err := s.db.SQL.QueryContext(r.Context(), `SELECT u.id,u.username,u.role FROM group_members m JOIN users u ON u.id=m.user_id WHERE m.group_id=$1 ORDER BY u.username`, id)
		if err != nil {
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list group members.")
			return
		}
		defer rows.Close()
		members := []map[string]string{}
		for rows.Next() {
			var memberID, username, role string
			if rows.Scan(&memberID, &username, &role) == nil {
				members = append(members, map[string]string{"id": memberID, "username": username, "role": role})
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": name, "source": source, "members": members})
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
			if isUniqueViolation(err) {
				writeProblem(w, r, http.StatusConflict, "Group exists", "A group with this name already exists.")
			} else {
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to update the group.")
			}
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
				if isForeignKeyViolation(err) {
					writeProblem(w, r, http.StatusBadRequest, "Invalid member", "A member ID is invalid.")
				} else {
					writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to add the group member.")
				}
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
