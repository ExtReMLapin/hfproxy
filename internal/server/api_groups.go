package server

import (
	"errors"
	"net/http"
	"strings"

	"github.com/nicolas/hfproxy/internal/db"
)

func (s *Server) apiGroups(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		if _, ok := s.requireAdmin(w, r); !ok {
			return
		}
		limit := pageLimit(r.URL.Query().Get("limit"))
		var after *[2]string
		if cursor := r.URL.Query().Get("cursor"); cursor != "" {
			parts, ok := decodeListCursor(cursor, 2)
			if !ok {
				writeProblem(w, r, http.StatusBadRequest, "Invalid cursor", "The group cursor is invalid.")
				return
			}
			after = &[2]string{parts[0], parts[1]}
		}
		groups, err := s.db.ListGroups(r.Context(), after, limit+1)
		if err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to list groups.")
			return
		}
		items := make([]map[string]any, 0, len(groups))
		for _, g := range groups {
			items = append(items, map[string]any{"id": g.ID, "name": g.Name, "source": g.Source, "members": g.Members})
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
	if err := s.db.CreateGroup(r.Context(), id, input.Name, input.Members); err != nil {
		switch {
		case errors.Is(err, db.ErrExists):
			writeProblem(w, r, 409, "Group exists", "A group with this name already exists.")
		case errors.Is(err, db.ErrInvalidMember):
			writeProblem(w, r, 400, "Invalid member", "A member ID is invalid.")
		default:
			var stage *db.StageError
			detail := "Unable to create group."
			if errors.As(err, &stage) {
				switch stage.Stage {
				case "begin":
					detail = "Unable to start group update."
				case "add_member":
					detail = "Unable to add the group member."
				case "commit":
					detail = "Unable to save group."
				}
			}
			writeProblem(w, r, 500, "Database error", detail)
		}
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
		group, found, err := s.db.GetGroup(r.Context(), id)
		if err != nil {
			var stage *db.StageError
			switch {
			case errors.Is(err, db.ErrNotFound):
				writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
			case errors.As(err, &stage) && stage.Stage == "members":
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to list group members.")
			default:
				writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to load the group.")
			}
			return
		}
		name, source := group.Name, group.Source
		members := []map[string]string{}
		for _, m := range found {
			members = append(members, map[string]string{"id": m.ID, "username": m.Username, "role": m.Role})
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
	var newName *string
	if input.Name != nil {
		newName = &name
	}
	currentName, members, err := s.db.UpdateGroup(r.Context(), id, newName, input.Members)
	if err != nil {
		var stage *db.StageError
		switch {
		case errors.Is(err, db.ErrNotFound):
			writeProblem(w, r, http.StatusNotFound, "Group not found", "The group does not exist.")
		case errors.Is(err, db.ErrExists):
			writeProblem(w, r, http.StatusConflict, "Group exists", "A group with this name already exists.")
		case errors.Is(err, db.ErrInvalidMember):
			writeProblem(w, r, http.StatusBadRequest, "Invalid member", "A member ID is invalid.")
		case errors.As(err, &stage):
			detail := map[string]string{
				"begin": "Unable to start group update.", "load": "Unable to load the group.", "rename": "Unable to update the group.",
				"replace_members": "Unable to replace group members.", "add_member": "Unable to add the group member.", "commit": "Unable to save group changes.",
			}[stage.Stage]
			if detail == "" {
				detail = "Unable to update the group."
			}
			writeProblem(w, r, http.StatusInternalServerError, "Database error", detail)
		default:
			writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to update the group.")
		}
		return
	}
	s.audit(r.Context(), p.ID, "group.update", "group", id, map[string]any{"name": currentName, "members": members})
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": currentName, "members": members})
}
