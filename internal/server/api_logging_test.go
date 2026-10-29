package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/logging"
)

// lockedBuffer lets a test read what the server logged while requests run.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// logRecords parses the JSON lines logged so far.
func logRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		out = append(out, record)
	}
	return out
}

func findLog(records []map[string]any, msg string) map[string]any {
	for _, record := range records {
		if record["msg"] == msg {
			return record
		}
	}
	return nil
}

func TestRequestLogLevel(t *testing.T) {
	for _, tc := range []struct {
		route  string
		status int
		want   slog.Level
	}{
		{"/api/v1/users", 200, slog.LevelInfo},
		{"/api/v1/users", 201, slog.LevelInfo},
		{"/healthz", 200, slog.LevelDebug},
		{"/readyz", 200, slog.LevelDebug},
		{"/assets/app.css", 200, slog.LevelDebug},
		{"/readyz", 503, slog.LevelError},
		{"/api/v1/me", 401, slog.LevelInfo},
		{"/models/x/resolve/main/config.json", 404, slog.LevelInfo},
		{"/api/v1/users", 400, slog.LevelWarn},
		{"/api/v1/users", 403, slog.LevelWarn},
		{"/login", 429, slog.LevelWarn},
		{"/api/v1/cache/entries", 500, slog.LevelError},
		{"/api/v1/settings/test-hub", 502, slog.LevelError},
	} {
		if got := requestLogLevel(tc.route, tc.status); got != tc.want {
			t.Errorf("%d on %s logged at %v, want %v", tc.status, tc.route, got, tc.want)
		}
	}
}

func TestLogLevelCanBeChangedFromTheConsole(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var out lockedBuffer
	logger, level := logging.New(&out, "info")
	s.logger = logger
	s.SetLogLevelVariable(level)
	admin := settingsAdmin(t, store)

	if view := settingsPayload(t, s, admin); view["log_level"] != "info" {
		t.Fatalf("the settings view must report the level, got %v", view["log_level"])
	}
	if findLog(logRecords(t, out.String()), "request authenticated") != nil {
		t.Fatal("debug lines must not appear at info level")
	}

	view := settingsPayload(t, s, admin)
	view["log_level"] = "debug"
	if status, body := putSettings(t, s, admin, view); status != http.StatusOK || body["log_level"] != "debug" {
		t.Fatalf("PUT log_level=debug = %d %v", status, body)
	}
	if got := logging.Name(level.Level()); got != "debug" {
		t.Fatalf("the running logger is at %q, want debug: the change must not need a restart", got)
	}
	if changed := findLog(logRecords(t, out.String()), "log level changed"); changed == nil || changed["from"] != "info" || changed["to"] != "debug" {
		t.Fatalf("the change must be logged, got %v", changed)
	}
	settingsPayload(t, s, admin)
	if findLog(logRecords(t, out.String()), "request authenticated") == nil {
		t.Fatal("debug lines must appear after switching to debug")
	}

	view["log_level"] = "warning" // accepted alias
	if status, body := putSettings(t, s, admin, view); status != http.StatusOK || body["log_level"] != "warn" {
		t.Fatalf("PUT log_level=warning = %d %v", status, body)
	}
	before := len(out.String())
	settingsPayload(t, s, admin)
	for _, record := range logRecords(t, out.String()[before:]) {
		if record["level"] == "DEBUG" || record["level"] == "INFO" {
			t.Fatalf("a %v line was logged at warn level: %v", record["level"], record)
		}
	}

	view["log_level"] = "verbose"
	if status, _ := putSettings(t, s, admin, view); status != http.StatusBadRequest {
		t.Fatalf("an unknown level = %d, want 400", status)
	}
	if got := s.currentConfig().LogLevel; got != "warn" {
		t.Fatalf("a refused change was applied: %q", got)
	}

	delete(view, "log_level")
	if status, body := putSettings(t, s, admin, view); status != http.StatusOK || body["log_level"] != "warn" {
		t.Fatalf("leaving the level out must keep it, got %d %v", status, body["log_level"])
	}

	saved, err := config.LoadPersisted(t.Context(), store.SQL, auditAPIConfig())
	if err != nil || saved.LogLevel != "warn" {
		t.Fatalf("the level must be saved with the settings: %q %v", saved.LogLevel, err)
	}
}

func TestLockedLogLevelStaysWithTheEnvironment(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.LockedSettings = []string{"log_level"}
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)
	view := settingsPayload(t, s, admin)
	view["log_level"] = "debug"
	if status, _ := putSettings(t, s, admin, view); status != http.StatusConflict {
		t.Fatalf("changing a locked level = %d, want 409", status)
	}
}

func TestServerErrorsAreLoggedWithTheirCauseButNotShownToTheClient(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var out lockedBuffer
	s.logger, _ = logging.New(&out, "info")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users", nil)
	req = req.WithContext(t.Context())
	rec := httptest.NewRecorder()
	s.fail(rec, req, errors.New(`pq: relation "secret_table" does not exist`), http.StatusInternalServerError, "Database error", "Unable to list users.")

	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "secret_table") {
		t.Fatalf("the cause must stay out of the response: %d %s", rec.Code, rec.Body.String())
	}
	record := findLog(logRecords(t, out.String()), "request failed")
	if record == nil || record["level"] != "ERROR" || !strings.Contains(record["error"].(string), "secret_table") {
		t.Fatalf("the cause must be logged at error level, got %v", record)
	}

	out = lockedBuffer{}
	rec = httptest.NewRecorder()
	s.fail(rec, req, errors.New("dial tcp: i/o timeout"), http.StatusBadGateway, "Hugging Face connection failed", "Check the Hub.")
	if record := findLog(logRecords(t, out.String()), "request could not be completed"); record == nil || record["level"] != "WARN" {
		t.Fatalf("an upstream failure is a warning, got %v", record)
	}
}

func TestAccessLogSaysWhyARequestWasRefused(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var out lockedBuffer
	s.logger, _ = logging.New(&out, "info")
	reader := &Principal{ID: "reader-1", Username: "reader", Role: "reader", Authenticated: true}

	if rec := auditAPIDoAs(s, reader, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a reader listing users = %d, want 403", rec.Code)
	}
	records := logRecords(t, out.String())
	denied := findLog(records, "access denied: role not allowed")
	if denied == nil || denied["user_id"] != "reader-1" || denied["level"] != "WARN" {
		t.Fatalf("a denied request must be logged, got %v", denied)
	}
	access := findLog(records, "http request")
	if access == nil || access["level"] != "WARN" || !strings.Contains(access["problem"].(string), "Forbidden") {
		t.Fatalf("the access log must carry the reason, got %v", access)
	}
}

func TestAdministrativeActionsAreLoggedWithoutTypedNames(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	var out lockedBuffer
	s.logger, _ = logging.New(&out, "info")
	admin := settingsAdmin(t, store)

	if rec := auditAPIDoAs(s, admin, http.MethodPost, "/api/v1/users", `{"username":"gary","password":"a-long-enough-password","role":"reader"}`); rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	event := findLog(logRecords(t, out.String()), "audit event")
	if event == nil || event["action"] != "user.create" || event["actor_id"] != admin.ID {
		t.Fatalf("an administrative action must be logged, got %v", event)
	}
	if strings.Contains(out.String(), "a-long-enough-password") {
		t.Fatal("a password reached the log")
	}

	out = lockedBuffer{}
	s.audit(t.Context(), "", "auth.login_failed", "user", "hunter2-typed-as-username", nil)
	if strings.Contains(out.String(), "hunter2") {
		t.Fatalf("the name typed at a failed sign-in must not be logged: %s", out.String())
	}
}
