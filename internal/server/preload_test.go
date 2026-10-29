package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

// operatorToken inserts an operator with a write-scoped token against env's
// database and returns the bearer token.
func operatorToken(t *testing.T, env *ahubEnv, name string) string {
	t.Helper()
	userID := db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'operator')`, userID, name+"-"+userID[:6]); err != nil {
		t.Fatal(err)
	}
	token := "hfpx_" + randomSecret(32)
	ring := env.server.keyRing()
	if _, err := env.store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['write'],$4)`, db.NewID(), userID, ring.Hash(token), ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	return token
}

func preloadStatus(t *testing.T, env *ahubEnv, token string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, env.proxy.URL+"/api/v1/downloads/active", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Preloads []map[string]any `json:"preloads"`
	}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &out); err != nil || len(out.Preloads) == 0 {
		t.Fatalf("no preload in %s", body)
	}
	return out.Preloads[0]
}

func waitForPreload(t *testing.T, env *ahubEnv, token string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		job := preloadStatus(t, env, token)
		if status := job["status"]; status != "queued" && status != "running" {
			return job
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the preload did not finish")
	return nil
}

func TestRepositoryPreloadFetchesEveryFileAcrossPages(t *testing.T) {
	files := map[string]string{"config.json": `{"a":1}`, "tokenizer.json": `{"b":2}`, "sub/vocab.txt": "hello"}
	var treeCalls atomic.Int32
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/tree/main"):
			treeCalls.Add(1)
			w.Header().Set("X-Repo-Commit", ahubCommit)
			if r.URL.Query().Get("cursor") == "" {
				w.Header().Set("Link", `<https://huggingface.co/api/models/x/tree/main?cursor=page2>; rel="next"`)
				fmt.Fprint(w, `[{"type":"file","path":"config.json"},{"type":"directory","path":"sub"},{"type":"file","path":"tokenizer.json"}]`)
				return
			}
			fmt.Fprint(w, `[{"type":"file","path":"sub/vocab.txt"}]`)
		default:
			for name, content := range files {
				if strings.HasSuffix(r.URL.Path, "/"+name) {
					w.Header().Set("X-Repo-Commit", ahubCommit)
					w.Header().Set("ETag", `"`+name+`"`)
					w.Header().Set("Content-Length", fmt.Sprint(len(content)))
					if r.Method == http.MethodGet {
						io.WriteString(w, content)
					}
					return
				}
			}
			http.NotFound(w, r)
		}
	})
	env := ahubNewEnv(t, hub)
	token := operatorToken(t, env, "preloader")

	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/api/v1/repos/"+env.repoPK+"/preload", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("start preload = %d, want 202", resp.StatusCode)
	}
	job := waitForPreload(t, env, token)
	if job["status"] != "completed" || job["files_total"].(float64) != 3 || job["files_processed"].(float64) != 3 || job["failed_files"].(float64) != 0 {
		t.Fatalf("preload job = %v", job)
	}
	if treeCalls.Load() != 2 {
		t.Fatalf("the file listing was fetched %d times, want 2 pages", treeCalls.Load())
	}
	var cached int
	env.store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1`, env.repoPK).Scan(&cached)
	if cached != 3 {
		t.Fatalf("%d files cached, want 3", cached)
	}
}

func TestRepositoryPreloadReportsAnUnreachableRepository(t *testing.T) {
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	token := operatorToken(t, env, "preloader")
	req, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/api/v1/repos/"+env.repoPK+"/preload", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	job := waitForPreload(t, env, token)
	if job["status"] != "failed" || !strings.Contains(fmt.Sprint(job["error"]), "not found or is not accessible") {
		t.Fatalf("job = %v", job)
	}

	// An anonymous caller cannot start a preload, and an unknown repository is a 404.
	denied, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/api/v1/repos/"+env.repoPK+"/preload", nil)
	if resp, err := http.DefaultClient.Do(denied); err != nil || (resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden) {
		t.Fatalf("anonymous preload = %v %v, want 401 or 403", resp.StatusCode, err)
	}
	missing, _ := http.NewRequest(http.MethodPost, env.proxy.URL+"/api/v1/repos/does-not-exist/preload", nil)
	missing.Header.Set("Authorization", "Bearer "+token)
	if resp, err := http.DefaultClient.Do(missing); err != nil || resp.StatusCode != http.StatusNotFound {
		t.Fatalf("preload of an unknown repository = %v %v, want 404", resp, err)
	}
}
