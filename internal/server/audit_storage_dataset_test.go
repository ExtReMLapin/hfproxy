package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestDatasetParquetShardDownloadUsesTypedRouteAndCache(t *testing.T) {
	content := bytes.Repeat([]byte("parquet-shard"), 1024)
	file := &auditHubFile{
		repoType:   "datasets",
		name:       "data/train-00000-of-00001.parquet",
		advertised: content,
		served:     content,
	}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file.handler(holder.repoID).ServeHTTP(w, r)
	})
	env := newAuditStorageEnv(t, mux, auditStorageOptions{repoType: "datasets"})
	*holder = *env

	first := env.get(context.Background(), file.name)
	if first.Code != http.StatusOK || !bytes.Equal(first.Body.Bytes(), content) {
		t.Fatalf("dataset shard download: status=%d body length=%d, want 200 and %d bytes", first.Code, first.Body.Len(), len(content))
	}
	if got := first.Header().Get("X-Repo-Commit"); got != auditStorageCommit {
		t.Fatalf("dataset shard commit = %q, want %q", got, auditStorageCommit)
	}

	second := env.get(context.Background(), file.name)
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), content) {
		t.Fatalf("cached dataset shard: status=%d body length=%d, want 200 and %d bytes", second.Code, second.Body.Len(), len(content))
	}
	if got := second.Header().Get("X-Cache"); got != "HIT" {
		t.Fatalf("cached dataset shard X-Cache = %q, want HIT", got)
	}
}
