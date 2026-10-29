package server

import (
	"net/http"

	"github.com/hfproxy/hfproxy/internal/emojiver"
)

// apiVersion returns the running version and its EmojiVer forms. The emoji
// travels in a header as an RFC 8187 ext-value, since header values are ASCII.
func (s *Server) apiVersion(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to view the version.")
		return
	}
	body := map[string]string{"semver": Version, "commit": Commit}
	if release, ok := runningRelease(); ok {
		body["emoji"], body["tag"], body["a11y"] = release.String(), release.Tag(), release.A11y()
		w.Header().Set("X-HFProxy-Version-Emoji", emojiver.HeaderValue(release.String()))
	}
	writeJSON(w, http.StatusOK, body)
}

// runningRelease prefers the string embedded by CI and falls back to a local
// rendering when it is missing or does not belong to this build.
func runningRelease() (emojiver.Release, bool) {
	if release, err := emojiver.Resolve(Version, Commit, Emoji); err == nil {
		return release, true
	}
	release, err := emojiver.Resolve(Version, Commit, "")
	return release, err == nil
}
