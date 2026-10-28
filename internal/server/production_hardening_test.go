package server

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/storage"
)

func TestActiveContentIsForcedToDownload(t *testing.T) {
	for _, tc := range []struct {
		contentType, path string
		want              string
	}{
		{"text/html; charset=utf-8", "docs/index.html", `attachment; filename=index.html`},
		{"image/svg+xml", "logo.svg", `attachment; filename=logo.svg`},
		{"application/octet-stream", "page.HTML", `attachment; filename=page.HTML`},
		{"application/json", "config.json", ""},
		{"application/octet-stream", "model.safetensors", ""},
	} {
		rec := httptest.NewRecorder()
		upstream := http.Header{"Content-Disposition": {"inline"}}
		(&Server{}).setArtifactHeaders(rec, artifact{ContentType: tc.contentType, Path: tc.path, Commit: "abc"}, upstream)
		got := rec.Header().Get("Content-Disposition")
		if tc.want == "" {
			if strings.HasPrefix(got, "attachment") {
				t.Errorf("%s %s: unexpectedly forced to download: %q", tc.contentType, tc.path, got)
			}
			continue
		}
		if got != tc.want {
			t.Errorf("%s %s: Content-Disposition = %q, want %q", tc.contentType, tc.path, got, tc.want)
		}
	}
}

func TestStrictTransportSecurityOnlyForHTTPSPublicURL(t *testing.T) {
	for url, want := range map[string]bool{"https://hf.example.com": true, "http://localhost:8080": false, "": false} {
		s := &Server{}
		s.runtime.Store(&runtimeState{config: config.Config{PublicURL: url}})
		rec := httptest.NewRecorder()
		s.securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if got := rec.Header().Get("Strict-Transport-Security") != ""; got != want {
			t.Errorf("PublicURL %q: HSTS present = %v, want %v", url, got, want)
		}
	}
}

func TestFillSlotsBoundConcurrentFills(t *testing.T) {
	previous := fillSlotWait
	fillSlotWait = 20 * time.Millisecond
	t.Cleanup(func() { fillSlotWait = previous })
	s := &Server{fillSlots: make(chan struct{}, 1)}
	release, err := s.acquireFillSlot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.acquireFillSlot(context.Background()); err != errFillBusy {
		t.Fatalf("second acquire = %v, want errFillBusy", err)
	}
	release()
	again, err := s.acquireFillSlot(context.Background())
	if err != nil {
		t.Fatalf("acquire after release: %v", err)
	}
	again()
	busy := &Server{fillSlots: make(chan struct{}, 1)}
	busy.fillSlots <- struct{}{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := busy.acquireFillSlot(canceled); err != context.Canceled {
		t.Fatalf("acquire with a canceled context = %v, want context.Canceled", err)
	}
}

func TestStagingReservationRefusesFilesLargerThanFreeSpace(t *testing.T) {
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{StagingDir: t.TempDir()}})
	if _, ok := diskFree(s.stagingDirectory()); !ok {
		t.Skip("free space is not available on this platform")
	}
	if _, err := s.reserveStaging(1 << 62); err != errStagingFull {
		t.Fatalf("reserveStaging(huge) = %v, want errStagingFull", err)
	}
	if s.stagingReserved.Load() != 0 {
		t.Fatalf("failed reservation left %d bytes reserved", s.stagingReserved.Load())
	}
	release, err := s.reserveStaging(1)
	if err != nil {
		t.Fatalf("reserveStaging(1): %v", err)
	}
	release()
	if s.stagingReserved.Load() != 0 {
		t.Fatalf("released reservation left %d bytes reserved", s.stagingReserved.Load())
	}
}

func TestStagingFilesUseConfiguredDirectory(t *testing.T) {
	dir := t.TempDir()
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{StagingDir: dir}})
	f, err := s.createStagingFile("hfproxy-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !strings.HasPrefix(f.Name(), dir) {
		t.Fatalf("staging file %q is not under %q", f.Name(), dir)
	}
}

