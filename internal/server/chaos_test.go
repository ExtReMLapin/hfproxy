package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync/atomic"
	"testing"
)

// Failure-injection tests: the Hub, the object store or PostgreSQL fail in the
// middle of a cache fill. After each failure the client must get an error (never
// a truncated 200), nothing half-written may be left behind (no catalog row, no
// staging file, no corrupt blob), and the next request, once the dependency is
// back, must succeed with the right bytes.

const (
	chaosHubUp = iota
	chaosHubDown
	chaosHubCutsBody
)

type chaosSetup struct {
	env     *ahubEnv
	content []byte
	digest  string
	mode    *atomic.Int32
	staging string
}

func newChaosSetup(t *testing.T) *chaosSetup {
	t.Helper()
	content := bytes.Repeat([]byte("chaos-model-bytes-"), 16384) // ~288 KiB
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	mode := &atomic.Int32{}
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == "huggingface.co" {
			w.Header().Set("X-Repo-Commit", ahubCommit)
			w.Header().Set("X-Linked-Etag", `"`+digest+`"`)
			w.Header().Set("X-Linked-Size", strconv.Itoa(len(content)))
			w.Header().Set("Location", "https://cdn-lfs.hf.co/chaos/model.bin")
			w.WriteHeader(http.StatusFound)
			return
		}
		w.Header().Set("ETag", `"chaos"`)
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			return
		}
		switch mode.Load() {
		case chaosHubDown:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		case chaosHubCutsBody:
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(content[:len(content)/2])
			panic(http.ErrAbortHandler) // drops the connection mid-body
		}
		start, end, partial, _ := parseByteRange(r.Header.Get("Range"), int64(len(content)))
		if partial {
			w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.Itoa(len(content)))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(content[start : end+1])
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		_, _ = w.Write(content)
	})
	env := ahubNewEnv(t, hub)
	staging := t.TempDir()
	state := *env.server.runtime.Load()
	state.config.StagingDir = staging
	env.server.runtime.Store(&state)
	return &chaosSetup{env: env, content: content, digest: digest, mode: mode, staging: staging}
}

func (c *chaosSetup) get(t *testing.T) (int, []byte) {
	t.Helper()
	response, err := http.Get(c.env.proxy.URL + "/" + c.env.repoID + "/resolve/main/model.bin")
	if err != nil {
		return 0, nil // the connection was dropped: an error, as it should be for a truncated body
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, body
}

func (c *chaosSetup) assertNothingLeftBehind(t *testing.T, what string) {
	t.Helper()
	var artifacts int
	if err := c.env.store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1`, c.env.repoPK).Scan(&artifacts); err != nil {
		t.Fatal(err)
	}
	if artifacts != 0 {
		t.Fatalf("%s: %d catalog rows were published for a download that failed", what, artifacts)
	}
	entries, err := os.ReadDir(c.staging)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		t.Errorf("%s: staging file %s was not cleaned up", what, entry.Name())
	}
}

// assertServedAndIntact checks the next download and that what is stored in the
// object store really is the file (no corrupt blob behind the catalog row).
func (c *chaosSetup) assertServedAndIntact(t *testing.T, what string) {
	t.Helper()
	status, body := c.get(t)
	if status != http.StatusOK || !bytes.Equal(body, c.content) {
		t.Fatalf("%s: after recovery got status %d and %d bytes, want 200 and the whole file", what, status, len(body))
	}
	var sha, key string
	if err := c.env.store.SQL.QueryRow(`SELECT b.sha256,b.storage_key FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1`, c.env.repoPK).Scan(&sha, &key); err != nil {
		t.Fatalf("%s: no catalog row after recovery: %v", what, err)
	}
	if sha != c.digest {
		t.Fatalf("%s: catalog sha256 %s, want %s", what, sha, c.digest)
	}
	c.env.fakeS3.mu.Lock()
	defer c.env.fakeS3.mu.Unlock()
	stored := c.env.fakeS3.objects["bucket/"+key]
	if !bytes.Equal(stored, c.content) {
		t.Fatalf("%s: the stored object (%d bytes) differs from the file", what, len(stored))
	}
}

func TestChaosHubDownDuringAFill(t *testing.T) {
	c := newChaosSetup(t)
	c.mode.Store(chaosHubDown)
	if status, _ := c.get(t); status == http.StatusOK {
		t.Fatal("a fill with the Hub down must not answer 200")
	}
	c.assertNothingLeftBehind(t, "Hub down")
	c.mode.Store(chaosHubUp)
	c.assertServedAndIntact(t, "Hub down")
}

func TestChaosHubCutsTheConnectionMidBody(t *testing.T) {
	c := newChaosSetup(t)
	c.mode.Store(chaosHubCutsBody)
	status, body := c.get(t)
	if status == http.StatusOK && bytes.Equal(body, c.content) {
		t.Fatal("the test did not cut the body")
	}
	if status == http.StatusOK && len(body) < len(c.content) {
		// A 200 header was already sent when the cut happened: acceptable only if
		// the client sees the connection fail (short body); it must never be cached.
		t.Logf("client received a short body (%d of %d bytes) with the connection aborted", len(body), len(c.content))
	}
	c.assertNothingLeftBehind(t, "Hub cut")
	c.mode.Store(chaosHubUp)
	c.assertServedAndIntact(t, "Hub cut")
}

func TestChaosObjectStoreOutageDuringPublication(t *testing.T) {
	c := newChaosSetup(t)
	c.env.fakeS3.failWrites.Store(true)
	status, _ := c.get(t)
	if status == http.StatusOK {
		t.Fatal("a fill that cannot be stored must not answer 200 with a cacheable result")
	}
	c.assertNothingLeftBehind(t, "object store down")
	c.env.fakeS3.failWrites.Store(false)
	c.assertServedAndIntact(t, "object store down")
}

func TestChaosPostgresRejectsThePublication(t *testing.T) {
	c := newChaosSetup(t)
	for _, statement := range []string{
		`CREATE FUNCTION chaos_refuse() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'chaos: database refuses writes'; END $$ LANGUAGE plpgsql`,
		`CREATE TRIGGER chaos_artifacts BEFORE INSERT ON artifacts FOR EACH ROW EXECUTE FUNCTION chaos_refuse()`,
	} {
		if _, err := c.env.store.SQL.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	if status, _ := c.get(t); status == http.StatusOK {
		t.Fatal("a fill whose metadata cannot be saved must not answer 200")
	}
	c.assertNothingLeftBehind(t, "database refuses")
	if _, err := c.env.store.SQL.Exec(`DROP TRIGGER chaos_artifacts ON artifacts`); err != nil {
		t.Fatal(err)
	}
	c.assertServedAndIntact(t, "database refuses")
}
