package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Handler-level tests for the administration API: they check status codes,
// error wording and the shape of responses, on top of the store tests in
// internal/db.

func apiJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("response is not JSON: %q", body)
	}
	return out
}

func TestUsersAPILifecycle(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)
	do := func(method, target, body string) *httptest.ResponseRecorder {
		return auditAPIDoAs(s, admin, method, target, body)
	}

	created := do(http.MethodPost, "/api/v1/users", `{"username":"erin","email":"erin@example.com","password":"a-long-enough-password","role":"operator"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	id := apiJSON(t, created.Body.String())["id"].(string)
	if rec := do(http.MethodPost, "/api/v1/users", `{"username":"erin","password":"another-long-password"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate username = %d, want 409", rec.Code)
	}
	for name, body := range map[string]string{
		"bad username":   `{"username":"no spaces","password":"a-long-enough-password"}`,
		"short password": `{"username":"frank","password":"short"}`,
		"bad role":       `{"username":"frank","password":"a-long-enough-password","role":"root"}`,
		"bad email":      `{"username":"frank","email":"not-an-email","password":"a-long-enough-password"}`,
	} {
		if rec := do(http.MethodPost, "/api/v1/users", body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, rec.Code)
		}
	}
	list := apiJSON(t, do(http.MethodGet, "/api/v1/users?limit=1", "").Body.String())
	if items := list["items"].([]any); len(items) != 1 || list["next_cursor"] == "" {
		t.Fatalf("first page = %v", list)
	}
	next := apiJSON(t, do(http.MethodGet, "/api/v1/users?limit=5&cursor="+list["next_cursor"].(string), "").Body.String())
	// Users are ordered by name: "erin" comes before the administrator "settings-...".
	if first := list["items"].([]any)[0].(map[string]any); first["username"] != "erin" {
		t.Fatalf("first page = %v", list)
	}
	if items := next["items"].([]any); len(items) != 1 || items[0].(map[string]any)["id"] != admin.ID {
		t.Fatalf("second page = %v", next)
	}
	if rec := do(http.MethodGet, "/api/v1/users?cursor=Zm9v", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("garbage cursor = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/v1/users/"+id, `{"status":"disabled"}`); rec.Code != http.StatusOK {
		t.Fatalf("disable = %d %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodPatch, "/api/v1/users/"+id, `{"role":"wizard"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad role patch = %d", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/v1/users/"+id, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/v1/users/does-not-exist", `{"role":"reader"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown user = %d, want 404", rec.Code)
	}
	// The administrator making the call is the only active one.
	if rec := do(http.MethodPatch, "/api/v1/users/"+admin.ID, `{"status":"disabled"}`); rec.Code != http.StatusConflict {
		t.Fatalf("disabling the last administrator = %d, want 409", rec.Code)
	}
	reader := &Principal{ID: id, Username: "erin", Role: "reader", Authenticated: true}
	if rec := auditAPIDoAs(s, reader, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusForbidden {
		t.Fatalf("a reader listing users = %d, want 403", rec.Code)
	}
}

func TestGroupsAPILifecycle(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)
	do := func(method, target, body string) *httptest.ResponseRecorder {
		return auditAPIDoAs(s, admin, method, target, body)
	}
	created := do(http.MethodPost, "/api/v1/groups", `{"name":"ml","members":["`+admin.ID+`"]}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", created.Code, created.Body.String())
	}
	id := apiJSON(t, created.Body.String())["id"].(string)
	if rec := do(http.MethodPost, "/api/v1/groups", `{"name":"ml"}`); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate = %d, want 409", rec.Code)
	}
	if rec := do(http.MethodPost, "/api/v1/groups", `{"name":"ghosts","members":["nobody"]}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown member = %d, want 400", rec.Code)
	}
	if rec := do(http.MethodPost, "/api/v1/groups", `{"name":"  "}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("blank name = %d, want 400", rec.Code)
	}
	detail := apiJSON(t, do(http.MethodGet, "/api/v1/groups/"+id, "").Body.String())
	if members := detail["members"].([]any); detail["name"] != "ml" || len(members) != 1 {
		t.Fatalf("detail = %v", detail)
	}
	if rec := do(http.MethodPatch, "/api/v1/groups/"+id, `{"name":"ml-team","members":[]}`); rec.Code != http.StatusOK {
		t.Fatalf("patch = %d %s", rec.Code, rec.Body.String())
	}
	list := apiJSON(t, do(http.MethodGet, "/api/v1/groups", "").Body.String())
	first := list["items"].([]any)[0].(map[string]any)
	if first["name"] != "ml-team" || first["members"].(float64) != 0 {
		t.Fatalf("list after patch = %v", list)
	}
	if rec := do(http.MethodGet, "/api/v1/groups/does-not-exist", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown group = %d", rec.Code)
	}
	if rec := do(http.MethodPatch, "/api/v1/groups/"+id, `{}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d", rec.Code)
	}
}

func TestRepositoriesGrantsStatsAndAuditAPI(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)
	do := func(p *Principal, method, target, body string) *httptest.ResponseRecorder {
		return auditAPIDoAs(s, p, method, target, body)
	}
	created := do(admin, http.MethodPost, "/api/v1/repos", `{"repo_type":"models","repo_id":"org/private","access_policy":"allowlist"}`)
	if created.Code != http.StatusCreated {
		t.Fatalf("create repo = %d %s", created.Code, created.Body.String())
	}
	repo := apiJSON(t, created.Body.String())["id"].(string)
	if rec := do(admin, http.MethodPost, "/api/v1/repos", `{"repo_type":"nope","repo_id":"x","access_policy":"public"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid repo = %d", rec.Code)
	}
	if rec := do(admin, http.MethodPatch, "/api/v1/repos/"+repo, `{"access_policy":"public"}`); rec.Code != http.StatusOK {
		t.Fatalf("policy change = %d", rec.Code)
	}
	if rec := do(admin, http.MethodPatch, "/api/v1/repos/"+repo, `{"access_policy":"everyone"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid policy = %d", rec.Code)
	}
	if rec := do(admin, http.MethodPatch, "/api/v1/repos/missing", `{"access_policy":"public"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("policy of unknown repo = %d", rec.Code)
	}

	// Grants.
	body := `{"subject_type":"user","subject_id":"` + admin.ID + `","permission":"read"}`
	if rec := do(admin, http.MethodPost, "/api/v1/repos/"+repo+"/grants", body); rec.Code != http.StatusCreated {
		t.Fatalf("grant = %d %s", rec.Code, rec.Body.String())
	}
	for _, permission := range []string{"write", "admin", ""} {
		if rec := do(admin, http.MethodPost, "/api/v1/repos/"+repo+"/grants", `{"subject_type":"user","subject_id":"`+admin.ID+`","permission":"`+permission+`"}`); rec.Code != http.StatusBadRequest {
			t.Fatalf("a %q grant = %d, want 400: the proxy only ever reads", permission, rec.Code)
		}
	}
	if rec := do(admin, http.MethodPost, "/api/v1/repos/"+repo+"/grants", `{"subject_type":"user","subject_id":"ghost","permission":"read"}`); rec.Code != http.StatusNotFound {
		t.Fatalf("grant to an unknown subject = %d, want 404", rec.Code)
	}
	if rec := do(admin, http.MethodPost, "/api/v1/repos/missing/grants", body); rec.Code != http.StatusNotFound {
		t.Fatalf("grant on an unknown repo = %d, want 404", rec.Code)
	}
	grants := apiJSON(t, do(admin, http.MethodGet, "/api/v1/repos/"+repo+"/grants", "").Body.String())["items"].([]any)
	if len(grants) != 1 {
		t.Fatalf("grants = %v", grants)
	}
	if rec := do(admin, http.MethodDelete, "/api/v1/repos/"+repo+"/grants?subject_type=user&subject_id="+admin.ID, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("revoke = %d", rec.Code)
	}

	// Stats and the audit trail reflect what happened.
	stats := apiJSON(t, do(admin, http.MethodGet, "/api/v1/stats", "").Body.String())
	if stats["repositories"].(float64) != 1 {
		t.Fatalf("stats = %v", stats)
	}
	if rec := do(admin, http.MethodGet, "/api/v1/downloads/active", ""); rec.Code != http.StatusOK {
		t.Fatalf("active downloads = %d", rec.Code)
	}
	audit := apiJSON(t, do(admin, http.MethodGet, "/api/v1/audit-events?limit=2", "").Body.String())
	items := audit["items"].([]any)
	if len(items) != 2 || audit["next_cursor"] == "" {
		t.Fatalf("audit first page = %v", audit)
	}
	more := apiJSON(t, do(admin, http.MethodGet, "/api/v1/audit-events?limit=50&cursor="+audit["next_cursor"].(string), "").Body.String())
	seen := map[string]bool{}
	for _, list := range [][]any{items, more["items"].([]any)} {
		for _, item := range list {
			seen[item.(map[string]any)["action"].(string)] = true
		}
	}
	for _, action := range []string{"repository.upsert", "repository.policy.update", "repository.grant.upsert", "repository.grant.delete"} {
		if !seen[action] {
			t.Errorf("the audit log has no %q event (saw %v)", action, seen)
		}
	}
	if rec := do(admin, http.MethodGet, "/api/v1/audit-events?cursor=!!!", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("bad audit cursor = %d", rec.Code)
	}

	// Deleting a repository hides it and removes its grants.
	if rec := do(admin, http.MethodDelete, "/api/v1/repos/"+repo, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d", rec.Code)
	}
	if rec := do(admin, http.MethodGet, "/api/v1/repos/"+repo, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleted repo = %d, want 404", rec.Code)
	}
	if rec := do(admin, http.MethodDelete, "/api/v1/repos/"+repo, ""); rec.Code != http.StatusNotFound {
		t.Fatalf("deleting twice = %d, want 404", rec.Code)
	}
	reader := &Principal{ID: "r", Username: "r", Role: "reader", Authenticated: true}
	if rec := do(reader, http.MethodPost, "/api/v1/repos", `{"repo_type":"models","repo_id":"a/b","access_policy":"public"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("reader creating a repo = %d, want 403", rec.Code)
	}
}

func TestLogoutEndsTheSession(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.AuthCacheTTL = 0
	s := auditAPINewServer(t, store, cfg)
	admin := settingsAdmin(t, store)
	issued := httptest.NewRecorder()
	if _, err := s.createSession(t.Context(), admin.ID, issued, nil); err != nil {
		t.Fatal(err)
	}
	var session, csrf *http.Cookie
	for _, c := range issued.Result().Cookies() {
		switch c.Name {
		case "hfproxy_session":
			session = c
		case "hfproxy_csrf":
			csrf = c
		}
	}
	if session == nil {
		t.Fatal("no session cookie issued")
	}
	check := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		req.AddCookie(session)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	if code := check(); code != http.StatusOK {
		t.Fatalf("a fresh session = %d, want 200", code)
	}
	out := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(""))
	out.AddCookie(session)
	out.AddCookie(csrf)
	// Without the CSRF token a third-party page could log users out.
	forged := httptest.NewRequest(http.MethodPost, "/logout", strings.NewReader(""))
	forged.AddCookie(session)
	forgedRec := httptest.NewRecorder()
	s.Handler().ServeHTTP(forgedRec, forged)
	if forgedRec.Code != http.StatusForbidden {
		t.Fatalf("logout without CSRF token = %d, want 403", forgedRec.Code)
	}
	out.Header.Set("X-CSRF-Token", csrf.Value)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, out)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("logout = %d, want a redirect", rec.Code)
	}
	if code := check(); code == http.StatusOK {
		t.Fatal("the session still works after logout")
	}
}