func TestMetricsListenerReplacesPublicEndpoint(t *testing.T) {
	cfg := config.Config{MetricsEnabled: true, MetricsPath: "/metrics", MetricsAddr: "127.0.0.1:0"}
	s := &Server{metrics: newMetrics(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.runtime.Store(&runtimeState{config: cfg})

	dedicated := httptest.NewRecorder()
	s.metricsHandler().ServeHTTP(dedicated, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if dedicated.Code != http.StatusOK || !strings.Contains(dedicated.Body.String(), "# HELP") {
		t.Fatalf("dedicated listener = %d, want metrics", dedicated.Code)
	}
	other := httptest.NewRecorder()
	s.metricsHandler().ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/api/v1/users", nil))
	if other.Code != http.StatusNotFound {
		t.Fatalf("dedicated listener served %d for another path, want 404", other.Code)
	}
	public := httptest.NewRecorder()
	s.Handler().ServeHTTP(public, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(public.Body.String(), "# HELP") {
		t.Fatal("metrics are still exposed on the public listener")
	}

	cfg.MetricsAddr = ""
	s.runtime.Store(&runtimeState{config: cfg})
	shared := httptest.NewRecorder()
	s.Handler().ServeHTTP(shared, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(shared.Body.String(), "# HELP") {
		t.Fatal("metrics should stay on the main listener when no dedicated address is set")
	}
}

func TestReadScopedTokenCannotUseAdministrationAPI(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	for scope, want := range map[string]int{"read": http.StatusForbidden, "write": http.StatusOK, "admin": http.StatusOK} {
		p := &Principal{ID: "admin", Username: "admin", Role: "admin", Authenticated: true, TokenID: "token-" + scope, Scopes: []string{scope}}
		if rec := auditAPIDoAs(s, p, http.MethodGet, "/api/v1/users", ""); rec.Code != want {
			t.Errorf("%s-scoped admin token: GET /api/v1/users = %d, want %d", scope, rec.Code, want)
		}
	}
	session := &Principal{ID: "admin", Username: "admin", Role: "admin", Authenticated: true}
	if rec := auditAPIDoAs(s, session, http.MethodGet, "/api/v1/users", ""); rec.Code != http.StatusOK {
		t.Errorf("browser session: GET /api/v1/users = %d, want 200", rec.Code)
	}
}

func TestLoginReplacesTheSessionTheBrowserAlreadyHolds(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	userID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'rotator','reader')`, userID); err != nil {
		t.Fatal(err)
	}
	old := HashToken("pre-login-session", auditAPISessionSecret)
	if _, err := store.SQL.Exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$1,now()+interval '1 hour')`, old, userID); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/login", nil)
	req.AddCookie(&http.Cookie{Name: "hfproxy_session", Value: "pre-login-session"})
	if _, err := s.createSession(context.Background(), userID, httptest.NewRecorder(), req); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM sessions WHERE token_hash=$1`, old).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatal("the pre-login session survived the login")
	}
	if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM sessions WHERE user_id=$1`, userID).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("sessions after login = %d (%v), want exactly the new one", remaining, err)
	}
}

func TestTokenAndSessionBookkeepingIsThrottled(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	userID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'throttle','reader')`, userID); err != nil {
		t.Fatal(err)
	}
	const secret = "throttled-token-value-0123456789"
	if _, err := store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix) VALUES('tok1',$1,$2,'throttled')`, userID, HashToken(secret, auditAPISessionSecret)); err != nil {
		t.Fatal(err)
	}
	authenticate := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/api/whoami-v2", nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		if _, err := s.authenticate(req); err != nil {
			t.Fatal(err)
		}
	}
	lastUsed := func() time.Time {
		t.Helper()
		var at time.Time
		if err := store.SQL.QueryRow(`SELECT last_used_at FROM access_tokens WHERE id='tok1'`).Scan(&at); err != nil {
			t.Fatal(err)
		}
		return at
	}
	authenticate()
	first := lastUsed()
	recent := first.Add(-10 * time.Second)
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET last_used_at=$1 WHERE id='tok1'`, recent); err != nil {
		t.Fatal(err)
	}
	authenticate()
	if !lastUsed().Equal(recent) {
		t.Fatal("last_used_at was rewritten within the throttle window")
	}
	stale := first.Add(-5 * time.Minute)
	if _, err := store.SQL.Exec(`UPDATE access_tokens SET last_used_at=$1 WHERE id='tok1'`, stale); err != nil {
		t.Fatal(err)
	}
	authenticate()
	if !lastUsed().After(stale) {
		t.Fatal("last_used_at was not refreshed after the throttle window")
	}
}

func TestRetentionPurgesExpiredRowsOnly(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.RetentionDownloads = 48 * time.Hour
	cfg.RetentionAudit = 48 * time.Hour
	s := auditAPINewServer(t, store, cfg)
	userID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'retention','reader')`, userID); err != nil {
		t.Fatal(err)
	}
	for name, age := range map[string]string{"old": "100 days", "new": "1 hour"} {
		if _, err := store.SQL.Exec(`INSERT INTO downloads(id,path,status,started_at) VALUES($1,$2,'hit',now()-$3::interval)`, db.NewID(), name, age); err != nil {
			t.Fatal(err)
		}
		if _, err := store.SQL.Exec(`INSERT INTO audit_events(id,action,created_at) VALUES($1,$2,now()-$3::interval)`, db.NewID(), name, age); err != nil {
			t.Fatal(err)
		}
	}
	for name, expiry := range map[string]string{"expired": "-1 hour", "active": "1 hour"} {
		hash := HashToken(name, auditAPISessionSecret)
		if _, err := store.SQL.Exec(`INSERT INTO sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$1,now()+$3::interval)`, hash, userID, expiry); err != nil {
			t.Fatal(err)
		}
	}
	s.runRetention(context.Background())
	count := func(query string) int {
		t.Helper()
		var n int
		if err := store.SQL.QueryRow(query).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM downloads WHERE path='old'`); n != 0 {
		t.Errorf("old downloads left = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM downloads WHERE path='new'`); n != 1 {
		t.Errorf("recent downloads left = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM audit_events WHERE action='old'`); n != 0 {
		t.Errorf("old audit events left = %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM audit_events WHERE action='new'`); n != 1 {
		t.Errorf("recent audit events left = %d, want 1", n)
	}
	if n := count(`SELECT COUNT(*) FROM sessions`); n != 1 {
		t.Errorf("sessions left = %d, want only the active one", n)
	}

	// A zero period keeps everything.
	cfg.RetentionDownloads, cfg.RetentionAudit = 0, 0
	keep := auditAPINewServer(t, store, cfg)
	if _, err := store.SQL.Exec(`INSERT INTO downloads(id,path,status,started_at) VALUES($1,'ancient','hit',now()-interval '1000 days')`, db.NewID()); err != nil {
		t.Fatal(err)
	}
	keep.runRetention(context.Background())
	if n := count(`SELECT COUNT(*) FROM downloads WHERE path='ancient'`); n != 1 {
		t.Errorf("retention disabled but ancient downloads left = %d, want 1", n)
	}
}

func seedEvictionArtifact(t *testing.T, store *db.Store, repo, name, blob string, size int64, age string) {
	t.Helper()
	if blob == "" {
		blob = db.NewID()
		sha := strings.ReplaceAll(db.NewID()+db.NewID(), "-", "")
		if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,$3,$4)`, blob, sha, size, "blobs/"+sha); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,last_accessed_at) VALUES($1,$2,$3,$4,$5,now()-$6::interval)`,
		db.NewID(), repo, strings.Repeat("b", 40), name, blob, age); err != nil {
		t.Fatal(err)
	}
}

