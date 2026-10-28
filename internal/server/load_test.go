package server

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestLoadParallelDownloads is a load test, off by default. Set
// HFPROXY_LOAD_TEST=1 to run it; HFPROXY_LOAD_CLIENTS (default 16) and
// HFPROXY_LOAD_FILE_MB (default 8) size it. Every client downloads its own file
// cold (a cache miss), then every client downloads it again (a hit). It reports
// latency percentiles, heap, database connections in use and leftover staging
// files, and fails on any error, wrong byte, leftover staging file or connection
// leak. CI runs a larger setting nightly.
func TestLoadParallelDownloads(t *testing.T) {
	if os.Getenv("HFPROXY_LOAD_TEST") == "" {
		t.Skip("set HFPROXY_LOAD_TEST=1 to run the load test")
	}
	clients := envInt("HFPROXY_LOAD_CLIENTS", 16)
	size := int64(envInt("HFPROXY_LOAD_FILE_MB", 8)) << 20

	pattern := func(file int) []byte {
		block := make([]byte, 64<<10)
		for i := range block {
			block[i] = byte((i*7 + file*13) % 251)
		}
		return block
	}
	write := func(w io.Writer, file int) {
		block := pattern(file)
		for remaining := size; remaining > 0; {
			n := int64(len(block))
			if n > remaining {
				n = remaining
			}
			if _, err := w.Write(block[:n]); err != nil {
				return
			}
			remaining -= n
		}
	}
	digests := make([]string, clients)
	for i := range digests {
		h := sha256.New()
		write(h, i)
		digests[i] = hex.EncodeToString(h.Sum(nil))
	}

	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var file int
		if r.Host == "huggingface.co" {
			fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "f%d.bin", &file)
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digests[file]+`"`)
			w.Header().Set("X-Linked-Size", strconv.FormatInt(size, 10))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/load/f"+strconv.Itoa(file)+".bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		fmt.Sscanf(r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:], "f%d.bin", &file)
		w.Header().Set("ETag", `"load"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
			return
		}
		start, end, partial, _ := parseByteRange(r.Header.Get("Range"), size)
		if partial {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			// Serve the requested window of the repeating pattern.
			window := io.NewSectionReader(&patternReader{pattern: pattern(file)}, start, end-start+1)
			_, _ = io.Copy(w, window)
			return
		}
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		write(w, file)
	})
	env := ahubNewEnv(t, hub)
	staging := t.TempDir()
	state := *env.server.runtime.Load()
	state.config.StagingDir = staging
	state.config.CacheFillConcurrency = 0
	env.server.runtime.Store(&state)

	var retries atomic.Int64
	maxInUse := 0
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		for {
			select {
			case <-stop:
				return
			case <-time.After(20 * time.Millisecond):
				if n := env.store.SQL.Stats().InUse; n > maxInUse {
					maxInUse = n
				}
			}
		}
	}()

	round := func(name string) (p50, p95, max time.Duration) {
		latencies := make([]time.Duration, clients)
		failures := make(chan string, clients)
		var wg sync.WaitGroup
		for i := 0; i < clients; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				started := time.Now()
				// Like a well-behaved client, retry a 503 "capacity exhausted"
				// after a short pause instead of counting it as a failure.
				var response *http.Response
				var err error
				for attempt := 0; attempt < 120; attempt++ {
					response, err = http.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/f" + strconv.Itoa(i) + ".bin")
					if err != nil || response.StatusCode != http.StatusServiceUnavailable {
						break
					}
					io.Copy(io.Discard, response.Body)
					response.Body.Close()
					retries.Add(1)
					time.Sleep(500 * time.Millisecond)
				}
				if err != nil {
					failures <- err.Error()
					return
				}
				defer response.Body.Close()
				h := sha256.New()
				n, _ := io.Copy(h, response.Body)
				latencies[i] = time.Since(started)
				if response.StatusCode != http.StatusOK || n != size || hex.EncodeToString(h.Sum(nil)) != digests[i] {
					failures <- fmt.Sprintf("client %d: status %d, %d bytes, wrong digest=%v", i, response.StatusCode, n, hex.EncodeToString(h.Sum(nil)) != digests[i])
				}
			}(i)
		}
		wg.Wait()
		close(failures)
		for failure := range failures {
			t.Errorf("%s: %s", name, failure)
		}
		sort.Slice(latencies, func(a, b int) bool { return latencies[a] < latencies[b] })
		return latencies[len(latencies)/2], latencies[len(latencies)*95/100], latencies[len(latencies)-1]
	}

	missP50, missP95, missMax := round("miss")
	hitP50, hitP95, hitMax := round("hit")
	close(stop)
	sampler.Wait()

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	leftovers, _ := os.ReadDir(staging)
	t.Logf("%d clients x %d MiB: miss p50=%v p95=%v max=%v | hit p50=%v p95=%v max=%v | heap in use %d MiB (includes the in-process fake object store and Hub), max DB connections in use %d, staging files left %d",
		clients, size>>20, missP50, missP95, missMax, hitP50, hitP95, hitMax, mem.HeapInuse>>20, maxInUse, len(leftovers))
	if len(leftovers) != 0 {
		t.Errorf("%d staging files were left behind", len(leftovers))
	}
	t.Logf("requests retried after a 503 (fill capacity): %d", retries.Load())
	// Connections are released just after the last byte is sent; allow a moment.
	deadline := time.Now().Add(5 * time.Second)
	for env.store.SQL.Stats().InUse != 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if inUse := env.store.SQL.Stats().InUse; inUse != 0 {
		t.Errorf("%d database connections are still in use after the downloads (leak)", inUse)
	}
}

type patternReader struct{ pattern []byte }

func (p *patternReader) ReadAt(b []byte, off int64) (int, error) {
	for i := range b {
		b[i] = p.pattern[(off+int64(i))%int64(len(p.pattern))]
	}
	return len(b), nil
}

func envInt(name string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return fallback
}
