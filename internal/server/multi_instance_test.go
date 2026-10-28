package server

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// secondInstance starts another hfproxy process' worth of server on the same
// database and object store as env, the way a second replica would run.
func secondInstance(t *testing.T, env *ahubEnv) *httptest.Server {
	t.Helper()
	cfg := ahubConfig()
	s, err := New(t.Context(), cfg, env.store, env.server.currentStorage(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Timeout:       cfg.HFTimeout,
		Transport:     ahubRewriteTransport{target: env.hub.Listener.Addr().String()},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	s.runtime.Store(&runtimeState{config: cfg, storage: env.server.currentStorage(), httpClient: client})
	proxy := httptest.NewServer(s.Handler())
	t.Cleanup(proxy.Close)
	return proxy
}

// A file with a verifiable digest requested through two replicas at once is
// fetched from the Hub once. Files without a digest are served correctly by
// both, but followers may each stream from the Hub (see docs/operations.md).
func TestTwoReplicasFillTheSameFileOnce(t *testing.T) {
	content := bytes.Repeat([]byte("replica-model-"), 8192)
	for _, tc := range []struct {
		name      string
		sizeKnown bool
	}{{"size announced by the Hub", true}, {"size unknown until the body ends", false}} {
		t.Run(tc.name, func(t *testing.T) {
			digest := auditSHA256(content)
			var bodyFetches atomic.Int32
			hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host == "huggingface.co" {
					w.Header().Set("X-Repo-Commit", ahubCommit)
					if tc.sizeKnown {
						w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
						w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
						w.Header().Set("Location", "https://cdn-lfs.hf.co/replica/model.bin")
						w.WriteHeader(http.StatusFound)
						return
					}
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if tc.sizeKnown {
					w.Header().Set("Content-Length", strconv.Itoa(len(content)))
				}
				if r.Method == http.MethodGet {
					bodyFetches.Add(1)
					time.Sleep(100 * time.Millisecond)
					_, _ = w.Write(content)
				}
			})
			env := ahubNewEnv(t, hub)
			other := secondInstance(t, env)

			targets := []string{env.proxy.URL, other.URL}
			start := make(chan struct{})
			var group sync.WaitGroup
			failures := make(chan string, 8)
			for i := 0; i < 8; i++ {
				group.Add(1)
				go func(base string) {
					defer group.Done()
					<-start
					response, err := http.Get(base + "/" + env.repoID + "/resolve/main/model.bin")
					if err != nil {
						failures <- err.Error()
						return
					}
					body, _ := io.ReadAll(response.Body)
					response.Body.Close()
					if response.StatusCode != http.StatusOK || !bytes.Equal(body, content) {
						failures <- "status " + strconv.Itoa(response.StatusCode) + ", " + strconv.Itoa(len(body)) + " bytes"
					}
				}(targets[i%2])
			}
			close(start)
			group.Wait()
			close(failures)
			for failure := range failures {
				t.Errorf("a download failed: %s", failure)
			}
			got := bodyFetches.Load()
			if tc.sizeKnown && got != 1 {
				t.Fatalf("the Hub was asked for the body %d times, want 1 across both replicas", got)
			}
			// Without a digest to verify, followers of a fill stream straight from
			// the Hub instead of waiting, so several fetches are expected; every
			// client must still get the right bytes (checked above).
			if !tc.sizeKnown && got < 1 {
				t.Fatal("the Hub was never asked for the file")
			}
		})
	}
}
