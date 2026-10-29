package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
)

// consoleCall sends a request the way the console does: with a browser session.
func (e *publishEnv) consoleCall(t *testing.T, cookies []*http.Cookie, method, path string, body any, header http.Header) (int, map[string]any) {
	t.Helper()
	var payload string
	switch b := body.(type) {
	case nil:
	case string:
		payload = b
	default:
		raw, _ := json.Marshal(b)
		payload = string(raw)
	}
	h := http.Header{}
	for k, v := range header {
		h[k] = v
	}
	if h.Get("Content-Type") == "" && payload != "" {
		h.Set("Content-Type", "application/json")
	}
	rec := auditAuthDo(e.server, method, path, payload, cookies, h)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func multipartUpload(files map[string]string) (string, http.Header) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for name, content := range files {
		part, _ := writer.CreateFormFile("files", name)
		part.Write([]byte(content))
	}
	writer.Close()
	return body.String(), http.Header{"Content-Type": {writer.FormDataContentType()}}
}

func TestTheConsoleManagesPublishedRepositories(t *testing.T) {
	env := newPublishEnv(t, nil)
	aliceCookies := auditAuthSession(t, env.server, env.aliceID)
	bobCookies := auditAuthSession(t, env.server, env.bobID)
	var daveID string
	_ = env.store.SQL.QueryRow(`SELECT id FROM users WHERE username='dave'`).Scan(&daveID)
	daveCookies := auditAuthSession(t, env.server, daveID)

	status, out := env.consoleCall(t, aliceCookies, http.MethodPost, "/api/v1/published", map[string]any{"repo_type": "model", "name": "console-model", "private": true}, nil)
	if status != http.StatusCreated || out["repo_id"] != "alice/console-model" || out["private"] != true || out["can_write"] != true {
		t.Fatalf("create = %d %v", status, out)
	}
	id := out["id"].(string)
	if status, out := env.consoleCall(t, aliceCookies, http.MethodPost, "/api/v1/published", map[string]any{"repo_type": "model", "name": "console-model"}, nil); status != http.StatusConflict {
		t.Fatalf("creating twice = %d %v", status, out)
	}
	if status, _ := env.consoleCall(t, bobCookies, http.MethodPost, "/api/v1/published", map[string]any{"repo_type": "dataset", "namespace": "alice", "name": "mine"}, nil); status != http.StatusForbidden {
		t.Errorf("publishing under someone else's name = %d", status)
	}
	if status, _ := env.consoleCall(t, bobCookies, http.MethodPost, "/api/v1/published", map[string]any{"repo_type": "dataset", "namespace": "research", "name": "corpus"}, nil); status != http.StatusCreated {
		t.Errorf("publishing under a group = %d", status)
	}

	// Upload files, with a folder, through a multipart form.
	body, header := multipartUpload(map[string]string{"README.md": "---\nlicense: apache-2.0\n---\n# Console model\nHello.\n", "weights/tiny.bin": "binary"})
	status, out = env.consoleCall(t, aliceCookies, http.MethodPost, "/api/v1/published/"+id+"/files?message=hi&directory=v1", body, header)
	if status != http.StatusCreated || out["files"] != float64(2) {
		t.Fatalf("upload = %d %v", status, out)
	}
	if status, out := env.consoleCall(t, daveCookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status != http.StatusNotFound {
		t.Errorf("a stranger uploaded to a private repository = %d %v", status, out)
	}

	// The same files are visible through the Hub API, the README as the card.
	status, _, data := env.do(t, http.MethodGet, "/alice/console-model/resolve/main/v1/weights/tiny.bin", env.alice, nil)
	if status != http.StatusOK || string(data) != "binary" {
		t.Fatalf("download of an uploaded file = %d %q", status, data)
	}
	status, out = env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id, nil, nil)
	if status != http.StatusOK {
		t.Fatalf("detail = %d %v", status, out)
	}
	if out["readme"] != nil {
		t.Errorf("a README in a folder was taken for the repository's card: %v", out["readme"])
	}
	// Only the README at the root is the card.
	body, header = multipartUpload(map[string]string{"README.md": "---\nlicense: apache-2.0\ntags: [console]\n---\n# Console model\nHello.\n"})
	if status, out := env.consoleCall(t, aliceCookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status != http.StatusCreated {
		t.Fatalf("upload of the card = %d %v", status, out)
	}
	status, out = env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id, nil, nil)
	if readme, _ := out["readme"].(map[string]any); status != http.StatusOK || readme == nil || !strings.Contains(fmt.Sprint(readme["content"]), "Hello.") || strings.Contains(fmt.Sprint(readme["content"]), "license:") || !strings.Contains(fmt.Sprint(readme["card"]), "license:apache-2.0") {
		t.Errorf("readme = %v", out["readme"])
	}

	// Tree and history.
	status, out = env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id+"/tree", nil, nil)
	items := out["items"].([]any)
	if status != http.StatusOK || len(items) != 2 || items[0].(map[string]any)["name"] != "v1" || items[0].(map[string]any)["type"] != "directory" || items[1].(map[string]any)["name"] != "README.md" {
		t.Fatalf("tree = %d %v", status, out)
	}
	status, out = env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id+"/tree?path=v1/weights", nil, nil)
	if items := out["items"].([]any); status != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["path"] != "v1/weights/tiny.bin" {
		t.Fatalf("subtree = %d %v", status, out)
	}
	if status, _ := env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id+"/tree?path=nope", nil, nil); status != http.StatusNotFound {
		t.Errorf("missing folder = %d", status)
	}
	status, out = env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id+"/commits", nil, nil)
	if commits := out["items"].([]any); status != http.StatusOK || len(commits) != 3 || commits[2].(map[string]any)["summary"] != "initial commit" || commits[1].(map[string]any)["summary"] != "hi" {
		t.Fatalf("commits = %d %v", status, out)
	}

	// Delete a folder, change visibility, list, delete the repository.
	status, out = env.consoleCall(t, aliceCookies, http.MethodPost, "/api/v1/published/"+id+"/delete-files", map[string]any{"paths": []string{"v1"}}, nil)
	if status != http.StatusOK {
		t.Fatalf("delete-files = %d %v", status, out)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/alice/console-model/resolve/main/v1/weights/tiny.bin", env.alice, nil); status != http.StatusNotFound {
		t.Errorf("deleted folder still served = %d", status)
	}
	if status, _ := env.consoleCall(t, daveCookies, http.MethodPatch, "/api/v1/published/"+id, map[string]any{"private": false}, nil); status != http.StatusNotFound {
		t.Errorf("a stranger changed the visibility = %d", status)
	}
	if status, _ := env.consoleCall(t, aliceCookies, http.MethodPatch, "/api/v1/published/"+id, map[string]any{"private": false}, nil); status != http.StatusOK {
		t.Fatalf("making it public = %d", status)
	}
	status, out = env.consoleCall(t, daveCookies, http.MethodGet, "/api/v1/published", nil, nil)
	listed := map[string]bool{}
	for _, item := range out["items"].([]any) {
		listed[item.(map[string]any)["repo_id"].(string)] = item.(map[string]any)["can_write"].(bool)
	}
	if status != http.StatusOK || listed["alice/console-model"] != false || len(listed) != 2 {
		t.Errorf("the list seen by another user = %v", listed)
	}
	if status, _ := env.consoleCall(t, daveCookies, http.MethodDelete, "/api/v1/published/"+id, nil, nil); status != http.StatusForbidden {
		t.Errorf("a reader deleted a public repository = %d", status)
	}
	if status, _ := env.consoleCall(t, aliceCookies, http.MethodDelete, "/api/v1/published/"+id, nil, nil); status != http.StatusNoContent {
		t.Fatalf("delete = %d", status)
	}
	if status, _ := env.consoleCall(t, aliceCookies, http.MethodGet, "/api/v1/published/"+id, nil, nil); status != http.StatusNotFound {
		t.Errorf("a deleted repository = %d", status)
	}

	// Without a session there is nothing to see or do.
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/published", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("anonymous list = %d", rec.Code)
	}
}

