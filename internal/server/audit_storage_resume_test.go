package server

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestInterruptedDownloadResumesFromLastCompleteBlock(t *testing.T) {
	if os.Getenv("HFPROXY_TEST_DATABASE_URL") == "" || os.Getenv("HFPROXY_TEST_S3_ENDPOINT") == "" {
		t.Skip("HFPROXY_TEST_DATABASE_URL and HFPROXY_TEST_S3_ENDPOINT are required")
	}
	content := make([]byte, downloadResumeChunkSize+1024)
	for i := range content {
		content[i] = byte(i % 251)
	}
	oid := auditSHA256(content)
	var holder *auditStorageEnv
	var secondBlockAttempts atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		resolvePath := "/" + holder.repoID + "/resolve/" + auditStorageCommit + "/resume.bin"
		cdnPath := "/cdn/" + oid
		switch r.URL.Path {
		case resolvePath:
			w.Header().Set("X-Repo-Commit", auditStorageCommit)
			w.Header().Set("X-Linked-Etag", `"`+oid+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", cdnPath)
			w.WriteHeader(http.StatusFound)
		case cdnPath:
			w.Header().Set("ETag", `"resume-etag"`)
			w.Header().Set("Content-Type", "application/octet-stream")
			if r.Method == http.MethodHead {
				w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				w.WriteHeader(http.StatusOK)
				return
			}
			start, end, partial, valid := parseByteRange(r.Header.Get("Range"), int64(len(content)))
			if !partial || !valid {
				http.Error(w, "missing test range", http.StatusBadRequest)
				return
			}
			body := content[start : end+1]
			if start == downloadResumeChunkSize && secondBlockAttempts.Add(1) == 1 {
				body = body[:len(body)/2]
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(content)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	})

	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	holder = env
	first := env.get(context.Background(), "resume.bin")
	if first.Code != http.StatusBadGateway {
		t.Fatalf("interrupted download status = %d, want 502", first.Code)
	}

	var completed int64
	if err := env.store.SQL.QueryRow(`SELECT completed_bytes FROM download_resumptions WHERE repository_id=$1 AND commit_sha=$2 AND path=$3`, env.repoPK, auditStorageCommit, "resume.bin").Scan(&completed); err != nil {
		t.Fatalf("read resumable progress: %v", err)
	}
	if completed != downloadResumeChunkSize {
		t.Fatalf("completed bytes = %d, want %d", completed, downloadResumeChunkSize)
	}

	second := env.get(context.Background(), "resume.bin")
	if second.Code != http.StatusOK || !bytes.Equal(second.Body.Bytes(), content) {
		t.Fatalf("resumed download: status=%d body length=%d, want 200 and %d bytes", second.Code, second.Body.Len(), len(content))
	}
	if secondBlockAttempts.Load() != 2 {
		t.Fatalf("second block attempts = %d, want one failed request and one resumed request", secondBlockAttempts.Load())
	}

	var remaining int
	if err := env.store.SQL.QueryRow(`SELECT count(*) FROM download_resumptions WHERE repository_id=$1 AND commit_sha=$2 AND path=$3`, env.repoPK, auditStorageCommit, "resume.bin").Scan(&remaining); err != nil {
		t.Fatalf("check resumable cleanup: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("resumable state rows after completion = %d, want 0", remaining)
	}

	var storageKey string
	if err := env.store.SQL.QueryRow(`SELECT b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.path=$2`, env.repoPK, "resume.bin").Scan(&storageKey); err != nil {
		t.Fatalf("read completed artifact: %v", err)
	}
	stored, err := env.objects.Get(context.Background(), storageKey, "")
	if err != nil {
		t.Fatalf("read completed blob: %v", err)
	}
	defer stored.Body.Close()
	actual, err := io.ReadAll(stored.Body)
	if err != nil {
		t.Fatalf("read completed blob body: %v", err)
	}
	if !bytes.Equal(actual, content) {
		t.Fatalf("completed blob was corrupted: got %d bytes, want %d", len(actual), len(content))
	}
	if strings.TrimSpace(second.Header().Get("ETag")) == "" {
		t.Fatal("resumed response did not include an ETag")
	}
}
