package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRangeCacheHitReturns206416ContentRangeAndETag(t *testing.T) {
	content := []byte("0123456789")
	file := &auditHubFile{name: "range.bin", advertised: content, served: content}
	holder := &auditStorageEnv{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		file.handler(holder.repoID).ServeHTTP(w, r)
	})
	env := newAuditStorageEnv(t, mux, auditStorageOptions{})
	*holder = *env

	if response := env.get(context.Background(), file.name); response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), content) {
		t.Fatalf("initial cache fill: status=%d body=%q", response.Code, response.Body.String())
	}

	request := func(rangeValue string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/"+env.repoID+"/resolve/"+auditStorageCommit+"/"+file.name, nil)
		req.Header.Set("Range", rangeValue)
		rec := httptest.NewRecorder()
		env.server.resolveFile(rec, req)
		return rec
	}

	digest := sha256.Sum256(content)
	wantETag := fmt.Sprintf(`"%x"`, digest)
	partial := request("bytes=2-5")
	if partial.Code != http.StatusPartialContent || partial.Body.String() != "2345" {
		t.Fatalf("partial response: status=%d body=%q", partial.Code, partial.Body.String())
	}
	if got := partial.Header().Get("Content-Range"); got != "bytes 2-5/10" {
		t.Fatalf("partial Content-Range = %q", got)
	}
	if got := partial.Header().Get("ETag"); got != wantETag {
		t.Fatalf("partial ETag = %q, want %q", got, wantETag)
	}
	if got := partial.Header().Get("Content-Length"); got != "4" {
		t.Fatalf("partial Content-Length = %q, want 4", got)
	}

	unsatisfiable := request("bytes=10-")
	if unsatisfiable.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("unsatisfiable status = %d, want 416", unsatisfiable.Code)
	}
	if got := unsatisfiable.Header().Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("unsatisfiable Content-Range = %q", got)
	}
	if got := unsatisfiable.Header().Get("ETag"); got != wantETag {
		t.Fatalf("unsatisfiable ETag = %q, want %q", got, wantETag)
	}

	multipart := request("bytes=0-1,4-5")
	if multipart.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("multipart status = %d, want 416", multipart.Code)
	}
	if got := multipart.Header().Get("Content-Range"); got != "bytes */10" {
		t.Fatalf("multipart Content-Range = %q", got)
	}
}