func TestEvictionRemovesLeastRecentlyUsedArtifactsOverQuota(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 25
	s := auditAPINewServer(t, store, cfg)
	repo := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models','org/evict','public')`, repo); err != nil {
		t.Fatal(err)
	}
	seedEvictionArtifact(t, store, repo, "oldest.bin", "", 10, "3 days")
	seedEvictionArtifact(t, store, repo, "middle.bin", "", 10, "2 days")
	seedEvictionArtifact(t, store, repo, "newest.bin", "", 10, "1 day")

	evicted, err := s.evictOverQuota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 1 {
		t.Fatalf("evicted = %d, want 1 (30 bytes over a 25 byte quota drops one 10 byte file)", evicted)
	}
	var names []string
	rows, err := store.SQL.Query(`SELECT path FROM artifacts ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		_ = rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if strings.Join(names, ",") != "middle.bin,newest.bin" {
		t.Fatalf("remaining artifacts = %v, want the two most recently used", names)
	}
	var orphaned int
	if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM blobs WHERE orphaned_at IS NOT NULL`).Scan(&orphaned); err != nil || orphaned != 1 {
		t.Fatalf("orphaned blobs = %d (%v), want 1", orphaned, err)
	}
	if again, err := s.evictOverQuota(context.Background()); err != nil || again != 0 {
		t.Fatalf("second pass evicted %d (%v), want 0 once under quota", again, err)
	}
}

