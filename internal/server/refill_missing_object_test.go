package server

import (
	"bytes"
	"context"
	"net/http"
	"testing"
)

func TestRefillRestoresMissingBlobObject(t *testing.T) {
	content := []byte("issue-110-" + auditRandomHex(300))
	file := &auditHubFile{name: "refill.txt", advertised: content, served: content}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { file.handler(holder.repoID).ServeHTTP(w, r) })
	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	*holder = *env
	ctx := context.Background()
	if rec := env.get(ctx, file.name); rec.Code != 200 {
		t.Fatalf("initial miss: %d", rec.Code)
	}
	var artifactID, key string
	if err := env.store.SQL.QueryRow(`SELECT a.id,b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, file.name).Scan(&artifactID, &key); err != nil {
		t.Fatal(err)
	}
	if err := env.server.purgeArtifact(ctx, artifactID); err != nil {
		t.Fatal(err)
	}
	// Recreate the state left if object deletion succeeds but deleting the blob
	// row fails before the garbage collection transaction commits.
	if err := env.objects.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	rec := env.get(ctx, file.name)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("refill returned status %d and body length %d; want 200 and %d bytes", rec.Code, rec.Body.Len(), len(content))
	}
}

// A catalog entry whose object disappeared from the bucket is repaired by the
// request that finds out: it gets the file (fetched again) instead of a 502,
// and the object is back for the next ones.
func TestAMissingObjectIsRepairedWhenAFileIsRead(t *testing.T) {
	content := []byte("self-heal-" + auditRandomHex(300))
	file := &auditHubFile{name: "heal.txt", advertised: content, served: content}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { file.handler(holder.repoID).ServeHTTP(w, r) })
	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	*holder = *env
	ctx := context.Background()
	if rec := env.get(ctx, file.name); rec.Code != 200 {
		t.Fatalf("initial miss: %d", rec.Code)
	}
	var key string
	if err := env.store.SQL.QueryRow(`SELECT b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, file.name).Scan(&key); err != nil {
		t.Fatal(err)
	}
	// The catalog still lists the file; the bucket has lost it.
	if err := env.objects.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	rec := env.get(ctx, file.name)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), content) {
		t.Fatalf("read of a file whose object is missing: status %d, %d bytes; want 200 and %d bytes", rec.Code, rec.Body.Len(), len(content))
	}
	if _, err := env.objects.Get(ctx, key, ""); err != nil {
		t.Fatalf("the object was not restored: %v", err)
	}
	var entries int
	if err := env.store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1 AND path=$2`, env.repoPK, file.name).Scan(&entries); err != nil || entries != 1 {
		t.Fatalf("%d catalog entries after the repair (%v), want 1", entries, err)
	}
	if again := env.get(ctx, file.name); again.Code != 200 || !bytes.Equal(again.Body.Bytes(), content) || again.Header().Get("X-Cache") != "HIT" {
		t.Fatalf("the next read: status %d, X-Cache %q; want a HIT", again.Code, again.Header().Get("X-Cache"))
	}
}
