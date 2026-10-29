package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
)

var limitFile = []byte(strings.Repeat("0123456789", 4)) // 40 bytes

func limitHub(t *testing.T, gate <-chan struct{}) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Repo-Commit", ahubCommit)
		w.Header().Set("ETag", `"limit-file"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(limitFile)))
		if r.Method == http.MethodGet {
			if gate != nil {
				<-gate
			}
			_, _ = w.Write(limitFile)
		}
	})
}

// setLimits changes the running configuration of a test server.
func setLimits(env *ahubEnv, change func(*config.Config)) {
	state := env.server.runtime.Load()
	cfg := state.config
	change(&cfg)
	env.server.runtime.Store(&runtimeState{config: cfg, storage: state.storage, httpClient: state.httpClient})
}

// tokenFor creates a reader with a read token and returns the token and the user id.
func tokenFor(t *testing.T, env *ahubEnv, name string) (token, userID string) {
	t.Helper()
	userID = db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'reader')`, userID, name+"-"+userID[:6]); err != nil {
		t.Fatal(err)
	}
	token = randomSecret(32)
	ring := env.server.keyRing()
	if _, err := env.store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix,name,scopes,key_id) VALUES($1,$2,$3,'p','t',ARRAY['read'],$4)`, db.NewID(), userID, ring.Hash(token), ring.KeyID()); err != nil {
		t.Fatal(err)
	}
	return token, userID
}

func (e *ahubEnv) fetch(token string) (*http.Response, []byte) {
	req, _ := http.NewRequest(http.MethodGet, e.proxy.URL+"/"+e.repoID+"/resolve/main/weights.bin", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return &http.Response{StatusCode: 0}, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, body
}

func TestDailyQuotaStopsDownloadsOnceReached(t *testing.T) {
	env := ahubNewEnv(t, limitHub(t, nil))
	token, userID := tokenFor(t, env, "quota")
	setLimits(env, func(c *config.Config) { c.DownloadDailyQuota = 100 })

	for i := 1; i <= 3; i++ {
		env.server.downloadLimits().forget(userID)
		if resp, body := env.fetch(token); resp.StatusCode != http.StatusOK || !bytes.Equal(body, limitFile) {
			t.Fatalf("download %d = %d, want 200 (80 of 100 bytes used at most)", i, resp.StatusCode)
		}
	}
	env.server.downloadLimits().forget(userID)
	resp, body := env.fetch(token)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("download over the quota = %d, want 429 (%s)", resp.StatusCode, body)
	}
	if resp.Header.Get("Retry-After") == "" || !strings.Contains(string(body), "quota") {
		t.Fatalf("the refusal must say why and when to retry: %v %s", resp.Header, body)
	}
	// An anonymous client has no account to count against.
	if resp, _ := env.fetch(""); resp.StatusCode != http.StatusOK {
		t.Fatalf("an anonymous download = %d, want 200", resp.StatusCode)
	}
	// Another user is not affected.
	other, _ := tokenFor(t, env, "other")
	if resp, _ := env.fetch(other); resp.StatusCode != http.StatusOK {
		t.Fatalf("another user = %d, want 200", resp.StatusCode)
	}
}

func TestAUsersOwnQuotaOverridesTheDefault(t *testing.T) {
	env := ahubNewEnv(t, limitHub(t, nil))
	setLimits(env, func(c *config.Config) { c.DownloadDailyQuota = 50 })
	limited, limitedID := tokenFor(t, env, "limited")
	unlimited, unlimitedID := tokenFor(t, env, "unlimited")
	admin := settingsAdmin(t, env.store)
	patch := func(id, body string) *httptest.ResponseRecorder {
		return auditAPIDoAs(env.server, admin, http.MethodPatch, "/api/v1/users/"+id, body)
	}

	if rec := patch(unlimitedID, `{"daily_quota_bytes":0}`); rec.Code != http.StatusOK {
		t.Fatalf("set unlimited = %d %s", rec.Code, rec.Body.String())
	}
	if rec := patch(limitedID, `{"daily_quota_bytes":10}`); rec.Code != http.StatusOK {
		t.Fatalf("set 10 bytes = %d %s", rec.Code, rec.Body.String())
	}
	for i := 0; i < 4; i++ { // far beyond the default of 50 bytes
		if resp, _ := env.fetch(unlimited); resp.StatusCode != http.StatusOK {
			t.Fatalf("an unlimited user was refused on download %d", i+1)
		}
	}
	if resp, _ := env.fetch(limited); resp.StatusCode != http.StatusOK {
		t.Fatalf("first download of the 10-byte user = %d", resp.StatusCode)
	}
	env.server.downloadLimits().forget(limitedID)
	if resp, _ := env.fetch(limited); resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a 40-byte file already used the 10-byte quota, got %d", resp.StatusCode)
	}
	// Clearing the override falls back to the default.
	if rec := patch(limitedID, `{"daily_quota_bytes":null}`); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body.String())
	}
	list := apiJSON(t, auditAPIDoAs(env.server, admin, http.MethodGet, "/api/v1/users?limit=50", "").Body.String())
	for _, item := range list["items"].([]any) {
		user := item.(map[string]any)
		switch user["id"] {
		case unlimitedID:
			if user["daily_quota_bytes"] != float64(0) {
				t.Errorf("unlimited user listed with %v", user["daily_quota_bytes"])
			}
		case limitedID:
			if user["daily_quota_bytes"] != nil {
				t.Errorf("a cleared quota is listed as %v", user["daily_quota_bytes"])
			}
		}
	}
	for name, body := range map[string]string{"negative": `{"daily_quota_bytes":-5}`, "text": `{"daily_quota_bytes":"lots"}`} {
		if rec := patch(limitedID, body); rec.Code != http.StatusBadRequest {
			t.Errorf("%s quota = %d, want 400", name, rec.Code)
		}
	}
	if rec := patch("no-such-user", `{"daily_quota_bytes":5}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown user = %d, want 404", rec.Code)
	}
	if rec := patch(limitedID, `{}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty patch = %d, want 400", rec.Code)
	}
}

func TestConcurrentDownloadsPerUserAreLimited(t *testing.T) {
	gate := make(chan struct{})
	env := ahubNewEnv(t, limitHub(t, gate))
	setLimits(env, func(c *config.Config) { c.DownloadMaxConcurrent = 1 })
	token, _ := tokenFor(t, env, "busy")
	other, _ := tokenFor(t, env, "free")

	var wg sync.WaitGroup
	first := make(chan int, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		resp, _ := env.fetch(token)
		first <- resp.StatusCode
	}()
	// Wait until the first transfer is running (blocked at the Hub).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		l := env.server.downloadLimits()
		l.mu.Lock()
		running := l.active["user:"+userOf(t, env, token)]
		l.mu.Unlock()
		if running == 1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	resp, body := env.fetch(token)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("a second simultaneous download = %d %s, want 429", resp.StatusCode, body)
	}
	// Another user and an anonymous client have their own allowance: both get
	// past the limiter and wait at the Hub like the first one.
	released := make(chan struct{})
	go func() {
		time.Sleep(300 * time.Millisecond)
		close(gate)
		close(released)
	}()
	if resp, _ := env.fetch(other); resp.StatusCode != http.StatusOK {
		t.Fatalf("another user = %d, want 200", resp.StatusCode)
	}
	<-released
	wg.Wait()
	if status := <-first; status != http.StatusOK {
		t.Fatalf("the first download = %d, want 200", status)
	}
	// The slot was given back.
	if resp, body := env.fetch(token); resp.StatusCode != http.StatusOK {
		l := env.server.downloadLimits()
		l.mu.Lock()
		active := len(l.active)
		l.mu.Unlock()
		t.Fatalf("a download after the first finished = %d %s (%d slots in use)", resp.StatusCode, body, active)
	}
	l := env.server.downloadLimits()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.active) != 0 {
		t.Fatalf("slots leaked: %v", l.active)
	}
}

func userOf(t *testing.T, env *ahubEnv, token string) string {
	t.Helper()
	var id string
	ring := env.server.keyRing()
	if err := env.store.SQL.QueryRow(`SELECT user_id FROM access_tokens WHERE token_hash=$1`, ring.Hash(token)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestSpeedLimitPacesTheResponse(t *testing.T) {
	rec := httptest.NewRecorder()
	writer := newThrottledWriter(context.Background(), rec, 1_000_000) // 1 MB/s
	data := bytes.Repeat([]byte("x"), 1_000_000)
	start := time.Now()
	n, err := writer.Write(data)
	elapsed := time.Since(start)
	if err != nil || n != len(data) || rec.Body.Len() != len(data) {
		t.Fatalf("wrote %d of %d bytes, err %v", n, len(data), err)
	}
	// 1 MB at 1 MB/s is a second, less the quarter second let through at once
	// and the last chunk, which is not waited for.
	if elapsed < 500*time.Millisecond {
		t.Fatalf("1 MB went through in %v: the speed cap did nothing", elapsed)
	}
	if elapsed > 4*time.Second {
		t.Fatalf("1 MB took %v at 1 MB/s: far slower than the cap", elapsed)
	}
}

func TestSpeedLimitStopsWhenTheClientLeaves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := newThrottledWriter(ctx, httptest.NewRecorder(), 1_000) // 1 kB/s
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := writer.Write(bytes.Repeat([]byte("x"), 100_000))
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("a cancelled transfer must stop quickly: err=%v after %v", err, time.Since(start))
	}
}

func TestDownloadLimitsAreConsoleSettings(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	admin := settingsAdmin(t, store)
	view := settingsPayload(t, s, admin)
	for _, key := range []string{"download_max_concurrent", "download_rate_limit", "download_daily_quota"} {
		if view[key] != float64(0) {
			t.Fatalf("%s = %v, want 0 (off)", key, view[key])
		}
	}
	view["download_max_concurrent"], view["download_rate_limit"], view["download_daily_quota"] = 3, 1048576, 5368709120
	if status, out := putSettings(t, s, admin, view); status != http.StatusOK || out["download_daily_quota"] != float64(5368709120) {
		t.Fatalf("PUT = %d %v", status, out)
	}
	cfg := s.currentConfig()
	if cfg.DownloadMaxConcurrent != 3 || cfg.DownloadRateLimit != 1048576 || cfg.DownloadDailyQuota != 5368709120 {
		t.Fatalf("limits not applied: %+v", cfg)
	}
	delete(view, "download_rate_limit") // left out: unchanged
	view["download_daily_quota"] = 0
	if status, _ := putSettings(t, s, admin, view); status != http.StatusOK {
		t.Fatal("PUT without a limit must work")
	}
	if cfg := s.currentConfig(); cfg.DownloadRateLimit != 1048576 || cfg.DownloadDailyQuota != 0 {
		t.Fatalf("a missing limit must stay, 0 must switch it off: %+v", cfg)
	}
	view["download_max_concurrent"] = -1
	if status, _ := putSettings(t, s, admin, view); status != http.StatusBadRequest {
		t.Fatalf("a negative limit = %d, want 400", status)
	}
}

func TestASlotIsGivenBackWhenTheBodyIsComplete(t *testing.T) {
	released := 0
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Length", "10")
	w := &slotWriter{ResponseWriter: rec, release: func() { released++ }, length: -1}
	w.Write([]byte("12345"))
	if released != 0 {
		t.Fatal("the slot was given back before the body was complete")
	}
	w.Write([]byte("67890"))
	if released != 1 {
		t.Fatalf("the slot must be given back with the last byte, released %d times", released)
	}
}
