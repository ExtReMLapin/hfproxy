package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

// publishEnv is a proxy with publishing on and a few people to publish with.
type publishEnv struct {
	server                            *Server
	store                             *db.Store
	proxy                             *httptest.Server
	alice, bob, carol, dave, daveRead string // tokens
	aliceID, bobID                    string
}

func (e *publishEnv) token(t *testing.T, username, role, scope string) (id, token string) {
	t.Helper()
	id = db.NewID()
	if _, err := e.store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,$3)`, id, username, role); err != nil {
		t.Fatal(err)
	}
	return id, e.tokenFor(t, id, scope)
}

func (e *publishEnv) tokenFor(t *testing.T, userID, scope string) string {
	t.Helper()
	token := "hfpx_" + randomSecret(32)
	ring := e.server.keyRing()
	if _, err := e.store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY[$4],$5)`, db.NewID(), userID, ring.Hash(token), scope, ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	return token
}

func newPublishEnv(t *testing.T, mutate func(*config.Config)) *publishEnv {
	t.Helper()
	store := auditAPIFreshStore(t)
	objects, _ := ahubNewFakeS3(t)
	cfg := ahubConfig()
	cfg.CustomRepos = true
	if mutate != nil {
		mutate(&cfg)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	s, err := New(ctx, cfg, store, objects, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(s.Handler())
	t.Cleanup(proxy.Close)
	env := &publishEnv{server: s, store: store, proxy: proxy}
	// The links the API hands out point at the proxy itself.
	state := s.runtime.Load()
	next := *state
	next.config.PublicURL = proxy.URL
	s.runtime.Store(&next)

	env.aliceID, env.alice = env.token(t, "alice", "reader", "publish")
	env.bobID, env.bob = env.token(t, "bob", "reader", "publish")
	_, env.carol = env.token(t, "carol", "reader", "publish")
	daveID, dave := env.token(t, "dave", "reader", "publish")
	env.dave, env.daveRead = dave, env.tokenFor(t, daveID, "read")
	groupID := db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO groups(id,name) VALUES($1,'research')`, groupID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.store.SQL.Exec(`INSERT INTO group_members(group_id,user_id) SELECT $1,id FROM users WHERE username IN ('bob','carol')`, groupID); err != nil {
		t.Fatal(err)
	}
	return env
}

func (e *publishEnv) do(t *testing.T, method, path, token string, body any) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		reader = strings.NewReader(b)
	case []byte:
		reader = bytes.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, e.proxy.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if _, isJSON := body.(map[string]any); isJSON {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, data
}

func asMap(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("not a JSON object: %q", data)
	}
	return out
}

func sum(content []byte) string {
	h := sha256.Sum256(content)
	return hex.EncodeToString(h[:])
}

// commitLines builds the JSON lines of a commit request.
func commitLines(summary string, items ...map[string]any) string {
	lines := []string{mustJSON(map[string]any{"key": "header", "value": map[string]any{"summary": summary}})}
	for _, item := range items {
		lines = append(lines, mustJSON(item))
	}
	return strings.Join(lines, "\n") + "\n"
}

func mustJSON(v any) string {
	raw, _ := json.Marshal(v)
	return string(raw)
}

func inlineFile(path string, content []byte) map[string]any {
	return map[string]any{"key": "file", "value": map[string]any{"path": path, "content": base64.StdEncoding.EncodeToString(content), "encoding": "base64"}}
}

func lfsFile(path string, content []byte) map[string]any {
	return map[string]any{"key": "lfsFile", "value": map[string]any{"path": path, "algo": "sha256", "oid": sum(content), "size": len(content)}}
}

// uploadLFS runs the Git LFS exchange for content: batch, upload, verify.
func (e *publishEnv) uploadLFS(t *testing.T, repo, token string, content []byte) {
	t.Helper()
	status, _, body := e.do(t, http.MethodPost, "/"+repo+".git/info/lfs/objects/batch", token,
		map[string]any{"operation": "upload", "transfers": []string{"basic"}, "objects": []map[string]any{{"oid": sum(content), "size": len(content)}}})
	if status != http.StatusOK {
		t.Fatalf("batch = %d %s", status, body)
	}
	object := asMap(t, body)["objects"].([]any)[0].(map[string]any)
	actions, ok := object["actions"].(map[string]any)
	if !ok {
		return // already stored
	}
	href := actions["upload"].(map[string]any)["href"].(string)
	req, _ := http.NewRequest(http.MethodPut, href, bytes.NewReader(content))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("upload = %d", resp.StatusCode)
	}
	verify := actions["verify"].(map[string]any)["href"].(string)
	vreq, _ := http.NewRequest(http.MethodPost, verify, strings.NewReader(mustJSON(map[string]any{"oid": sum(content), "size": len(content)})))
	vreq.Header.Set("Authorization", "Bearer "+token)
	vresp, err := http.DefaultClient.Do(vreq)
	if err != nil {
		t.Fatal(err)
	}
	vresp.Body.Close()
	if vresp.StatusCode != http.StatusOK {
		t.Fatalf("verify = %d", vresp.StatusCode)
	}
}

func TestPublishAModelAndReadItBack(t *testing.T) {
	env := newPublishEnv(t, nil)

	// Create.
	status, _, body := env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "tiny", "organization": "alice", "type": "model"})
	if status != http.StatusOK || asMap(t, body)["url"] != env.proxy.URL+"/alice/tiny" {
		t.Fatalf("create = %d %s", status, body)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "tiny", "organization": "alice"}); status != http.StatusConflict {
		t.Fatalf("creating twice = %d", status)
	}

	// Preupload: a small text file is regular, a big or weights file goes through LFS.
	weights := bytes.Repeat([]byte("w"), 2048)
	status, _, body = env.do(t, http.MethodPost, "/api/models/alice/tiny/preupload/main", env.alice, map[string]any{"files": []map[string]any{
		{"path": "config.json", "size": 30}, {"path": "model.safetensors", "size": len(weights)}, {"path": "huge.txt", "size": 20 << 20},
	}})
	modes := map[string]string{}
	for _, f := range asMap(t, body)["files"].([]any) {
		entry := f.(map[string]any)
		modes[entry["path"].(string)] = entry["uploadMode"].(string)
	}
	if status != http.StatusOK || modes["config.json"] != "regular" || modes["model.safetensors"] != "lfs" || modes["huge.txt"] != "lfs" {
		t.Fatalf("preupload = %d %v", status, modes)
	}

	// Commit a regular file and an LFS file.
	config := []byte(`{"layers": 2}`)
	readme := []byte("---\nlicense: mit\ntags:\n- tiny\npipeline_tag: text-generation\n---\n# Tiny\n")
	env.uploadLFS(t, "alice/tiny", env.alice, weights)
	status, _, body = env.do(t, http.MethodPost, "/api/models/alice/tiny/commit/main", env.alice,
		commitLines("first", inlineFile("config.json", config), inlineFile("README.md", readme), lfsFile("model.safetensors", weights), inlineFile("sub/dir/deep.txt", []byte("deep"))))
	first := asMap(t, body)
	if status != http.StatusOK || first["success"] != true || len(first["commitOid"].(string)) != 40 {
		t.Fatalf("commit = %d %s", status, body)
	}

	// Download: headers, range, conditional, by commit.
	status, header, data := env.do(t, http.MethodGet, "/alice/tiny/resolve/main/config.json", env.alice, nil)
	if status != http.StatusOK || !bytes.Equal(data, config) || header.Get("X-Repo-Commit") != first["commitOid"] || len(strings.Trim(header.Get("ETag"), `"`)) != 40 || header.Get("X-Linked-Etag") != "" {
		t.Fatalf("regular file = %d %q %v", status, data, header)
	}
	status, header, _ = env.do(t, http.MethodHead, "/alice/tiny/resolve/main/model.safetensors", env.alice, nil)
	if status != http.StatusOK || strings.Trim(header.Get("X-Linked-Etag"), `"`) != sum(weights) || header.Get("X-Linked-Size") != fmt.Sprint(len(weights)) || header.Get("Content-Length") != fmt.Sprint(len(weights)) {
		t.Fatalf("LFS file HEAD = %d %v", status, header)
	}
	req, _ := http.NewRequest(http.MethodGet, env.proxy.URL+"/alice/tiny/resolve/main/model.safetensors", nil)
	req.Header.Set("Authorization", "Bearer "+env.alice)
	req.Header.Set("Range", "bytes=5-9")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	ranged, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(ranged) != "wwwww" {
		t.Fatalf("range = %d %q", resp.StatusCode, ranged)
	}
	req, _ = http.NewRequest(http.MethodGet, env.proxy.URL+"/alice/tiny/resolve/"+first["commitOid"].(string)+"/config.json", nil)
	req.Header.Set("Authorization", "Bearer "+env.alice)
	req.Header.Set("If-None-Match", header.Get("ETag"))
	resp, _ = http.DefaultClient.Do(req)
	resp.Body.Close()
	if status, _, _ := env.do(t, http.MethodGet, "/alice/tiny/resolve/"+first["commitOid"].(string)+"/sub/dir/deep.txt", env.alice, nil); status != http.StatusOK {
		t.Fatalf("by commit = %d", status)
	}
	status, header, _ = env.do(t, http.MethodGet, "/alice/tiny/resolve/main/missing.txt", env.alice, nil)
	if status != http.StatusNotFound || header.Get("X-Error-Code") != "EntryNotFound" {
		t.Fatalf("missing file = %d %v", status, header)
	}
	status, header, _ = env.do(t, http.MethodGet, "/alice/tiny/resolve/nope/config.json", env.alice, nil)
	if status != http.StatusNotFound || header.Get("X-Error-Code") != "RevisionNotFound" {
		t.Fatalf("missing revision = %d %v", status, header)
	}

	// Repository description, listings, history.
	status, _, body = env.do(t, http.MethodGet, "/api/models/alice/tiny", env.alice, nil)
	info := asMap(t, body)
	if status != http.StatusOK || info["sha"] != first["commitOid"] || info["private"] != false || len(info["siblings"].([]any)) != 4 {
		t.Fatalf("info = %d %s", status, body)
	}
	tags := fmt.Sprint(info["tags"])
	if !strings.Contains(tags, "license:mit") || !strings.Contains(tags, "tiny") || info["pipeline_tag"] != "text-generation" {
		t.Fatalf("the card's metadata is not in the description: %s", body)
	}
	status, _, body = env.do(t, http.MethodGet, "/api/models/alice/tiny/tree/main?recursive=True", env.alice, nil)
	var tree []map[string]any
	_ = json.Unmarshal(body, &tree)
	paths := map[string]string{}
	for _, e := range tree {
		paths[e["path"].(string)] = e["type"].(string)
	}
	if status != http.StatusOK || paths["sub"] != "directory" || paths["sub/dir"] != "directory" || paths["sub/dir/deep.txt"] != "file" || paths["model.safetensors"] != "file" {
		t.Fatalf("recursive tree = %v", paths)
	}
	status, _, body = env.do(t, http.MethodGet, "/api/models/alice/tiny/tree/main/sub", env.alice, nil)
	_ = json.Unmarshal(body, &tree)
	if status != http.StatusOK || len(tree) != 1 || tree[0]["path"] != "sub/dir" {
		t.Fatalf("subtree = %d %s", status, body)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/api/models/alice/tiny/tree/main/ghost", env.alice, nil); status != http.StatusNotFound {
		t.Fatalf("missing folder = %d", status)
	}
	status, _, body = env.do(t, http.MethodPost, "/api/models/alice/tiny/paths-info/main", env.alice, "paths=config.json&paths=sub&paths=ghost")
	if status != http.StatusOK || strings.Count(string(body), `"path"`) != 2 {
		t.Fatalf("paths-info = %d %s", status, body)
	}

	// Branches and tags, a second commit, deletion.
	if status, _, _ := env.do(t, http.MethodPost, "/api/models/alice/tiny/tag/main", env.alice, map[string]any{"tag": "v1"}); status != http.StatusOK {
		t.Fatalf("tag = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/api/models/alice/tiny/branch/dev", env.alice, map[string]any{"startingPoint": "v1"}); status != http.StatusOK {
		t.Fatalf("branch = %d", status)
	}
	status, _, body = env.do(t, http.MethodPost, "/api/models/alice/tiny/commit/main", env.alice,
		commitLines("second", map[string]any{"key": "deletedFile", "value": map[string]any{"path": "config.json"}}, map[string]any{"key": "deletedFolder", "value": map[string]any{"path": "sub"}},
			map[string]any{"key": "copyFile", "value": map[string]any{"path": "copy.safetensors", "srcPath": "model.safetensors"}}))
	if status != http.StatusOK {
		t.Fatalf("second commit = %d %s", status, body)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/alice/tiny/resolve/main/config.json", env.alice, nil); status != http.StatusNotFound {
		t.Fatalf("deleted file still served = %d", status)
	}
	if status, _, data := env.do(t, http.MethodGet, "/alice/tiny/resolve/v1/config.json", env.alice, nil); status != http.StatusOK || !bytes.Equal(data, config) {
		t.Fatalf("the tag lost the file: %d", status)
	}
	if status, _, data := env.do(t, http.MethodGet, "/alice/tiny/resolve/main/copy.safetensors", env.alice, nil); status != http.StatusOK || !bytes.Equal(data, weights) {
		t.Fatalf("copied file = %d", status)
	}
	status, _, body = env.do(t, http.MethodGet, "/api/models/alice/tiny/refs", env.alice, nil)
	refs := asMap(t, body)
	if status != http.StatusOK || len(refs["branches"].([]any)) != 2 || len(refs["tags"].([]any)) != 1 {
		t.Fatalf("refs = %s", body)
	}
	status, _, body = env.do(t, http.MethodGet, "/api/models/alice/tiny/commits/main", env.alice, nil)
	var commits []map[string]any
	_ = json.Unmarshal(body, &commits)
	if status != http.StatusOK || len(commits) != 3 || commits[0]["title"] != "second" || commits[2]["title"] != "initial commit" {
		t.Fatalf("commits = %s", body)
	}
	if status, _, _ := env.do(t, http.MethodDelete, "/api/models/alice/tiny/tag/v1", env.alice, nil); status != http.StatusOK {
		t.Fatalf("delete tag = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodDelete, "/api/models/alice/tiny/branch/main", env.alice, nil); status != http.StatusBadRequest {
		t.Fatalf("deleting main = %d, want 400", status)
	}

	// Downloads are counted for the repository.
	var downloads int
	_ = env.store.SQL.QueryRow(`SELECT count(*) FROM downloads d JOIN repositories r ON r.id=d.repository_id WHERE r.repo_id='alice/tiny' AND d.status='hit'`).Scan(&downloads)
	if downloads == 0 {
		t.Fatal("downloads of a published repository are not recorded")
	}

	// Delete frees the name and orphans the content.
	if status, _, body := env.do(t, http.MethodDelete, "/api/repos/delete", env.alice, map[string]any{"name": "tiny", "organization": "alice", "type": "model"}); status != http.StatusOK {
		t.Fatalf("delete = %d %s", status, body)
	}
	if status, header, _ := env.do(t, http.MethodGet, "/api/models/alice/tiny", env.alice, nil); status != http.StatusNotFound || header.Get("X-Error-Code") != "RepoNotFound" {
		t.Fatalf("deleted repository = %d %v", status, header)
	}
	var orphaned int
	_ = env.store.SQL.QueryRow(`SELECT count(*) FROM blobs WHERE sha256=$1 AND orphaned_at IS NOT NULL`, sum(weights)).Scan(&orphaned)
	if orphaned != 1 {
		t.Fatal("the content of a deleted repository was not released to the collector")
	}
}

func TestWhoCanPublishWhere(t *testing.T) {
	env := newPublishEnv(t, nil)
	create := func(token, org, name string, private bool) int {
		status, _, _ := env.do(t, http.MethodPost, "/api/repos/create", token, map[string]any{"name": name, "organization": org, "private": private})
		return status
	}
	if create(env.bob, "alice", "x", false) != http.StatusForbidden {
		t.Error("a user created a repository in another user's namespace")
	}
	if create(env.daveRead, "dave", "x", false) != http.StatusForbidden {
		t.Error("a read-only token created a repository")
	}
	if create("", "dave", "x", false) != http.StatusUnauthorized {
		t.Error("an anonymous request created a repository")
	}
	if create(env.alice, "nobody-here", "x", false) != http.StatusForbidden {
		t.Error("a namespace that does not exist was accepted")
	}
	if create(env.alice, "datasets", "x", false) != http.StatusForbidden {
		t.Error("a reserved namespace was accepted")
	}
	for _, name := range []string{"-bad", "bad-", "a--b", "a..b", "model.git", strings.Repeat("a", 97), "with space"} {
		if create(env.alice, "alice", name, false) != http.StatusBadRequest {
			t.Errorf("the name %q was accepted", name)
		}
	}
	if create(env.alice, "alice", "spaced", false) != http.StatusOK {
		t.Fatal("alice cannot publish under her own name")
	}
	if status, _, _ := env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "sp", "organization": "alice", "type": "space"}); status != http.StatusBadRequest {
		t.Errorf("a space was accepted = %d", status)
	}

	// Group namespaces: any member, nobody else.
	if create(env.bob, "research", "shared", true) != http.StatusOK {
		t.Fatal("a group member cannot publish under the group")
	}
	if create(env.dave, "research", "other", false) != http.StatusForbidden {
		t.Error("a non-member published under a group")
	}
	status, _, _ := env.do(t, http.MethodPost, "/api/models/research/shared/commit/main", env.carol, commitLines("by carol", inlineFile("a.txt", []byte("a"))))
	if status != http.StatusOK {
		t.Errorf("another member could not commit = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/api/models/research/shared/commit/main", env.dave, commitLines("by dave", inlineFile("b.txt", []byte("b")))); status != http.StatusForbidden {
		t.Errorf("a non-member committed = %d", status)
	}

	// A private repository is invisible to others and visible to the group.
	if status, header, _ := env.do(t, http.MethodGet, "/api/models/research/shared", env.dave, nil); status != http.StatusForbidden && status != http.StatusNotFound {
		t.Errorf("a private repository answered a stranger with %d %v", status, header)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/research/shared/resolve/main/a.txt", env.dave, nil); status == http.StatusOK {
		t.Error("a private file was served to a stranger")
	}
	if status, _, _ := env.do(t, http.MethodGet, "/research/shared/resolve/main/a.txt", env.carol, nil); status != http.StatusOK {
		t.Errorf("a group member could not read = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodPut, "/api/models/research/shared/settings", env.bob, map[string]any{"private": false}); status != http.StatusOK {
		t.Fatalf("making it public = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodGet, "/research/shared/resolve/main/a.txt", env.dave, nil); status != http.StatusOK {
		t.Errorf("a public file was refused = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodDelete, "/api/repos/delete", env.dave, map[string]any{"name": "shared", "organization": "research"}); status != http.StatusForbidden {
		t.Errorf("a stranger deleted a repository = %d", status)
	}

	// Cookies are not enough: publishing is for bearer tokens (no CSRF surface).
	req := httptest.NewRequest(http.MethodPost, "/api/repos/create", strings.NewReader(`{"name":"viasession","organization":"alice"}`))
	req.AddCookie(&http.Cookie{Name: "hfproxy_session", Value: "whatever"})
	rec := httptest.NewRecorder()
	env.server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a session cookie was accepted for publishing = %d", rec.Code)
	}
}

func TestPublishingCanBeTurnedOff(t *testing.T) {
	env := newPublishEnv(t, func(c *config.Config) { c.CustomRepos = false })
	if status, _, _ := env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "x", "organization": "alice"}); status != http.StatusForbidden {
		t.Fatalf("create while off = %d", status)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/alice/x.git/info/lfs/objects/batch", env.alice, map[string]any{"operation": "upload", "objects": []any{}}); status == http.StatusOK {
		t.Fatal("the LFS batch answered while publishing is off")
	}
}

func TestPublishedContentIsBoundByLimitsAndIntegrity(t *testing.T) {
	env := newPublishEnv(t, func(c *config.Config) { c.UploadMaxFileSize = 100; c.UploadUserQuota = 150 })
	env.do(t, http.MethodPost, "/api/repos/create", env.alice, map[string]any{"name": "small", "organization": "alice"})

	big := bytes.Repeat([]byte("b"), 101)
	status, _, _ := env.do(t, http.MethodPost, "/api/models/alice/small/preupload/main", env.alice, map[string]any{"files": []map[string]any{{"path": "big.bin", "size": len(big)}}})
	if status != http.StatusRequestEntityTooLarge {
		t.Errorf("preupload of a file over the limit = %d", status)
	}
	status, _, body := env.do(t, http.MethodPost, "/alice/small.git/info/lfs/objects/batch", env.alice, map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": sum(big), "size": len(big)}}})
	object := asMap(t, body)["objects"].([]any)[0].(map[string]any)
	if status != http.StatusOK || object["error"] == nil {
		t.Errorf("batch of a file over the limit = %d %s", status, body)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/alice/small.git/info/lfs/objects/batch", env.alice, map[string]any{"operation": "download", "objects": []any{}}); status != http.StatusUnprocessableEntity {
		t.Errorf("a download batch = %d", status)
	}

	// Quota: 150 bytes in total for alice.
	first := bytes.Repeat([]byte("1"), 90)
	second := bytes.Repeat([]byte("2"), 90)
	env.uploadLFS(t, "alice/small", env.alice, first)
	if status, _, body := env.do(t, http.MethodPost, "/api/models/alice/small/commit/main", env.alice, commitLines("one", lfsFile("one.bin", first))); status != http.StatusOK {
		t.Fatalf("first commit = %d %s", status, body)
	}
	status, _, body = env.do(t, http.MethodPost, "/alice/small.git/info/lfs/objects/batch", env.alice, map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": sum(second), "size": len(second)}}})
	if object := asMap(t, body)["objects"].([]any)[0].(map[string]any); status != http.StatusOK || object["error"] == nil {
		t.Errorf("an upload past the quota was offered: %s", body)
	}
	if status, _, _ := env.do(t, http.MethodPost, "/api/models/alice/small/commit/main", env.alice, commitLines("two", inlineFile("two.txt", second))); status != http.StatusRequestEntityTooLarge {
		t.Errorf("an inline file past the quota = %d", status)
	}
	// Content already owned costs nothing again.
	if status, _, body := env.do(t, http.MethodPost, "/api/models/alice/small/commit/main", env.alice, commitLines("again", lfsFile("copy.bin", first))); status != http.StatusOK {
		t.Errorf("re-adding content already held = %d %s", status, body)
	}

	// An upload link only works for what it was made for.
	third := bytes.Repeat([]byte("3"), 10)
	_, _, body = env.do(t, http.MethodPost, "/alice/small.git/info/lfs/objects/batch", env.alice, map[string]any{"operation": "upload", "objects": []map[string]any{{"oid": sum(third), "size": len(third)}}})
	href := asMap(t, body)["objects"].([]any)[0].(map[string]any)["actions"].(map[string]any)["upload"].(map[string]any)["href"].(string)
	put := func(url string, content []byte) int {
		req, _ := http.NewRequest(http.MethodPut, url, bytes.NewReader(content))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if put(href, bytes.Repeat([]byte("x"), 10)) != http.StatusUnprocessableEntity {
		t.Error("content that does not match its hash was stored")
	}
	if put(strings.Replace(href, "/"+sum(third), "/"+sum(first), 1), first) != http.StatusForbidden {
		t.Error("an upload link worked for another object")
	}
	if put(strings.Replace(href, "alice/small", "alice/other", 1), third) == http.StatusOK {
		t.Error("an upload link worked for another repository")
	}
	if put(href, third) != http.StatusOK {
		t.Error("the right content was refused")
	}
}

func TestACardIsValidatedLikeTheHubDoes(t *testing.T) {
	env := newPublishEnv(t, nil)
	check := func(content string) (int, map[string]any) {
		status, _, body := env.do(t, http.MethodPost, "/api/validate-yaml", "", map[string]any{"content": content, "repoType": "model"})
		return status, asMap(t, body)
	}
	if status, out := check("---\nlicense: mit\ntags: [a, b]\n---\n# x\n"); status != http.StatusOK || len(out["errors"].([]any)) != 0 || len(out["warnings"].([]any)) != 0 {
		t.Errorf("a good card = %d %v", status, out)
	}
	if status, out := check("# no metadata\n"); status != http.StatusOK || len(out["warnings"].([]any)) != 1 {
		t.Errorf("a card without metadata = %d %v", status, out)
	}
	if status, out := check("---\nlicense: [unclosed\n---\n"); status != http.StatusBadRequest || len(out["errors"].([]any)) != 1 {
		t.Errorf("broken YAML = %d %v", status, out)
	}
	if status, _ := check("---\nlicense: mit\n"); status != http.StatusBadRequest {
		t.Errorf("an unclosed block = %d", status)
	}
	if status, _ := check("---\n- just\n- a list\n---\n"); status != http.StatusBadRequest {
		t.Errorf("metadata that is not key: value pairs = %d", status)
	}
}
