package server

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func seedPinnedArtifact(t *testing.T, store *db.Store, repo, commit, name string, size int64, age string) string {
	t.Helper()
	blob := db.NewID()
	sha := strings.ReplaceAll(db.NewID()+db.NewID(), "-", "")
	if _, err := store.SQL.Exec(`INSERT INTO blobs(id,sha256,size_bytes,storage_key) VALUES($1,$2,$3,$4)`, blob, sha, size, "blobs/"+sha); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,last_accessed_at) VALUES($1,$2,$3,$4,$5,now()-$6::interval)`,
		db.NewID(), repo, commit, name, blob, age); err != nil {
		t.Fatal(err)
	}
	return blob
}

func seedRepository(t *testing.T, store *db.Store, repoID string) string {
	t.Helper()
	id := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models',$2,'public')`, id, repoID); err != nil {
		t.Fatal(err)
	}
	return id
}

func remainingArtifacts(t *testing.T, store *db.Store) string {
	t.Helper()
	rows, err := store.SQL.Query(`SELECT path FROM artifacts ORDER BY path`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		_ = rows.Scan(&name)
		names = append(names, name)
	}
	return strings.Join(names, ",")
}

const (
	pinCommitA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pinCommitB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestEvictionNeverTouchesPinnedRepositoriesOrCommits(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 25
	s := auditAPINewServer(t, store, cfg)

	pinnedRepo := seedRepository(t, store, "org/pinned-repo")
	mixedRepo := seedRepository(t, store, "org/mixed")
	plainRepo := seedRepository(t, store, "org/plain")
	// The pinned files are also the oldest, which is exactly what LRU would drop first.
	seedPinnedArtifact(t, store, pinnedRepo, pinCommitA, "pinned-repo-oldest", 10, "30 days")
	seedPinnedArtifact(t, store, mixedRepo, pinCommitA, "pinned-commit", 10, "20 days")
	seedPinnedArtifact(t, store, mixedRepo, pinCommitB, "unpinned-commit", 10, "10 days")
	seedPinnedArtifact(t, store, plainRepo, pinCommitA, "plain-oldest", 10, "5 days")
	seedPinnedArtifact(t, store, plainRepo, pinCommitA, "plain-newest", 10, "1 day")
	if _, err := store.SQL.Exec(`INSERT INTO cache_pins(repository_id,commit_sha) VALUES($1,''),($2,$3)`, pinnedRepo, mixedRepo, pinCommitA); err != nil {
		t.Fatal(err)
	}

	if pinned, err := s.pinnedBytes(context.Background()); err != nil || pinned != 20 {
		t.Fatalf("pinned bytes = %d (%v), want 20", pinned, err)
	}
	// 50 bytes over a 25 byte quota: only the 30 unpinned bytes can go.
	evicted, err := s.evictOverQuota(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if evicted != 3 {
		t.Fatalf("evicted %d artifacts, want the 3 unpinned ones", evicted)
	}
	if got := remainingArtifacts(t, store); got != "pinned-commit,pinned-repo-oldest" {
		t.Fatalf("remaining = %q, want only the pinned files", got)
	}
	// Pinned content above the quota must not make eviction loop or fail.
	if again, err := s.evictOverQuota(context.Background()); err != nil || again != 0 {
		t.Fatalf("second pass evicted %d (%v), want 0", again, err)
	}
}

func TestUnpinningMakesFilesEvictableAgain(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 5
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/toggle")
	seedPinnedArtifact(t, store, repo, pinCommitA, "file", 10, "1 day")
	if _, err := store.SQL.Exec(`INSERT INTO cache_pins(repository_id,commit_sha) VALUES($1,'')`, repo); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.evictOverQuota(context.Background()); n != 0 {
		t.Fatalf("a pinned file was evicted")
	}
	if _, err := store.SQL.Exec(`DELETE FROM cache_pins`); err != nil {
		t.Fatal(err)
	}
	if n, err := s.evictOverQuota(context.Background()); err != nil || n != 1 {
		t.Fatalf("after unpinning, evicted %d (%v), want 1", n, err)
	}
}

func TestPinAPILifecycleAndPermissions(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	repo := seedRepository(t, store, "org/api-pins")
	adminID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'pin-admin','admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	admin := &Principal{ID: adminID, Username: "pin-admin", Role: "admin", Authenticated: true, TokenID: "pin-token", Scopes: []string{"admin"}}
	reader := &Principal{ID: "reader", Username: "reader", Role: "reader", Authenticated: true, TokenID: "reader-token", Scopes: []string{"write"}}
	route := "/api/v1/repos/" + repo + "/pins"

	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scope":"repository"`) {
		t.Fatalf("pin the repository: %d %s", rec.Code, rec.Body.String())
	}
	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{"commit":"`+pinCommitA+`"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scope":"commit"`) {
		t.Fatalf("pin a commit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{"commit":"`+pinCommitA+`"}`); rec.Code != http.StatusOK {
		t.Fatalf("pinning twice must be harmless: %d", rec.Code)
	}
	list := auditAPIDoAs(s, admin, http.MethodGet, route, "")
	if list.Code != http.StatusOK || strings.Count(list.Body.String(), `"scope"`) != 2 {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	for _, bad := range []string{`{"commit":"main"}`, `{"commit":"abc"}`, `{"unknown":1}`} {
		if rec := auditAPIDoAs(s, admin, http.MethodPost, route, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
	if rec := auditAPIDoAs(s, reader, http.MethodPost, route, `{}`); rec.Code != http.StatusForbidden {
		t.Errorf("a reader pinned a repository: %d", rec.Code)
	}
	if rec := auditAPIDoAs(s, admin, http.MethodGet, "/api/v1/repos/missing/pins", ""); rec.Code != http.StatusNotFound {
		t.Errorf("unknown repository = %d, want 404", rec.Code)
	}
	if rec := auditAPIDoAs(s, admin, http.MethodPatch, route, `{}`); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("PATCH = %d, want 405", rec.Code)
	}

	if rec := auditAPIDoAs(s, admin, http.MethodDelete, route+"?commit="+pinCommitA, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unpin the commit: %d %s", rec.Code, rec.Body.String())
	}
	if rec := auditAPIDoAs(s, admin, http.MethodDelete, route+"?commit="+pinCommitA, ""); rec.Code != http.StatusNotFound {
		t.Errorf("unpinning a missing pin = %d, want 404", rec.Code)
	}
	if rec := auditAPIDoAs(s, admin, http.MethodDelete, route, ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unpin the repository: %d", rec.Code)
	}
	var left, events int
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM cache_pins`).Scan(&left)
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action IN ('cache.pin','cache.unpin')`).Scan(&events)
	if left != 0 || events != 4 {
		t.Fatalf("pins left = %d, audit events = %d, want 0 and 4 (two pins, two unpins)", left, events)
	}
}

func TestDeletingARepositoryRemovesItsPins(t *testing.T) {
	store := auditAPIFreshStore(t)
	repo := seedRepository(t, store, "org/gone")
	if _, err := store.SQL.Exec(`INSERT INTO cache_pins(repository_id,commit_sha) VALUES($1,'')`, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := store.SQL.Exec(`DELETE FROM repositories WHERE id=$1`, repo); err != nil {
		t.Fatal(err)
	}
	var left int
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM cache_pins`).Scan(&left)
	if left != 0 {
		t.Fatalf("%d pins outlived their repository", left)
	}
}

func TestPinnedBranchFollowsTheCommitItPointsTo(t *testing.T) {
	store := auditAPIFreshStore(t)
	cfg := auditAPIConfig()
	cfg.CacheMaxTotalSize = 5
	s := auditAPINewServer(t, store, cfg)
	repo := seedRepository(t, store, "org/branch")
	seedPinnedArtifact(t, store, repo, pinCommitA, "old-main", 10, "5 days")
	seedPinnedArtifact(t, store, repo, pinCommitB, "new-main", 10, "9 days")
	if _, err := store.SQL.Exec(`INSERT INTO cache_pins(repository_id,commit_sha,ref) VALUES($1,'','main')`, repo); err != nil {
		t.Fatal(err)
	}

	// A branch nobody has requested yet points nowhere, so it protects nothing.
	if pinned, _ := s.pinnedBytes(context.Background()); pinned != 0 {
		t.Fatalf("an unresolved branch pinned %d bytes", pinned)
	}

	if _, err := store.SQL.Exec(`INSERT INTO revisions(id,repository_id,requested_ref,commit_sha) VALUES($1,$2,'main',$3)`, db.NewID(), repo, pinCommitA); err != nil {
		t.Fatal(err)
	}
	if pinned, _ := s.pinnedBytes(context.Background()); pinned != 10 {
		t.Fatalf("pinned bytes = %d, want the 10 of the commit main points to", pinned)
	}
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remainingArtifacts(t, store); got != "old-main" {
		t.Fatalf("remaining = %q, want only the commit main points to (the other was older in use but unpinned)", got)
	}

	// The branch moves: what it used to point to is no longer protected.
	seedPinnedArtifact(t, store, repo, pinCommitB, "newer-main", 10, "1 day")
	if _, err := store.SQL.Exec(`UPDATE revisions SET commit_sha=$1 WHERE repository_id=$2 AND requested_ref='main'`, pinCommitB, repo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.evictOverQuota(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := remainingArtifacts(t, store); got != "newer-main" {
		t.Fatalf("remaining = %q, want the commit main points to now", got)
	}
}

func TestPinAPIAcceptsBranchesAndRejectsAmbiguousPins(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	repo := seedRepository(t, store, "org/api-ref")
	adminID := db.NewID()
	if _, err := store.SQL.Exec(`INSERT INTO users(id,username,role) VALUES($1,'ref-admin','admin')`, adminID); err != nil {
		t.Fatal(err)
	}
	admin := &Principal{ID: adminID, Username: "ref-admin", Role: "admin", Authenticated: true, TokenID: "ref-token", Scopes: []string{"admin"}}
	route := "/api/v1/repos/" + repo + "/pins"

	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{"ref":"main"}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scope":"ref"`) {
		t.Fatalf("pin a branch: %d %s", rec.Code, rec.Body.String())
	}
	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{"ref":"refs/pr/12"}`); rec.Code != http.StatusOK {
		t.Fatalf("pin a ref with a slash: %d", rec.Code)
	}
	if rec := auditAPIDoAs(s, admin, http.MethodPost, route, `{}`); rec.Code != http.StatusOK {
		t.Fatalf("a branch pin and a repository pin must coexist: %d", rec.Code)
	}
	for _, bad := range []string{`{"ref":"main","commit":"` + pinCommitA + `"}`, `{"ref":"` + pinCommitA + `"}`, `{"ref":"../x"}`, `{"ref":"a b"}`} {
		if rec := auditAPIDoAs(s, admin, http.MethodPost, route, bad); rec.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", bad, rec.Code)
		}
	}
	list := auditAPIDoAs(s, admin, http.MethodGet, route, "")
	if strings.Count(list.Body.String(), `"scope"`) != 3 || !strings.Contains(list.Body.String(), `"ref":"main"`) {
		t.Fatalf("list: %s", list.Body.String())
	}
	if rec := auditAPIDoAs(s, admin, http.MethodDelete, route+"?ref=main", ""); rec.Code != http.StatusNoContent {
		t.Fatalf("unpin the branch: %d", rec.Code)
	}
	if rec := auditAPIDoAs(s, admin, http.MethodDelete, route+"?ref=main", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unpinning twice = %d, want 404", rec.Code)
	}
	var whole int
	_ = store.SQL.QueryRow(`SELECT COUNT(*) FROM cache_pins WHERE commit_sha='' AND ref=''`).Scan(&whole)
	if whole != 1 {
		t.Fatal("removing a branch pin removed the repository-wide pin")
	}
}