func TestConsoleUploadsRespectTheLimits(t *testing.T) {
	env := newPublishEnv(t, func(c *config.Config) { c.UploadMaxFileSize = 10; c.UploadUserQuota = 15 })
	cookies := auditAuthSession(t, env.server, env.aliceID)
	_, out := env.consoleCall(t, cookies, http.MethodPost, "/api/v1/published", map[string]any{"repo_type": "dataset", "name": "limits"}, nil)
	id := out["id"].(string)

	body, header := multipartUpload(map[string]string{"big.txt": "01234567890"})
	if status, _ := env.consoleCall(t, cookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status != http.StatusRequestEntityTooLarge {
		t.Errorf("a file over the limit = %d", status)
	}
	body, header = multipartUpload(map[string]string{"a.txt": "0123456789"})
	if status, out := env.consoleCall(t, cookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status != http.StatusCreated {
		t.Fatalf("a file within the limit = %d %v", status, out)
	}
	body, header = multipartUpload(map[string]string{"b.txt": "abcdefghij"})
	if status, _ := env.consoleCall(t, cookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an upload over the quota = %d", status)
	}
	for _, name := range []string{"../escape.txt", "a/.git/x", ""} {
		body, header = multipartUpload(map[string]string{name: "x"})
		if status, _ := env.consoleCall(t, cookies, http.MethodPost, "/api/v1/published/"+id+"/files", body, header); status == http.StatusCreated {
			t.Errorf("the file name %q was accepted", name)
		}
	}
	// A request without the session's CSRF token is refused.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/published/"+id+"/files", strings.NewReader(body))
	for _, c := range cookies {
		req.AddCookie(c)
	}
	req.Header.Set("Content-Type", header.Get("Content-Type"))
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("an upload without the CSRF token = %d", rec.Code)
	}
}
