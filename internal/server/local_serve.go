package server

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/hfproxy/hfproxy/internal/db"
)

// serveLocalFile answers /{repo}/resolve/{revision}/{path} for a repository
// published on this proxy: the file comes from the commit the revision names,
// with the headers the Hub's clients read (X-Repo-Commit, ETag, and for large
// files X-Linked-Etag and X-Linked-Size), range requests and conditional
// requests included.
func (s *Server) serveLocalFile(w http.ResponseWriter, r *http.Request, parsed resolvePath, grant repositoryGrant, p *Principal) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowedWith(w, "GET, HEAD")
		return
	}
	r, w, release, admitted := s.admitDownload(w, r, p)
	if !admitted {
		return
	}
	defer release()
	sha, err := s.db.ResolveLocalRevision(r.Context(), grant.ID, parsed.Revision)
	if errors.Is(err, db.ErrRevisionNotFound) {
		hfError(w, http.StatusNotFound, "RevisionNotFound", "Invalid rev id: "+parsed.Revision)
		return
	}
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The revision could not be read.")
		return
	}
	file, err := s.db.LocalFileAt(r.Context(), grant.ID, sha, parsed.FilePath)
	if errors.Is(err, db.ErrNotFound) {
		w.Header().Set("X-Repo-Commit", sha)
		hfError(w, http.StatusNotFound, "EntryNotFound", "Entry not found")
		return
	}
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The file could not be read.")
		return
	}
	a := artifact{
		ID: file.ArtifactID, Repository: grant.ID, Commit: sha, Path: file.Path, BlobID: file.BlobID, StorageKey: file.StorageKey,
		SHA256: file.SHA256, Size: file.Size, ETag: `"` + file.OID + `"`, ContentType: contentTypeFor(file.Path),
		LastMod: sql.NullTime{Time: file.LastModified, Valid: true},
	}
	record := func() {
		s.recordDownload(r.Context(), grant.ID, sha, file.Path, "hit", file.Size, "")
	}
	if s.serveOrAcknowledgePreloadReporting(w, r, a, "LOCAL", record) {
		// The catalog lists the file but its content is gone from the bucket.
		// There is no Hub to fetch it from again: say so plainly.
		s.logger.Error("the content of a published file is missing from object storage", "request_id", requestID(r.Context()), "repo_id", parsed.RepoID, "path", safeLogValue(file.Path, 300), "sha256", file.SHA256)
		hfError(w, http.StatusBadGateway, "", "The stored content of this file is missing. Ask an administrator to check the object storage.")
	}
}