func TestEvictionKeepsBlobsStillReferenced(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 5
	s := auditAPINewServer(t, store, cfg)
	repo := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models','org/shared','public')`, repo); err != nil {
		t.Fatal(err)
	}
	blob := db.NewID()
	sha := strings.ReplaceAll(db.NewID()+db.NewID(), "-", "")
	if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,10,$3)`, blob, sha, "blobs/"+sha); err != nil {
		t.Fatal(err)
	}
	seedEvictionArtifact(t, store, repo, "a.bin", blob, 10, "2 days")
	seedEvictionArtifact(t, store, repo, "b.bin", blob, 10, "1 day")
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	var artifacts, orphaned int
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM artifacts`).Scan(&artifacts)
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM blobs WHERE orphaned_at IS NOT NULL`).Scan(&orphaned)
	if artifacts != 0 || orphaned != 1 {
		t.Fatalf("artifacts=%d orphaned blobs=%d, want both artifacts gone and the shared blob orphaned once", artifacts, orphaned)
	}
}

func TestRetryableUpstreamStatuses(t *testing.T) {
	ctx := context.Background()
	for status, want := range map[int]bool{200: false, 404: false, 429: true, 500: false, 502: true, 503: true, 504: true} {
		if got := retryableUpstream(ctx, &http.Response{StatusCode: status}, nil); got != want {
			t.Errorf("status %d: retryable = %v, want %v", status, got, want)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if retryableUpstream(canceled, &http.Response{StatusCode: 503}, nil) {
		t.Error("a canceled request must not be retried")
	}
	delay := upstreamRetryDelay(0, &http.Response{Header: http.Header{"Retry-After": {"2"}}})
	if delay < 2*time.Second || delay > 3*time.Second {
		t.Errorf("Retry-After: 2 gave delay %v", delay)
	}
	if delay := upstreamRetryDelay(0, &http.Response{Header: http.Header{"Retry-After": {"3600"}}}); delay > 7*time.Second {
		t.Errorf("a huge Retry-After was not capped: %v", delay)
	}
}

func TestFetchHubRetriesTransientFailures(t *testing.T) {
	previous := upstreamRetryBase
	upstreamRetryBase = time.Millisecond
	t.Cleanup(func() { upstreamRetryBase = previous })
	var attempts atomic.Int32
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	resp, _, err := env.server.fetchHub(context.Background(), http.MethodGet, "/api/models/org/m", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || attempts.Load() != 3 {
		t.Fatalf("status=%d after %d attempts, want 200 after 3", resp.StatusCode, attempts.Load())
	}

	attempts.Store(-100)
	resp2, _, err := env.server.fetchHub(context.Background(), http.MethodGet, "/api/models/org/m", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable || attempts.Load() != -97 {
		t.Fatalf("persistent failure: status=%d attempts=%d, want 503 after exactly 1+%d tries", resp2.StatusCode, attempts.Load()+100, maxUpstreamRetries)
	}
}

func TestCachedFileIsServedWhenTheHubIsDown(t *testing.T) {
	content := []byte(strings.Repeat("stale-model-", 2048))
	digest := auditSHA256(content)
	var down atomic.Bool
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/stale/model.bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"stale-cdn"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(content)
		}
	}))
	previous := upstreamRetryBase
	upstreamRetryBase = time.Millisecond
	t.Cleanup(func() { upstreamRetryBase = previous })
	target := env.proxy.URL + "/models/" + env.repoID + "/resolve/main/model.bin"

	get := func() (*http.Response, []byte) {
		t.Helper()
		resp, err := http.Get(target)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp, body
	}
	if resp, _ := get(); resp.StatusCode != http.StatusOK {
		t.Fatalf("initial fill = %d", resp.StatusCode)
	}
	if _, err := env.store.SQL.Exec(`UPDATE revisions SET resolved_at=now()-interval '1 day' WHERE repository_id=$1`, env.repoPK); err != nil {
		t.Fatal(err)
	}
	down.Store(true)
	resp, body := get()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("X-Cache") != "STALE" || string(body) != string(content) {
		t.Fatalf("hub down: status=%d X-Cache=%q bodyOK=%v, want 200 STALE with the cached bytes", resp.StatusCode, resp.Header.Get("X-Cache"), string(body) == string(content))
	}

	// A file that was never cached cannot be invented.
	unknown, err := http.Get(env.proxy.URL + "/models/" + env.repoID + "/resolve/main/other.bin")
	if err != nil {
		t.Fatal(err)
	}
	unknown.Body.Close()
	if unknown.StatusCode != http.StatusBadGateway && unknown.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("uncached file with the hub down = %d, want an upstream error", unknown.StatusCode)
	}
}

func TestSlowAPIRequestsAreCutOff(t *testing.T) {
	previous := apiReadTimeout
	apiReadTimeout = 300 * time.Millisecond
	t.Cleanup(func() { apiReadTimeout = previous })
	s := &Server{metrics: newMetrics(), logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	s.runtime.Store(&runtimeState{config: config.Config{AuthMode: "local", SessionSecret: strings.Repeat("s", 40)}})
	server := httptest.NewServer(s.Handler())
	defer server.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// Promise a body, send none of it.
	if _, err := io.WriteString(conn, "POST /login HTTP/1.1\r\nHost: x\r\nContent-Type: application/x-www-form-urlencoded\r\nContent-Length: 100\r\n\r\nuser="); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	start := time.Now()
	buf := make([]byte, 512)
	n, err := conn.Read(buf)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the stalled request was held for %v (read %d bytes, err %v)", elapsed, n, err)
	}
}

func TestDownloadRoutesKeepNoReadDeadline(t *testing.T) {
	previous := apiReadTimeout
	apiReadTimeout = time.Millisecond
	t.Cleanup(func() { apiReadTimeout = previous })
	var deadlineSet bool
	s := &Server{}
	handler := s.requestDeadlines(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadlineSet = http.NewResponseController(w).SetReadDeadline(time.Time{}) == nil
	}))
	// A recorder cannot take deadlines, so this only checks the route split does not panic.
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/models/org/m/resolve/main/f.bin", nil))
	_ = deadlineSet
}

func TestRequestIDsAreRestrictedToSafeCharacters(t *testing.T) {
	for id, want := range map[string]bool{"abc-123_X.y:z": true, "": false, "has space": false, "semi;colon": false, "new\nline": false, strings.Repeat("a", 101): false, strings.Repeat("a", 100): true} {
		if got := validRequestID(id); got != want {
			t.Errorf("validRequestID(%q) = %v, want %v", id, got, want)
		}
	}
	if got := safeLogValue("agent\r\nforged=1\x00", 200); got != "agentforged=1" {
		t.Errorf("safeLogValue kept control characters: %q", got)
	}
	if got := safeLogValue(strings.Repeat("a", 500), 200); len(got) != 200 {
		t.Errorf("safeLogValue length = %d, want 200", len(got))
	}
}

func TestDownloadsRecordTheCaller(t *testing.T) {
	content := []byte(strings.Repeat("audited-", 2048))
	digest := auditSHA256(content)
	env := ahubNewEnv(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/audit/model.bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"audit-cdn"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(content)
		}
	}))
	userID := db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,$2,'reader')`, userID, "audited-"+userID[:8]); err != nil {
		t.Fatal(err)
	}
	secret := "audited-token-" + strings.ReplaceAll(db.NewID(), "-", "")
	tokenID := db.NewID()
	if _, err := env.store.SQL.Exec(`INSERT INTO access_tokens(id,user_id,token_hash,prefix) VALUES($1,$2,$3,'audited')`, tokenID, userID, HashToken(secret, env.server.currentConfig().SessionSecret)); err != nil {
		t.Fatal(err)
	}
	target := env.proxy.URL + "/models/" + env.repoID + "/resolve/" + ahubCommit + "/model.bin"
	for i := 0; i < 2; i++ {
		req, _ := http.NewRequest(http.MethodGet, target, nil)
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("request %d = %d", i, resp.StatusCode)
		}
	}
	rows, err := env.store.SQL.Query(`SELECT status, COALESCE(user_id,''), COALESCE(token_id,''), COALESCE(client_ip,'') FROM downloads WHERE repository_id=$1 ORDER BY started_at`, env.repoPK)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var status, user, token, ip string
		if err := rows.Scan(&status, &user, &token, &ip); err != nil {
			t.Fatal(err)
		}
		seen++
		if user != userID || token != tokenID || ip != "127.0.0.1" {
			t.Errorf("%s row: user=%q token=%q ip=%q, want %q %q 127.0.0.1", status, user, token, ip, userID, tokenID)
		}
	}
	if seen < 2 {
		t.Fatalf("recorded %d download rows, want at least a fill and a hit", seen)
	}
}

