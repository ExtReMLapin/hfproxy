package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrentCacheMissPublishesOneArtifactAndBlob(t *testing.T) {
	content := bytes.Repeat([]byte("concurrent-model"), 4096)
	digest := auditSHA256(content)
	var getCount atomic.Int32
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/concurrent/model.bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		if r.Host != "cdn-lfs.hf.co" || r.URL.Path != "/concurrent/model.bin" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `"concurrent-cdn"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			getCount.Add(1)
			time.Sleep(50 * time.Millisecond)
			_, _ = w.Write(content)
		}
	})
	env := ahubNewEnv(t, hub)

	const workers = 8
	start := make(chan struct{})
	responses := make(chan []byte, workers)
	errors := make(chan error, workers)
	var group sync.WaitGroup
	for i := 0; i < workers; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			response, err := http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/model.bin")
			if err != nil {
				errors <- err
				return
			}
			body, readErr := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusOK {
				errors <- fmt.Errorf("unexpected HTTP status %d", response.StatusCode)
				return
			}
			if readErr != nil {
				errors <- readErr
				return
			}
			responses <- body
		}()
	}
	close(start)
	group.Wait()
	close(responses)
	close(errors)

	for err := range errors {
		t.Fatalf("concurrent download failed: %v", err)
	}
	count := 0
	for body := range responses {
		count++
		if !bytes.Equal(body, content) {
			t.Fatalf("concurrent response body was corrupted: got %d bytes, want %d", len(body), len(content))
		}
	}
	if count != workers {
		t.Fatalf("successful concurrent responses = %d, want %d", count, workers)
	}
	if got := getCount.Load(); got != 1 {
		t.Fatalf("upstream GET count = %d, want one cache-miss leader", got)
	}

	var artifacts, blobs int
	if err := env.store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1`, env.repoPK).Scan(&artifacts); err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if err := env.store.SQL.QueryRow(`SELECT count(*) FROM blobs b WHERE EXISTS (SELECT 1 FROM artifacts a WHERE a.blob_id=b.id AND a.repository_id=$1)`, env.repoPK).Scan(&blobs); err != nil {
		t.Fatalf("count blobs: %v", err)
	}
	if artifacts != 1 || blobs != 1 {
		t.Fatalf("published cache state = %d artifacts, %d blobs; want 1 artifact and 1 blob", artifacts, blobs)
	}
}
