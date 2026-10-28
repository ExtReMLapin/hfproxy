package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestArtifactResponsesAreSandboxed(t *testing.T) {
	rec := httptest.NewRecorder()
	(&Server{}).setArtifactHeaders(rec, artifact{ContentType: "text/html", Commit: "abc"}, http.Header{})
	if got := rec.Header().Get("Content-Security-Policy"); got != "sandbox" {
		t.Fatalf("Content-Security-Policy = %q, want sandbox", got)
	}
	if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
