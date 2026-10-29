package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/emojiver"
)

func withBuild(t *testing.T, version, commit, emoji string) {
	t.Helper()
	oldVersion, oldCommit, oldEmoji := Version, Commit, Emoji
	Version, Commit, Emoji = version, commit, emoji
	t.Cleanup(func() { Version, Commit, Emoji = oldVersion, oldCommit, oldEmoji })
}

func TestVersionEndpointReturnsEmojiVer(t *testing.T) {
	const commit = "4fcdcfc2b1f0a6c1d3e5f7a9b0c2d4e6f8a0b1c3"
	built, err := emojiver.Build("0.1.0", commit, time.Date(2024, 4, 23, 23, 49, 0, 0, time.UTC), emojiver.Feature, emojiver.Green)
	if err != nil {
		t.Fatal(err)
	}
	withBuild(t, "0.1.0", commit, built.String())
	s := auditAPINewServer(t, nil, auditAPIConfig())
	rec := auditAPIDoAs(s, auditAPIAdmin(), http.MethodGet, "/api/v1/version", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body)
	}
	var got map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["semver"] != "0.1.0" || got["emoji"] != built.String() || got["tag"] != built.Tag() || got["a11y"] != built.A11y() {
		t.Fatalf("body = %v", got)
	}
	if h := rec.Header().Get("X-HFProxy-Version-Emoji"); h != emojiver.HeaderValue(built.String()) {
		t.Fatalf("X-HFProxy-Version-Emoji = %q", h)
	}
}

func TestVersionEndpointIgnoresAForeignEmbeddedString(t *testing.T) {
	const commit = "4fcdcfc2b1f0a6c1d3e5f7a9b0c2d4e6f8a0b1c3"
	foreign, err := emojiver.Build("0.0.3", commit, time.Time{}, emojiver.Fix, emojiver.Green)
	if err != nil {
		t.Fatal(err)
	}
	withBuild(t, "0.1.0", commit, foreign.String())
	s := auditAPINewServer(t, nil, auditAPIConfig())
	rec := auditAPIDoAs(s, auditAPIAdmin(), http.MethodGet, "/api/v1/version", "")
	var got map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if !strings.HasPrefix(got["emoji"], "🐙1️⃣🔹0️⃣❔🌚⚪") {
		t.Fatalf("emoji = %q, want the local rendering of 0.1.0", got["emoji"])
	}
}

func TestVersionEndpointForDevBuildsHasNoEmoji(t *testing.T) {
	withBuild(t, "dev", "unknown", "")
	s := auditAPINewServer(t, nil, auditAPIConfig())
	rec := auditAPIDoAs(s, auditAPIAdmin(), http.MethodGet, "/api/v1/version", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"semver":"dev"`) || rec.Header().Get("X-HFProxy-Version-Emoji") != "" {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body)
	}
}

func TestVersionEndpointRequiresAuthentication(t *testing.T) {
	s := auditAPINewServer(t, nil, auditAPIConfig())
	if rec := auditAPIDo(s, http.MethodGet, "/api/v1/version", "", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status = %d, want 401", rec.Code)
	}
}
