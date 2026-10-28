package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nicolas/hfproxy/internal/config"
)

func gaugeValue(t *testing.T, s *Server, name string) float64 {
	t.Helper()
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() == name && len(family.GetMetric()) > 0 {
			return family.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s is not registered", name)
	return 0
}

func TestStagingGaugesFollowReservations(t *testing.T) {
	s := &Server{metrics: newMetrics()}
	s.runtime.Store(&runtimeState{config: config.Config{StagingDir: t.TempDir()}})
	if _, ok := diskFree(s.stagingDirectory()); !ok {
		t.Skip("free space is not available on this platform")
	}
	release, err := s.reserveStaging(1000)
	if err != nil {
		t.Fatal(err)
	}
	if got := gaugeValue(t, s, "hfproxy_staging_reserved_bytes"); got != 1000 {
		t.Fatalf("reserved gauge = %v, want 1000", got)
	}
	if got := gaugeValue(t, s, "hfproxy_staging_free_bytes"); got <= 0 {
		t.Fatalf("free gauge = %v, want the free space of the staging directory", got)
	}
	release()
	if got := gaugeValue(t, s, "hfproxy_staging_reserved_bytes"); got != 0 {
		t.Fatalf("reserved gauge after release = %v, want 0", got)
	}
}

func TestReadinessReportsLowStagingSpaceOnlyWhenAsked(t *testing.T) {
	env := ahubNewEnv(t, http.NotFoundHandler())
	if _, ok := diskFree(env.server.stagingDirectory()); !ok {
		t.Skip("free space is not available on this platform")
	}
	readyz := func() (int, string) {
		resp, err := http.Get(env.proxy.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := readyz(); status != http.StatusOK {
		t.Fatalf("baseline /readyz = %d %s", status, body)
	}
	state := *env.server.runtime.Load()
	state.config.StagingMinFreeBytes = 1 << 62
	env.server.runtime.Store(&state)
	status, body := readyz()
	if status != http.StatusServiceUnavailable || !strings.Contains(body, "staging_space") {
		t.Fatalf("with an unreachable minimum, /readyz = %d %s, want 503 staging_space", status, body)
	}
	state.config.StagingMinFreeBytes = 0
	env.server.runtime.Store(&state)
	if status, _ := readyz(); status != http.StatusOK {
		t.Fatalf("with the check off, /readyz = %d, want 200", status)
	}
}

func TestSimultaneousFillsNeverExceedTheConfiguredLimit(t *testing.T) {
	const files, limit = 6, 2
	content := func(name string) []byte { return bytes.Repeat([]byte(name+"-"), 4096) }
	var inFlight, peak atomic.Int32
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		data := content(name)
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+auditSHA256(data)+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(data)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/fills/"+name)
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"fill-cdn"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		if r.Method == http.MethodGet {
			now := inFlight.Add(1)
			for {
				seen := peak.Load()
				if now <= seen || peak.CompareAndSwap(seen, now) {
					break
				}
			}
			time.Sleep(200 * time.Millisecond)
			_, _ = w.Write(data)
			inFlight.Add(-1)
		}
	})
	env := ahubNewEnv(t, hub)
	env.server.fillSlots = make(chan struct{}, limit)

	var wg sync.WaitGroup
	statuses := make([]int, files)
	start := make(chan struct{})
	for i := 0; i < files; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, err := http.Get(fmt.Sprintf("%s/models/%s/resolve/%s/file%d.bin", env.proxy.URL, env.repoID, ahubCommit, i))
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			statuses[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("file %d = %d; waiting fills must complete once a slot frees up", i, status)
		}
	}
	if got := peak.Load(); got > limit {
		t.Fatalf("up to %d files were fetched from the Hub at once, limit %d", got, limit)
	} else if got < limit {
		t.Fatalf("only %d files were fetched at once; the limit %d should be reached with %d simultaneous requests", got, limit, files)
	}
}
