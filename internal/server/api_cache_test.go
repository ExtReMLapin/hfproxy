package server

import (
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeMetadataHub struct {
	requests    atomic.Int32
	conditional atomic.Int32
	down        atomic.Bool
	delay       time.Duration
}

const fakeMetadataETag = `"rev-1"`

func (h *fakeMetadataHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/api/models/") {
		http.NotFound(w, r)
		return
	}
	h.requests.Add(1)
	if h.delay > 0 {
		time.Sleep(h.delay)
	}
	if h.down.Load() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if r.Header.Get("If-None-Match") == fakeMetadataETag {
		h.conditional.Add(1)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("ETag", fakeMetadataETag)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Link", `<https://huggingface.co/api/models/org/m/tree/main?cursor=abc>; rel="next"`)
	_, _ = io.WriteString(w, `{"sha":"`+ahubCommit+`"}`)
}

func metadataCacheEnv(t *testing.T, hub *fakeMetadataHub, ttl time.Duration) *ahubEnv {
	t.Helper()
	previous := upstreamRetryBase
	upstreamRetryBase = time.Millisecond
	t.Cleanup(func() { upstreamRetryBase = previous })
	env := ahubNewEnv(t, hub)
	state := *env.server.runtime.Load()
	state.config.APICacheMaxBytes = 1 << 20
	state.config.APICacheMaxStale = time.Hour
	state.config.CacheRefTTL = ttl
	env.server.runtime.Store(&state)
	return env
}

func getMetadata(t *testing.T, env *ahubEnv, method, route string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, env.proxy.URL+"/api/models/"+env.repoID+route, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func TestMetadataIsCachedThenRevalidated(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, 150*time.Millisecond)

	first, body := getMetadata(t, env, http.MethodGet, "/revision/main")
	if first.StatusCode != http.StatusOK || first.Header.Get("X-Cache") != "MISS" || !strings.Contains(body, ahubCommit) {
		t.Fatalf("first request: %d X-Cache=%q body=%q", first.StatusCode, first.Header.Get("X-Cache"), body)
	}
	second, secondBody := getMetadata(t, env, http.MethodGet, "/revision/main")
	if second.Header.Get("X-Cache") != "HIT" || secondBody != body || hub.requests.Load() != 1 {
		t.Fatalf("second request: X-Cache=%q hub requests=%d, want a HIT without contacting the Hub", second.Header.Get("X-Cache"), hub.requests.Load())
	}
	if link := second.Header.Get("Link"); !strings.Contains(link, strings.TrimPrefix(env.proxy.URL, "http://")) || strings.Contains(link, "huggingface.co") {
		t.Fatalf("a cached response must have its Link rewritten for this proxy, got %q", link)
	}

	time.Sleep(250 * time.Millisecond)
	third, thirdBody := getMetadata(t, env, http.MethodGet, "/revision/main")
	if third.Header.Get("X-Cache") != "REVALIDATED" || thirdBody != body || hub.conditional.Load() != 1 {
		t.Fatalf("expired entry: X-Cache=%q conditional requests=%d, want REVALIDATED through If-None-Match", third.Header.Get("X-Cache"), hub.conditional.Load())
	}
	if again, _ := getMetadata(t, env, http.MethodGet, "/revision/main"); again.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("a revalidated entry must be fresh again, got %q", again.Header.Get("X-Cache"))
	}
}

func TestDifferentRoutesAndQueriesAreCachedSeparately(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, time.Minute)
	for _, route := range []string{"/revision/main", "/revision/dev", "/tree/main?recursive=true", "/tree/main"} {
		if resp, _ := getMetadata(t, env, http.MethodGet, route); resp.Header.Get("X-Cache") != "MISS" {
			t.Fatalf("%s: X-Cache=%q, want its own MISS", route, resp.Header.Get("X-Cache"))
		}
	}
	if hub.requests.Load() != 4 {
		t.Fatalf("Hub saw %d requests, want 4", hub.requests.Load())
	}
}

func TestStaleMetadataIsServedWhenTheHubFails(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, 100*time.Millisecond)
	_, body := getMetadata(t, env, http.MethodGet, "/revision/main")
	time.Sleep(200 * time.Millisecond)
	hub.down.Store(true)

	resp, staleBody := getMetadata(t, env, http.MethodGet, "/revision/main")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Cache") != "STALE" || staleBody != body {
		t.Fatalf("Hub down: %d X-Cache=%q, want the cached answer marked STALE", resp.StatusCode, resp.Header.Get("X-Cache"))
	}
	if uncached, _ := getMetadata(t, env, http.MethodGet, "/revision/never-seen"); uncached.StatusCode < 500 {
		t.Fatalf("an answer that was never cached cannot be invented, got %d", uncached.StatusCode)
	}

	hub.down.Store(false)
	if recovered, _ := getMetadata(t, env, http.MethodGet, "/revision/main"); recovered.Header.Get("X-Cache") != "REVALIDATED" {
		t.Fatalf("after recovery: X-Cache=%q, want REVALIDATED", recovered.Header.Get("X-Cache"))
	}
}

