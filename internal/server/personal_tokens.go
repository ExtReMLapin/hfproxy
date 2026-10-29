package server

import (
	"errors"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/hfproxy/hfproxy/internal/db"
)

type personalToken struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scopes     []string   `json:"scopes"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// Personal credentials are managed using a browser session, never another token.
func (s *Server) apiPersonalTokens(w http.ResponseWriter, r *http.Request, suffix string) {
	w.Header().Set("Cache-Control", "no-store")
	p, err := s.authenticate(r)
	if err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to manage your access tokens.")
		return
	}
	if p.TokenID != "" || r.Header.Get("Authorization") != "" {
		writeProblem(w, r, http.StatusForbidden, "Browser session required", "Sign in to manage your access tokens.")
		return
	}
	if suffix != "" {
		id := strings.TrimPrefix(suffix, "/")
		if id == "" || strings.Contains(id, "/") {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This access token does not exist.")
			return
		}
		if r.Method != http.MethodDelete {
			methodNotAllowedWith(w, "DELETE")
			return
		}
		revoked, err := s.db.RevokeToken(r.Context(), p.ID, id)
		if err != nil {
			s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to revoke this token.")
			return
		}
		if !revoked {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This access token does not exist.")
			return
		}
		s.authLookups().purge()
		s.audit(r.Context(), p.ID, "token.revoked", "access_token", id, nil)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	switch r.Method {
	case http.MethodGet:
		tokens, err := s.db.ListTokens(r.Context(), p.ID)
		if err != nil {
			s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to load your tokens.")
			return
		}
		items := make([]personalToken, 0, len(tokens))
		for _, t := range tokens {
			items = append(items, personalToken{ID: t.ID, Name: t.Name, Prefix: t.Prefix, Scopes: t.Scopes, CreatedAt: t.CreatedAt, ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt})
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	case http.MethodPost:
		var input struct {
			Name          string `json:"name"`
			ExpiresInDays int    `json:"expires_in_days"`
		}
		if !decodeJSON(w, r, &input) {
			return
		}
		input.Name = strings.TrimSpace(input.Name)
		if input.Name == "" || utf8.RuneCountInString(input.Name) > 100 || input.ExpiresInDays < 1 || input.ExpiresInDays > 365 {
			writeProblem(w, r, http.StatusBadRequest, "Invalid token", "Provide a name of 1–100 characters and an expiration of 1–365 days.")
			return
		}
		secret := "hfpx_" + randomSecret(32)
		expires := time.Now().UTC().Add(time.Duration(input.ExpiresInDays) * 24 * time.Hour)
		item := personalToken{ID: db.NewID(), Name: input.Name, Prefix: secret[:16], Scopes: []string{"read"}, ExpiresAt: &expires}
		ring := s.keyRing()
		stored := db.AccessToken{ID: item.ID, Name: item.Name, Prefix: item.Prefix, Scopes: item.Scopes, ExpiresAt: item.ExpiresAt}
		if err := s.db.CreateToken(r.Context(), p.ID, &stored, ring.Hash(secret), ring.KeyID(), 100); err != nil {
			switch {
			case errors.Is(err, db.ErrInactiveUser):
				writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to create a token.")
			case errors.Is(err, db.ErrTokenLimit):
				writeProblem(w, r, http.StatusConflict, "Token limit reached", "Revoke an existing token before creating another (maximum 100).")
			default:
				s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to create a token.")
			}
			return
		}
		item.CreatedAt = stored.CreatedAt
		s.audit(r.Context(), p.ID, "token.created", "access_token", item.ID, map[string]any{"name": item.Name, "scopes": item.Scopes})
		writeJSON(w, http.StatusCreated, struct {
			personalToken
			Token string `json:"token"`
		}{item, secret})
	default:
		methodNotAllowedWith(w, "GET, POST")
	}
}
