package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestSpaceFileDownloadUsesTypedRouteAndDoesNotExecuteRuntime(t *testing.T) {
	content := []byte("space application source")
	file := &auditHubFile{
		repoType:   "spaces",
		name:       "app.py",
		advertised: content,
		served:     content,
	}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file.handler(holder.repoID).ServeHTTP(w, r)
	})
	env := newAuditStorageEnv(t, mux, auditStorageOptions{repoType: "spaces"})
	*holder = *env

	response := env.get(context.Background(), file.name)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), content) {
		t.Fatalf("Space file download: status=%d body=%q, want 200 and source bytes", response.Code, response.Body.String())
	}
	if got := response.Header().Get("X-Repo-Commit"); got != auditStorageCommit {
		t.Fatalf("Space file commit = %q, want %q", got, auditStorageCommit)
	}
	if got := response.Header().Get("X-Cache"); got != "MISS" {
		t.Fatalf("Space file X-Cache = %q, want MISS", got)
	}
}