func TestStaleMetadataIsNotServedBeyondTheLimit(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, 50*time.Millisecond)
	state := *env.server.runtime.Load()
	state.config.APICacheMaxStale = 150 * time.Millisecond
	env.server.runtime.Store(&state)
	getMetadata(t, env, http.MethodGet, "/revision/main")
	time.Sleep(300 * time.Millisecond)
	hub.down.Store(true)
	if resp, _ := getMetadata(t, env, http.MethodGet, "/revision/main"); resp.StatusCode < 500 {
		t.Fatalf("an entry older than the stale limit was served: %d X-Cache=%q", resp.StatusCode, resp.Header.Get("X-Cache"))
	}
}

func TestAccessPolicyIsCheckedBeforeTheMetadataCache(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, time.Minute)
	getMetadata(t, env, http.MethodGet, "/revision/main")
	if _, err := env.store.SQL.Exec(`UPDATE repositories SET access_policy='admin_only' WHERE id=$1`, env.repoPK); err != nil {
		t.Fatal(err)
	}
	before := hub.requests.Load()
	resp, body := getMetadata(t, env, http.MethodGet, "/revision/main")
	if resp.StatusCode != http.StatusUnauthorized || strings.Contains(body, ahubCommit) {
		t.Fatalf("a cached answer was served to an anonymous client of a restricted repository: %d %q", resp.StatusCode, body)
	}
	if hub.requests.Load() != before {
		t.Fatal("a refused request still reached the Hub")
	}
}

func TestHeadRequestsBypassTheMetadataCache(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, time.Minute)
	getMetadata(t, env, http.MethodGet, "/revision/main")
	for i := 0; i < 2; i++ {
		getMetadata(t, env, http.MethodHead, "/revision/main")
	}
	if hub.requests.Load() != 3 {
		t.Fatalf("Hub saw %d requests, want the GET and both HEADs (3)", hub.requests.Load())
	}
}

func TestConcurrentMetadataRequestsShareOneHubFetch(t *testing.T) {
	hub := &fakeMetadataHub{delay: 200 * time.Millisecond}
	env := metadataCacheEnv(t, hub, time.Minute)
	const clients = 12
	var wg sync.WaitGroup
	statuses := make([]int, clients)
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, _ := getMetadata(t, env, http.MethodGet, "/revision/main")
			statuses[i] = resp.StatusCode
		}(i)
	}
	close(start)
	wg.Wait()
	for i, status := range statuses {
		if status != http.StatusOK {
			t.Fatalf("client %d got %d", i, status)
		}
	}
	if got := hub.requests.Load(); got != 1 {
		t.Fatalf("Hub saw %d requests for %d simultaneous identical requests, want 1", got, clients)
	}
}

func TestMetadataCacheIsOffWhenNotConfigured(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := ahubNewEnv(t, hub) // the default test configuration leaves the cache size at 0
	for i := 0; i < 3; i++ {
		getMetadata(t, env, http.MethodGet, "/revision/main")
	}
	if hub.requests.Load() != 3 {
		t.Fatalf("Hub saw %d requests, want 3 with the cache disabled", hub.requests.Load())
	}
}

func TestAPICacheEvictsLeastRecentlyUsedWithinItsByteBudget(t *testing.T) {
	entry := func(key string, size int) apiCacheEntry {
		return apiCacheEntry{key: key, body: make([]byte, size), storedAt: time.Now()}
	}
	one := entry("a", 1000)
	cache := newAPICache(one.size()*2 + 10)
	cache.put(entry("a", 1000))
	cache.put(entry("b", 1000))
	if _, ok := cache.get("a"); !ok { // a is now the most recently used
		t.Fatal("a was evicted too early")
	}
	cache.put(entry("c", 1000))
	if _, ok := cache.get("b"); ok {
		t.Fatal("b should have been evicted as the least recently used")
	}
	for _, key := range []string{"a", "c"} {
		if _, ok := cache.get(key); !ok {
			t.Fatalf("%s was evicted", key)
		}
	}
	if entries, used := cache.usage(); entries != 2 || used > cache.maxBytes {
		t.Fatalf("usage = %d entries %d bytes, budget %d", entries, used, cache.maxBytes)
	}
	cache.put(entry("huge", 1<<20))
	if _, ok := cache.get("huge"); ok {
		t.Fatal("an entry larger than the whole cache was stored")
	}
	cache.put(entry("a", 500))
	if got, _ := cache.get("a"); len(got.body) != 500 {
		t.Fatal("replacing an entry must keep the new value")
	}
	cache.delete("a")
	cache.delete("c")
	if entries, used := cache.usage(); entries != 0 || used != 0 {
		t.Fatalf("after deleting everything: %d entries %d bytes", entries, used)
	}
}

func TestChangingTheHubDoesNotServeAnswersFromThePreviousOne(t *testing.T) {
	hub := &fakeMetadataHub{}
	env := metadataCacheEnv(t, hub, time.Minute)
	getMetadata(t, env, http.MethodGet, "/revision/main")
	state := *env.server.runtime.Load()
	state.config.HFBaseURL = "http://localhost" // another Hub, same loopback test double
	env.server.runtime.Store(&state)
	if resp, _ := getMetadata(t, env, http.MethodGet, "/revision/main"); resp.Header.Get("X-Cache") != "MISS" {
		t.Fatalf("X-Cache=%q after the Hub URL changed, want a fresh MISS", resp.Header.Get("X-Cache"))
	}
}
