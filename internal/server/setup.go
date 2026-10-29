package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

// First-run setup: while no active administrator exists, the console offers to
// create one. Because a fresh instance may already be reachable from a network,
// the page asks for a setup code that only the operator can read: it is printed
// when the service starts, or fixed in advance with HFPROXY_SETUP_TOKEN.

// adminKnownTTL is how long "an administrator exists" is remembered, so the
// public status endpoint does not query the database on every call. An
// administrator can only disappear through direct database changes.
const adminKnownTTL = 30 * time.Second

// setupRequired reports whether the instance still needs its first administrator.
func (s *Server) setupRequired(r *http.Request) (bool, error) {
	if until := s.adminKnownUntil.Load(); until > time.Now().UnixNano() {
		return false, nil
	}
	exists, err := s.db.HasActiveAdmin(r.Context())
	if err != nil {
		return false, err
	}
	if exists {
		s.adminKnownUntil.Store(time.Now().Add(adminKnownTTL).UnixNano())
	}
	return !exists, nil
}

// setupCode returns the code that authorises the first administrator's
// creation. A random one is generated on first use and announced once.
func (s *Server) setupCode() string {
	if configured := s.currentConfig().SetupToken; configured != "" {
		return configured
	}
	s.setupMu.Lock()
	defer s.setupMu.Unlock()
	if s.setupGenerated == "" {
		s.setupGenerated = randomSecret(16)
		s.announceSetupCode(true)
	}
	return s.setupGenerated
}

// announceSetup prints how to create the first administrator when none exists.
// It goes to the process output rather than through the log level, because an
// operator who lowered the level to "error" still needs the code.
func (s *Server) announceSetup(ctx context.Context) {
	exists, err := s.db.HasActiveAdmin(ctx)
	if err != nil || exists {
		return
	}
	if s.currentConfig().SetupToken != "" {
		s.setupMu.Lock()
		s.announceSetupCode(false)
		s.setupMu.Unlock()
		return
	}
	s.setupCode()
}

// announceSetupCode writes the banner. The caller holds setupMu.
func (s *Server) announceSetupCode(generated bool) {
	out := s.setupOut
	if out == nil {
		return
	}
	address := s.currentConfig().PublicURL
	if address == "" {
		address = "http://" + s.currentConfig().ListenAddr
	}
	code := "(the value of HFPROXY_SETUP_TOKEN)"
	if generated {
		code = s.setupGenerated
	}
	fmt.Fprintf(out, "\nhfproxy: no administrator exists yet.\n  Open %s and enter this setup code to create the first one:\n\n      %s\n\n  Or from a shell: hfproxy admin create-user --username NAME --password-file PATH\n\n", address, code)
}

// apiSetup serves the two public setup endpoints: GET /api/v1/setup/status and
// POST /api/v1/setup. Neither needs a session, which is why they are routed
// before the CSRF check: the setup code plays that part for the POST.
func (s *Server) apiSetup(w http.ResponseWriter, r *http.Request, path string) {
	required, err := s.setupRequired(r)
	if err != nil {
		s.logger.Error("setup status could not be read", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to check whether setup is needed.")
		return
	}
	if required {
		s.setupCode() // makes sure the operator has been given a code
	}
	switch {
	case path == "setup/status":
		if r.Method != http.MethodGet {
			methodNotAllowedWith(w, "GET")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"required": required})
	case path == "setup":
		if r.Method != http.MethodPost {
			methodNotAllowedWith(w, "POST")
			return
		}
		s.createFirstAdmin(w, r, required)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) createFirstAdmin(w http.ResponseWriter, r *http.Request, required bool) {
	if !required {
		writeProblem(w, r, http.StatusConflict, "Setup already completed", "An administrator already exists. Sign in instead.")
		return
	}
	// One shared budget for every guess of the code, whoever makes it.
	if retryAfter, ok := s.allowLoginAttempt(r, "setup"); !ok {
		s.logger.Warn("setup refused: too many attempts", "request_id", requestID(r.Context()), "client_ip", s.loginClientIP(r), "retry_after_seconds", retryAfter)
		w.Header().Set("Retry-After", fmt.Sprint(retryAfter))
		writeProblem(w, r, http.StatusTooManyRequests, "Too many attempts", "Retry later.")
		return
	}
	var input struct {
		SetupCode   string `json:"setup_code"`
		Username    string `json:"username"`
		Email       string `json:"email"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if !decodeJSON(w, r, &input) {
		return
	}
	if !constantEqual(strings.TrimSpace(input.SetupCode), s.setupCode()) {
		s.recordLoginFailure(r.Context(), "setup")
		s.logger.Warn("setup refused: wrong setup code", "request_id", requestID(r.Context()), "client_ip", s.loginClientIP(r))
		writeProblem(w, r, http.StatusForbidden, "Invalid setup code", "Enter the setup code printed in the service's output when it started, or the value of HFPROXY_SETUP_TOKEN.")
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	input.Email = strings.TrimSpace(input.Email)
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if title, detail := newUserProblem(input.Username, input.Email, input.DisplayName); title != "" {
		writeProblem(w, r, http.StatusBadRequest, title, detail)
		return
	}
	hash, err := HashPassword(input.Password)
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid password", "Password must contain 12 to 1024 bytes.")
		return
	}
	id := db.NewID()
	switch err := s.db.CreateFirstAdmin(r.Context(), id, input.Username, input.Email, input.DisplayName, hash); {
	case errors.Is(err, db.ErrSetupDone):
		writeProblem(w, r, http.StatusConflict, "Setup already completed", "An administrator already exists. Sign in instead.")
		return
	case errors.Is(err, db.ErrExists):
		writeProblem(w, r, http.StatusConflict, "Username already exists", "Choose a different username.")
		return
	case err != nil:
		s.logger.Error("the first administrator could not be created", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
		writeProblem(w, r, http.StatusInternalServerError, "Database error", "Unable to create the administrator.")
		return
	}
	s.adminKnownUntil.Store(time.Now().Add(adminKnownTTL).UnixNano())
	s.audit(r.Context(), id, "setup.first_admin", "user", id, map[string]any{"username": input.Username})
	s.logger.Info("first administrator created from the console", "request_id", requestID(r.Context()), "user_id", id, "client_ip", s.loginClientIP(r))
	// Sign the new administrator in, so the console opens directly.
	if _, err := s.createSession(r.Context(), id, w, r); err != nil {
		s.logger.Warn("the first administrator was created but could not be signed in", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 200))
	}
	writeJSON(w, http.StatusCreated, map[string]string{"id": id, "username": input.Username, "role": "admin"})
}
