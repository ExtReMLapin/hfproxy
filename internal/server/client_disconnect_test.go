package server

import (
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"
)

// A client that hangs up the moment it has the last byte (curl, a connection
// that is not reused) must not abort the cache fill that is still storing the
// file it just received, nor leave the download "in progress" forever.
func TestFillSurvivesTheClientHangingUpAfterTheLastByte(t *testing.T) {
	content := []byte(`{"model_type":"bert","hidden_size":768}`)
	hub := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Repo-Commit", ahubCommit)
		w.Header().Set("ETag", `"small-config"`)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if r.Method == http.MethodGet {
			_, _ = w.Write(content)
		}
	})
	env := ahubNewEnv(t, hub)
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	response, err := client.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close() // hang up right away
	if response.StatusCode != http.StatusOK || string(body) != string(content) {
		t.Fatalf("first download = %d %q", response.StatusCode, body)
	}

	deadline := time.Now().Add(5 * time.Second)
	var artifacts, inProgress int
	for time.Now().Before(deadline) {
		env.store.SQL.QueryRow(`SELECT count(*) FROM artifacts WHERE repository_id=$1`, env.repoPK).Scan(&artifacts)
		env.store.SQL.QueryRow(`SELECT count(*) FROM downloads WHERE repository_id=$1 AND status='downloading'`, env.repoPK).Scan(&inProgress)
		if artifacts == 1 && inProgress == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if artifacts != 1 {
		t.Fatalf("%d files were cached, want 1: the fill was aborted when the client hung up", artifacts)
	}
	if inProgress != 0 {
		t.Fatalf("%d downloads are still marked in progress", inProgress)
	}
	second, err := client.Get(env.proxy.URL + "/" + env.repoID + "/resolve/main/config.json")
	if err != nil {
		t.Fatal(err)
	}
	second.Body.Close()
	if got := second.Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("second download X-Cache = %q, want HIT", got)
	}
}

func TestAbandonedDownloadsAreClosedByRetention(t *testing.T) {
	store := auditAPIFreshStore(t)
	s := auditAPINewServer(t, store, auditAPIConfig())
	for name, age := range map[string]string{"stale": "13 hours", "running": "5 minutes"} {
		if _, err := store.SQL.Exec(`INSERT INTO downloads(id,path,status,started_at) VALUES($1,$2,'downloading',now()-$3::interval)`, name, name, age); err != nil {
			t.Fatal(err)
		}
	}
	s.runRetention(t.Context())
	status := func(id string) (string, string) {
		var st, class string
		if err := store.SQL.QueryRow(`SELECT status,error_class FROM downloads WHERE id=$1`, id).Scan(&st, &class); err != nil {
			t.Fatal(err)
		}
		return st, class
	}
	if st, class := status("stale"); st != "failed" || class != "abandoned" {
		t.Fatalf("a download in progress for 13 hours = %q/%q, want failed/abandoned", st, class)
	}
	if st, _ := status("running"); st != "downloading" {
		t.Fatalf("a recent download was closed: %q", st)
	}
}