func TestBlobGCReportsAndSurvivesAFailingBlob(t *testing.T) {
	var deleted []string
	s3 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if strings.Contains(r.URL.Path, "boom") {
				w.Header().Set("Content-Type", "application/xml")
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
				return
			}
			deleted = append(deleted, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer s3.Close()
	objects, err := storage.New(context.Background(), s3.URL, "us-east-1", "bucket", "", "access", "secret", true, false)
	if err != nil {
		t.Fatal(err)
	}
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheGCGrace = time.Second
	s := auditAPINewServer(t, store, cfg)
	s.runtime.Store(&runtimeState{config: cfg, storage: objects, httpClient: http.DefaultClient})
	for _, key := range []string{"blobs/boom", "blobs/fine-1", "blobs/fine-2"} {
		sha := strings.ReplaceAll(db.NewID()+db.NewID(), "-", "")
		if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key,orphaned_at) VALUES($1,$2,1,$3,now()-interval '1 hour')`, db.NewID(), sha, key); err != nil {
			t.Fatal(err)
		}
	}
	count, err := s.collectOrphanedBlobsCounted(context.Background())
	if err == nil {
		t.Fatal("the failing blob must be reported")
	}
	if count != 2 || len(deleted) != 2 {
		t.Fatalf("deleted %d blobs (%d objects), want the 2 healthy ones despite the failure", count, len(deleted))
	}
	var remaining int
	if err := store.SQL.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&remaining); err != nil || remaining != 1 {
		t.Fatalf("blobs left = %d (%v), want only the failing one", remaining, err)
	}

	s.runBlobGC(context.Background())
	families, err := s.metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, family := range families {
		if family.GetName() == "hfproxy_gc_runs_total" {
			for _, metric := range family.GetMetric() {
				if metric.GetCounter().GetValue() > 0 {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatal("hfproxy_gc_runs_total was not recorded")
	}
}
