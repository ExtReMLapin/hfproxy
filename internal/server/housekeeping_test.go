package server

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
	"github.com/hfproxy/hfproxy/internal/keyring"
)

func TestFinishedPreloadsAreForgottenAfterADay(t *testing.T) {
	now := time.Now()
	jobs := map[string]*preloadJob{
		"old-done":    {Status: "completed", UpdatedAt: now.Add(-25 * time.Hour)},
		"old-failed":  {Status: "failed", UpdatedAt: now.Add(-48 * time.Hour)},
		"recent":      {Status: "completed", UpdatedAt: now.Add(-time.Hour)},
		"old-running": {Status: "running", UpdatedAt: now.Add(-72 * time.Hour)},
		"old-queued":  {Status: "queued", UpdatedAt: now.Add(-72 * time.Hour)},
	}
	pruneFinishedPreloads(jobs, now)
	if len(jobs) != 3 || jobs["old-done"] != nil || jobs["old-failed"] != nil {
		t.Fatalf("kept %v, want the recent job and the two unfinished ones", jobs)
	}
}

func TestNegativeCacheStaysBoundedAndCheapUnderAFlood(t *testing.T) {
	s := auditAPINewServer(t, nil, auditAPIConfig())
	cfg := s.currentConfig()
	cfg.CacheNegativeTTL = time.Minute
	state := s.runtime.Load()
	state.config = cfg
	begin := time.Now()
	for i := 0; i < 3*negativeCacheMaxEntries; i++ {
		s.negativeStore("flood-"+itoa(int64(i)), http.StatusNotFound, http.Header{})
	}
	if size := len(s.negative); size > negativeCacheMaxEntries {
		t.Fatalf("the cache holds %d entries, the bound is %d", size, negativeCacheMaxEntries)
	}
	if elapsed := time.Since(begin); elapsed > 3*time.Second {
		t.Fatalf("%d inserts took %s: each one scans the whole cache", 3*negativeCacheMaxEntries, elapsed)
	}
	// An entry that expired is still dropped when it is looked up.
	s.negative["stale"] = negativeCacheEntry{expires: time.Now().Add(-time.Second), status: 404}
	if _, _, ok := s.negativeLookup("stale"); ok {
		t.Fatal("an expired entry was served")
	}
}

// At a stop with the database unavailable, the queued hit events are given up
// after a budget instead of every batch waiting out its own timeout.
func TestStoppingDoesNotWaitForeverToWriteHitEvents(t *testing.T) {
	logs := &lockedBuffer{}
	s := &Server{logger: slog.New(slog.NewTextHandler(logs, nil))}
	h := &hitLog{events: make(chan db.DownloadEvent, 1024), done: make(chan struct{})}
	for i := 0; i < 500; i++ {
		h.events <- db.DownloadEvent{RepoID: "r", Status: "hit"}
	}
	previous := hitLogDrainBudget
	hitLogDrainBudget = -time.Second // already spent
	t.Cleanup(func() { hitLogDrainBudget = previous })
	begin := time.Now()
	s.drainHitLog(h)
	if time.Since(begin) > 2*time.Second {
		t.Fatal("the drain did not stop at its budget")
	}
	if !strings.Contains(logs.String(), "dropped at shutdown") {
		t.Fatalf("the dropped events were not reported:\n%s", logs.String())
	}
}

func TestAllCookiesAgreeOnTheSecureFlag(t *testing.T) {
	cfg := auditAPIConfig()
	cfg.PublicURL = "http://hfproxy.internal"
	cfg.OIDCRedirectURL = "https://hfproxy.example.com/auth/callback"
	for _, cookie := range newSessionCookies("token", "csrf", cfg) {
		if !cookie.Secure {
			t.Errorf("%s is not Secure although the sign-in goes through https", cookie.Name)
		}
	}
	s := auditAPINewServer(t, nil, cfg)
	rec := httptest.NewRecorder()
	s.login(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	found := false
	for _, cookie := range rec.Result().Cookies() {
		found = true
		if !cookie.Secure {
			t.Errorf("%s is not Secure", cookie.Name)
		}
	}
	if !found {
		t.Fatal("the login page set no cookie")
	}
	cfg.PublicURL, cfg.OIDCRedirectURL = "http://localhost:8080", ""
	if cookieSecure(cfg) {
		t.Error("a plain-http deployment must not set Secure cookies the browser would drop")
	}
}

// HashToken is the original token hash, HMAC-SHA256 keyed with the raw session
// secret, which tokens issued by older releases still carry. Production code
// only reads such hashes (keyring.Ring); tests write them to set up the case.
func HashToken(token, pepper string) []byte { return keyring.LegacyHash(token, pepper) }

// collectOrphanedBlobs runs one collection pass and returns its error.
func (s *Server) collectOrphanedBlobs(ctx context.Context) error {
	_, err := s.collectOrphanedBlobsCounted(ctx)
	return err
}
