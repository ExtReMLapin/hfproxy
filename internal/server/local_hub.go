package server

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git object names are SHA-1; they identify content, they do not protect it.
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

// Models and datasets published on this proxy are served through the same HTTP
// API the Hugging Face Hub offers, so that the Hub's own clients (huggingface_hub,
// the hf command, transformers, datasets) work against them unchanged. This file
// holds the routing, the Hub's error format, who may publish where, and the
// repository-level routes; local_upload.go holds the upload protocol (preupload,
// Git LFS, commit) and local_serve.go the file downloads.

// localRoute is /api/{models|datasets}/{namespace}/{name}/{rest...}.
type localRoute struct {
	Type, Namespace, Name, RepoID string
	Rest                          []string
}

// parseLocalRoute splits a Hub API path. Published repositories are always
// "namespace/name", which is what lets the rest of the path be told from the
// repository (Hub repositories cached here can have a single-segment name).
func parseLocalRoute(escaped string) (localRoute, bool) {
	parts := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || (parts[1] != "models" && parts[1] != "datasets") {
		return localRoute{}, false
	}
	decoded := make([]string, len(parts))
	for i, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil || hasControl(value) {
			return localRoute{}, false
		}
		decoded[i] = value
	}
	route := localRoute{Type: parts[1], Namespace: decoded[2], Name: decoded[3], Rest: decoded[4:]}
	if strings.ContainsAny(route.Namespace+route.Name, "/\\") {
		return localRoute{}, false
	}
	route.RepoID = route.Namespace + "/" + route.Name
	if !validRepoID(route.RepoID) {
		return localRoute{}, false
	}
	return route, true
}

// repoTypeName is the singular the Hub's API uses in request bodies.
func repoTypeName(plural string) string { return strings.TrimSuffix(plural, "s") }

// repoURLPrefix is how a repository type appears in Hub URLs.
func repoURLPrefix(repoType string) string {
	if repoType == "models" {
		return ""
	}
	return repoType + "/"
}

// hfError answers in the Hub's error format, which its clients read: a JSON body
// and the X-Error-Code / X-Error-Message headers they map to exceptions.
func hfError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	if code != "" {
		w.Header().Set("X-Error-Code", code)
	}
	w.Header().Set("X-Error-Message", headerSafe(message))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

// headerSafe makes text fit a header value: one line of printable ASCII.
func headerSafe(text string) string {
	var b strings.Builder
	for _, r := range text {
		if r >= 0x20 && r < 0x7f {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
		if b.Len() >= 300 {
			break
		}
	}
	return b.String()
}

func writeHFJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// publicBase is the address clients reach the proxy at, for the links the API
// hands out (upload targets, commit URLs).
func (s *Server) publicBase(r *http.Request) string {
	if base := strings.TrimRight(s.currentConfig().PublicURL, "/"); base != "" {
		return base
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

// reservedNamespaces are names that would make a published repository's URL
// ambiguous with the routes of the proxy or the Hub's repository types.
var reservedNamespaces = map[string]bool{
	"api": true, "models": true, "datasets": true, "spaces": true, "auth": true, "assets": true,
	"login": true, "logout": true, "healthz": true, "readyz": true, "metrics": true, "static": true,
}

// validLocalRepoName follows the Hub's rules for the name part: letters, digits,
// '-', '_' and '.', at most 96 characters, no leading or trailing '-' or '.',
// no '--' or '..', and not ending in '.git' (that would be taken for the Git LFS
// endpoint of the repository).
func validLocalRepoName(name string) bool {
	if name == "" || len(name) > 96 || strings.HasSuffix(strings.ToLower(name), ".git") {
		return false
	}
	if strings.ContainsAny(name[:1], "-.") || strings.ContainsAny(name[len(name)-1:], "-.") || strings.Contains(name, "--") || strings.Contains(name, "..") {
		return false
	}
	return validRepoID(name) && !strings.Contains(name, "/")
}

// serveLocalAPI handles the Hub API routes for published repositories and the
// repository management routes. It reports whether it answered; false lets the
// request go on to the Hub passthrough.
func (s *Server) serveLocalAPI(w http.ResponseWriter, r *http.Request) bool {
	switch r.URL.Path {
	case "/api/validate-yaml":
		s.apiValidateYAML(w, r)
		return true
	case "/api/repos/create":
		s.apiCreateRepo(w, r)
		return true
	case "/api/repos/delete":
		s.apiDeleteRepo(w, r)
		return true
	case "/api/repos/move":
		hfError(w, http.StatusNotImplemented, "", "Renaming or moving a repository is not supported by this proxy.")
		return true
	}
	route, ok := parseLocalRoute(r.URL.EscapedPath())
	if !ok {
		return false
	}
	repo, err := s.db.FindRepository(r.Context(), route.Type, route.RepoID)
	if errors.Is(err, db.ErrNotFound) && s.inLocalNamespace(r.Context(), route.Namespace) {
		hfError(w, http.StatusNotFound, "RepoNotFound", "Repository not found")
		return true
	}
	if err != nil || repo.Origin != "local" {
		return false
	}
	s.localRouteHandler(w, r, route, repo)
	return true
}

// inLocalNamespace says whether a repository name lives under a user or group
// of this proxy. Such a name is answered "not found" when nothing is published
// there, the way the Hub does, rather than "not registered" as for a Hub
// repository nobody has asked to cache.
func (s *Server) inLocalNamespace(ctx context.Context, namespace string) bool {
	if !s.currentConfig().CustomRepos || reservedNamespaces[strings.ToLower(namespace)] {
		return false
	}
	_, err := s.db.ResolveNamespace(ctx, namespace)
	return err == nil || errors.Is(err, db.ErrAmbiguousNamespace)
}

func (s *Server) localRouteHandler(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	write := r.Method != http.MethodGet && r.Method != http.MethodHead
	var verb string
	if len(route.Rest) > 0 {
		verb = route.Rest[0]
	}
	switch {
	case write:
		switch {
		case verb == "preupload" && r.Method == http.MethodPost:
			s.apiPreupload(w, r, route, repo)
		case verb == "commit" && r.Method == http.MethodPost:
			s.apiCommit(w, r, route, repo)
		case verb == "branch" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
			s.apiBranch(w, r, route, repo)
		case verb == "tag" && (r.Method == http.MethodPost || r.Method == http.MethodDelete):
			s.apiTag(w, r, route, repo)
		case verb == "settings" && r.Method == http.MethodPut:
			s.apiRepoSettings(w, r, route, repo)
		case verb == "paths-info" && r.Method == http.MethodPost:
			s.apiPathsInfo(w, r, route, repo)
		default:
			hfError(w, http.StatusNotFound, "", "This route is not supported for repositories published on this proxy.")
		}
		return
	}
	p, authErr := s.authenticate(r)
	if _, _, err := s.repositoryAccess(r.Context(), p, authErr, route.Type, route.RepoID); err != nil {
		s.hubAccessError(w, err)
		return
	}
	switch verb {
	case "":
		s.apiRepoInfo(w, r, route, repo, "")
	case "revision":
		if len(route.Rest) != 2 {
			hfError(w, http.StatusNotFound, "", "Revision route is incomplete.")
			return
		}
		s.apiRepoInfo(w, r, route, repo, route.Rest[1])
	case "tree":
		s.apiTree(w, r, route, repo)
	case "refs":
		s.apiRefs(w, r, route, repo)
	case "commits":
		s.apiCommits(w, r, route, repo)
	default:
		hfError(w, http.StatusNotFound, "", "This route is not supported for repositories published on this proxy.")
	}
}

// hubAccessError reports a failed read check the way the Hub does: a repository
// the caller cannot see is "not found" to its clients.
func (s *Server) hubAccessError(w http.ResponseWriter, err error) {
	var access accessError
	if errors.As(err, &access) {
		if access.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		}
		hfError(w, access.status, "RepoNotFound", access.detail)
		return
	}
	hfError(w, http.StatusInternalServerError, "", "The repository could not be read.")
}

// publisher authenticates a request that changes published content. It takes
// a bearer token with the publish scope and nothing else: a browser session
// would make these routes reachable by a forged cross-site request.
func (s *Server) publisher(w http.ResponseWriter, r *http.Request) *Principal {
	if !s.currentConfig().CustomRepos {
		hfError(w, http.StatusForbidden, "", "Publishing models and datasets is turned off on this proxy.")
		return nil
	}
	if !strings.HasPrefix(strings.ToLower(r.Header.Get("Authorization")), "bearer ") {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		hfError(w, http.StatusUnauthorized, "", "Publishing needs an access token with the publish scope: send it as a bearer token.")
		return nil
	}
	p, err := s.authenticate(r)
	if err != nil || p == nil || !p.Authenticated {
		w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		hfError(w, http.StatusUnauthorized, "", "Invalid or expired access token.")
		return nil
	}
	if !hasScope(p, "publish") {
		hfError(w, http.StatusForbidden, "", "This access token does not have the publish scope. Create one with that scope in the console.")
		return nil
	}
	return p
}

// authorizeNamespace checks that p may publish under a namespace: their own
// name, or the name of a group they belong to (an administrator may use any).
// It returns the HTTP status and message to answer with when not.
func (s *Server) authorizeNamespace(ctx context.Context, p *Principal, namespace string) (db.Namespace, int, string) {
	if reservedNamespaces[strings.ToLower(namespace)] {
		return db.Namespace{}, http.StatusForbidden, "This name is reserved and cannot be used as a namespace."
	}
	ns, err := s.db.ResolveNamespace(ctx, namespace)
	switch {
	case errors.Is(err, db.ErrNotFound):
		return db.Namespace{}, http.StatusForbidden, fmt.Sprintf("There is no user or group named %q to publish under.", namespace)
	case errors.Is(err, db.ErrAmbiguousNamespace):
		return db.Namespace{}, http.StatusConflict, fmt.Sprintf("A user and a group are both named %q: ask an administrator to rename one.", namespace)
	case err != nil:
		return db.Namespace{}, http.StatusInternalServerError, "The namespace could not be checked."
	}
	if p.Role == "admin" || (ns.Kind == "user" && ns.ID == p.ID) {
		return ns, 0, ""
	}
	if ns.Kind == "group" {
		member, err := s.db.IsGroupMember(ctx, ns.ID, p.ID)
		if err != nil {
			return db.Namespace{}, http.StatusInternalServerError, "The namespace could not be checked."
		}
		if member {
			return ns, 0, ""
		}
	}
	return db.Namespace{}, http.StatusForbidden, fmt.Sprintf("You cannot publish under %q.", namespace)
}

// writableRepo is the common start of every route that changes a published
// repository: authenticate, then check the caller may write to its namespace.
func (s *Server) writableRepo(w http.ResponseWriter, r *http.Request, route localRoute) (*Principal, bool) {
	p := s.publisher(w, r)
	if p == nil {
		return nil, false
	}
	if _, status, message := s.authorizeNamespace(r.Context(), p, route.Namespace); status != 0 {
		hfError(w, status, "", message)
		return nil, false
	}
	return p, true
}

type createRepoRequest struct {
	Name         string  `json:"name"`
	Organization *string `json:"organization"`
	Private      *bool   `json:"private"`
	Visibility   *string `json:"visibility"`
	Type         string  `json:"type"`
}

// requestedPrivacy reads the visibility a request asks for: newer clients send
// "visibility" ("public" or "private") where older ones send "private".
func requestedPrivacy(private *bool, visibility *string) (value, set, ok bool) {
	if visibility != nil {
		switch *visibility {
		case "public":
			return false, true, true
		case "private":
			return true, true, true
		default:
			return false, false, false
		}
	}
	if private != nil {
		return *private, true, true
	}
	return false, false, true
}

// repoTarget reads the repository a create or delete request names.
func repoTarget(name string, organization *string, typ string) (repoType, namespace, repo string, ok bool) {
	switch typ {
	case "", "model":
		repoType = "models"
	case "dataset":
		repoType = "datasets"
	default:
		return "", "", "", false
	}
	if organization != nil && *organization != "" {
		return repoType, *organization, name, true
	}
	if i := strings.Index(name, "/"); i > 0 {
		return repoType, name[:i], name[i+1:], true
	}
	return repoType, "", name, true
}

// apiCreateRepo is POST /api/repos/create.
func (s *Server) apiCreateRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		hfError(w, http.StatusMethodNotAllowed, "", "Use POST.")
		return
	}
	p := s.publisher(w, r)
	if p == nil {
		return
	}
	var in createRepoRequest
	if !decodeLenient(w, r, &in) {
		return
	}
	if in.Type == "space" {
		hfError(w, http.StatusBadRequest, "", "Spaces cannot be published on this proxy: only models and datasets.")
		return
	}
	repoType, namespace, name, ok := repoTarget(in.Name, in.Organization, in.Type)
	if !ok {
		hfError(w, http.StatusBadRequest, "", "Unknown repository type.")
		return
	}
	if namespace == "" {
		namespace = p.Username
	}
	private, _, valid := requestedPrivacy(in.Private, in.Visibility)
	if !valid {
		hfError(w, http.StatusBadRequest, "", "Only public and private repositories are supported.")
		return
	}
	id, status, message := s.createPublishedRepo(r.Context(), p, repoType, namespace, name, private, "hub-api")
	if status == http.StatusConflict {
		// The Hub's clients read the address from the conflict, for exist_ok.
		w.Header().Set("X-Error-Message", headerSafe(message))
		writeHFJSON(w, http.StatusConflict, map[string]string{"error": message, "url": s.publicBase(r) + "/" + repoURLPrefix(repoType) + namespace + "/" + name})
		return
	}
	if status != 0 {
		hfError(w, status, "", message)
		return
	}
	writeHFJSON(w, http.StatusOK, map[string]any{
		"url":  s.publicBase(r) + "/" + repoURLPrefix(repoType) + namespace + "/" + name,
		"name": namespace + "/" + name,
		"id":   id,
	})
}

// createPublishedRepo checks that p may publish under namespace and creates the
// repository. It returns the new id, or the HTTP status and message of the
// refusal.
func (s *Server) createPublishedRepo(ctx context.Context, p *Principal, repoType, namespace, name string, private bool, source string) (string, int, string) {
	if !validLocalRepoName(name) {
		return "", http.StatusBadRequest, "Invalid repository name: use letters, digits, '-', '_' and '.', at most 96 characters, without leading or trailing '-' or '.', '--', '..' or a '.git' ending."
	}
	if !validRepoID(namespace + "/" + name) {
		return "", http.StatusBadRequest, "This name cannot be used as a namespace: it has characters repository names cannot hold."
	}
	ns, status, message := s.authorizeNamespace(ctx, p, namespace)
	if status != 0 {
		return "", status, message
	}
	repoID := namespace + "/" + name
	id, err := s.db.CreateLocalRepository(ctx, repoType, repoID, policyFor(private), p.ID)
	if errors.Is(err, db.ErrExists) {
		return "", http.StatusConflict, "You already created this " + repoTypeName(repoType) + " repo"
	}
	if err != nil {
		s.logger.Error("published repository could not be created", "request_id", requestID(ctx), "error", safeLogValue(err.Error(), 300))
		return "", http.StatusInternalServerError, "The repository could not be created."
	}
	if err := s.applyVisibility(ctx, id, private, ns, p.ID); err != nil {
		s.logger.Error("published repository access could not be set", "request_id", requestID(ctx), "error", safeLogValue(err.Error(), 300))
		return "", http.StatusInternalServerError, "The repository was created but its access could not be set."
	}
	s.audit(ctx, p.ID, "repository.created", "repository", id, map[string]any{"repo_type": repoType, "repo_id": repoID, "private": private, "source": source})
	return id, 0, ""
}

// policyFor maps the Hub's public/private to an access policy: private means
// only the namespace's members and the people granted read.
func policyFor(private bool) string {
	if private {
		return "allowlist"
	}
	return "public"
}

// applyVisibility sets a published repository's policy and, when it is private,
// the grants that let its namespace read it.
func (s *Server) applyVisibility(ctx context.Context, repositoryID string, private bool, ns db.Namespace, creatorID string) error {
	if err := s.db.SetRepositoryPolicy(ctx, repositoryID, policyFor(private)); err != nil {
		return err
	}
	if !private {
		return nil
	}
	subjectType := "user"
	if ns.Kind == "group" {
		subjectType = "group"
	}
	if err := s.db.UpsertGrant(ctx, repositoryID, subjectType, ns.ID, "read"); err != nil {
		return err
	}
	if creatorID != "" && creatorID != ns.ID {
		return s.db.UpsertGrant(ctx, repositoryID, "user", creatorID, "read")
	}
	return nil
}

type deleteRepoRequest struct {
	Name         string  `json:"name"`
	Organization *string `json:"organization"`
	Type         string  `json:"type"`
}

// apiDeleteRepo is DELETE /api/repos/delete.
func (s *Server) apiDeleteRepo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		hfError(w, http.StatusMethodNotAllowed, "", "Use DELETE.")
		return
	}
	p := s.publisher(w, r)
	if p == nil {
		return
	}
	var in deleteRepoRequest
	if !decodeLenient(w, r, &in) {
		return
	}
	repoType, namespace, name, ok := repoTarget(in.Name, in.Organization, in.Type)
	if !ok || namespace == "" || !validRepoID(namespace+"/"+name) {
		hfError(w, http.StatusBadRequest, "", "Name the repository to delete as organization and name.")
		return
	}
	repoID := namespace + "/" + name
	repo, err := s.db.FindRepository(r.Context(), repoType, repoID)
	if err != nil || repo.Origin != "local" {
		hfError(w, http.StatusNotFound, "RepoNotFound", "Repository not found")
		return
	}
	if _, status, message := s.authorizeNamespace(r.Context(), p, namespace); status != 0 {
		hfError(w, status, "", message)
		return
	}
	if err := s.db.DeleteLocalRepository(r.Context(), repo.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			hfError(w, http.StatusNotFound, "RepoNotFound", "Repository not found")
			return
		}
		s.logger.Error("published repository could not be deleted", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
		hfError(w, http.StatusInternalServerError, "", "The repository could not be deleted.")
		return
	}
	s.forgetCredentials(r.Context())
	s.audit(r.Context(), p.ID, "repository.deleted", "repository", repo.ID, map[string]any{"repo_type": repoType, "repo_id": repoID, "source": "hub-api"})
	w.WriteHeader(http.StatusOK)
}

// decodeLenient reads a JSON body the Hub's clients send. Unlike the console
// API it ignores fields it does not know (clients send more than is used).
func decodeLenient(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(target); err != nil {
		hfError(w, http.StatusBadRequest, "", "The request body is not valid JSON.")
		return false
	}
	return true
}

// apiRepoSettings is PUT /api/{type}/{id}/settings; only visibility is kept.
func (s *Server) apiRepoSettings(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p, ok := s.writableRepo(w, r, route)
	if !ok {
		return
	}
	var in struct {
		Private    *bool   `json:"private"`
		Visibility *string `json:"visibility"`
	}
	if !decodeLenient(w, r, &in) {
		return
	}
	private, set, valid := requestedPrivacy(in.Private, in.Visibility)
	if !valid {
		hfError(w, http.StatusBadRequest, "", "Only public and private repositories are supported.")
		return
	}
	if set {
		ns, status, message := s.authorizeNamespace(r.Context(), p, route.Namespace)
		if status != 0 {
			hfError(w, status, "", message)
			return
		}
		if err := s.applyVisibility(r.Context(), repo.ID, private, ns, repo.OwnerID); err != nil {
			hfError(w, http.StatusInternalServerError, "", "The visibility could not be changed.")
			return
		}
		s.forgetCredentials(r.Context())
		s.audit(r.Context(), p.ID, "repository.visibility", "repository", repo.ID, map[string]any{"private": private, "source": "hub-api"})
	}
	w.WriteHeader(http.StatusOK)
}

// resolveRevisionOr404 turns a revision into a commit, answering the Hub's
// RevisionNotFound when it does not exist.
func (s *Server) resolveRevisionOr404(w http.ResponseWriter, r *http.Request, repoID, revision string) (string, bool) {
	if revision == "" {
		revision = "main"
	}
	sha, err := s.db.ResolveLocalRevision(r.Context(), repoID, revision)
	if errors.Is(err, db.ErrRevisionNotFound) {
		hfError(w, http.StatusNotFound, "RevisionNotFound", fmt.Sprintf("Invalid rev id: %s", revision))
		return "", false
	}
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The revision could not be read.")
		return "", false
	}
	return sha, true
}

// lfsPointerSize is the size of the pointer file Git LFS would store in place of
// a large file, which the Hub reports in its listings.
func lfsPointerSize(oid string, size int64) int64 {
	return int64(len(fmt.Sprintf("version https://git-lfs.github.com/spec/v1\noid sha256:%s\nsize %d\n", oid, size)))
}

type fileEntry struct {
	Type string         `json:"type"`
	OID  string         `json:"oid"`
	Size int64          `json:"size,omitempty"`
	Path string         `json:"path"`
	LFS  map[string]any `json:"lfs,omitempty"`
}

func fileEntryFor(f db.LocalFile) fileEntry {
	e := fileEntry{Type: "file", OID: f.OID, Size: f.Size, Path: f.Path}
	if f.LFS {
		e.LFS = map[string]any{"oid": f.SHA256, "size": f.Size, "pointerSize": lfsPointerSize(f.SHA256, f.Size)}
	}
	return e
}

// dirOID gives a directory a stable Git-style identity from what it holds.
func dirOID(entries []string) string {
	sort.Strings(entries)
	h := sha1.New() // #nosec G401 -- see the import.
	for _, e := range entries {
		h.Write([]byte(e))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// treeEntries lists the entries under prefix ("" is the root): the files and
// folders directly inside it, or with recursive everything below.
func treeEntries(files []db.LocalFile, prefix string, recursive bool) []any {
	if prefix != "" && !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	dirs := map[string][]string{}
	var entries []fileEntry
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		parts := strings.Split(f.Path[len(prefix):], "/")
		for i := 0; i < len(parts)-1; i++ {
			if i > 0 && !recursive {
				break
			}
			dir := prefix + strings.Join(parts[:i+1], "/")
			dirs[dir] = append(dirs[dir], f.Path+"\x00"+f.OID)
		}
		if len(parts) == 1 || recursive {
			entries = append(entries, fileEntryFor(f))
		}
	}
	for dir, below := range dirs {
		entries = append(entries, fileEntry{Type: "directory", OID: dirOID(below), Path: dir})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	out := make([]any, len(entries))
	for i, e := range entries {
		out[i] = e
	}
	return out
}

// apiRepoInfo is GET /api/{type}/{id} and /revision/{revision}.
func (s *Server) apiRepoInfo(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess, revision string) {
	sha, ok := s.resolveRevisionOr404(w, r, repo.ID, revision)
	if !ok {
		return
	}
	commit, err := s.db.LocalCommitBySHA(r.Context(), repo.ID, sha)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The revision could not be read.")
		return
	}
	files, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The files could not be listed.")
		return
	}
	stored, err := s.db.GetRepository(r.Context(), repo.ID)
	if err != nil {
		hfError(w, http.StatusNotFound, "RepoNotFound", "Repository not found")
		return
	}
	siblings := make([]map[string]any, 0, len(files))
	var used int64
	for _, f := range files {
		sibling := map[string]any{"rfilename": f.Path, "size": f.Size, "blobId": f.OID}
		if f.LFS {
			sibling["lfs"] = map[string]any{"sha256": f.SHA256, "size": f.Size, "pointerSize": lfsPointerSize(f.SHA256, f.Size)}
		}
		siblings = append(siblings, sibling)
		used += f.Size
	}
	card := s.readCard(r.Context(), files)
	tags := card.Tags
	if tags == nil {
		tags = []string{}
	}
	info := map[string]any{
		"_id": repo.ID, "id": route.RepoID, "author": route.Namespace, "sha": sha,
		"createdAt": stored.CreatedAt.UTC().Format(time.RFC3339), "lastModified": commit.CreatedAt.UTC().Format(time.RFC3339),
		"private": repo.Policy != "public", "disabled": false, "gated": false, "downloads": 0, "likes": 0,
		"tags": tags, "siblings": siblings, "usedStorage": used,
	}
	if len(card.Data) > 0 {
		info["cardData"] = card.Data
		for _, key := range []string{"pipeline_tag", "library_name"} {
			if value, ok := card.Data[key].(string); ok {
				info[key] = value
			}
		}
	}
	if route.Type == "models" {
		info["modelId"] = route.RepoID
	}
	writeHFJSON(w, http.StatusOK, info)
}

// apiTree is GET /api/{type}/{id}/tree/{revision}[/{path}].
func (s *Server) apiTree(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	if len(route.Rest) < 2 {
		hfError(w, http.StatusNotFound, "", "Tree route is incomplete.")
		return
	}
	sha, ok := s.resolveRevisionOr404(w, r, repo.ID, route.Rest[1])
	if !ok {
		return
	}
	prefix := path.Clean(strings.Join(route.Rest[2:], "/"))
	if prefix == "." || prefix == "/" {
		prefix = ""
	}
	files, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The files could not be listed.")
		return
	}
	recursive := queryFlag(r, "recursive")
	entries := treeEntries(files, prefix, recursive)
	if prefix != "" && len(entries) == 0 {
		hfError(w, http.StatusNotFound, "EntryNotFound", "Path not found: "+prefix)
		return
	}
	if entries == nil {
		entries = []any{}
	}
	writeHFJSON(w, http.StatusOK, entries)
}

// apiPathsInfo is POST /api/{type}/{id}/paths-info/{revision}.
func (s *Server) apiPathsInfo(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p, authErr := s.authenticate(r)
	if _, _, err := s.repositoryAccess(r.Context(), p, authErr, route.Type, route.RepoID); err != nil {
		s.hubAccessError(w, err)
		return
	}
	if len(route.Rest) != 2 {
		hfError(w, http.StatusNotFound, "", "Revision route is incomplete.")
		return
	}
	sha, ok := s.resolveRevisionOr404(w, r, repo.ID, route.Rest[1])
	if !ok {
		return
	}
	// huggingface_hub sends the paths as a form, other clients as JSON.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		hfError(w, http.StatusBadRequest, "", "The request body could not be read.")
		return
	}
	var in struct {
		Paths []string `json:"paths"`
	}
	if trimmed := bytes.TrimSpace(raw); len(trimmed) > 0 && trimmed[0] == '{' {
		if err := json.Unmarshal(trimmed, &in); err != nil {
			hfError(w, http.StatusBadRequest, "", "The request body is not valid JSON.")
			return
		}
	} else {
		form, err := url.ParseQuery(string(raw))
		if err != nil {
			hfError(w, http.StatusBadRequest, "", "The request body is not a valid form.")
			return
		}
		in.Paths = form["paths"]
	}
	files, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The files could not be listed.")
		return
	}
	byPath := make(map[string]db.LocalFile, len(files))
	for _, f := range files {
		byPath[f.Path] = f
	}
	out := []any{}
	for _, want := range in.Paths {
		want = strings.Trim(want, "/")
		if f, found := byPath[want]; found {
			out = append(out, fileEntryFor(f))
			continue
		}
		var children []string
		for _, f := range files {
			if strings.HasPrefix(f.Path, want+"/") {
				children = append(children, f.Path+"\x00"+f.OID)
			}
		}
		if len(children) > 0 {
			out = append(out, fileEntry{Type: "directory", OID: dirOID(children), Path: want})
		}
	}
	writeHFJSON(w, http.StatusOK, out)
}

// apiRefs is GET /api/{type}/{id}/refs.
func (s *Server) apiRefs(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	refs, err := s.db.LocalRefs(r.Context(), repo.ID)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The references could not be read.")
		return
	}
	out := map[string]any{"branches": []any{}, "tags": []any{}, "converts": []any{}, "pullRequests": []any{}}
	branches, tags := []any{}, []any{}
	for _, ref := range refs {
		entry := map[string]string{"name": ref.Name, "targetCommit": ref.CommitSHA}
		if ref.Kind == "branch" {
			entry["ref"] = "refs/heads/" + ref.Name
			branches = append(branches, entry)
		} else {
			entry["ref"] = "refs/tags/" + ref.Name
			tags = append(tags, entry)
		}
	}
	out["branches"], out["tags"] = branches, tags
	writeHFJSON(w, http.StatusOK, out)
}

// apiCommits is GET /api/{type}/{id}/commits/{revision}.
func (s *Server) apiCommits(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	if len(route.Rest) != 2 {
		hfError(w, http.StatusNotFound, "", "Revision route is incomplete.")
		return
	}
	sha, ok := s.resolveRevisionOr404(w, r, repo.ID, route.Rest[1])
	if !ok {
		return
	}
	commits, err := s.db.LocalHistory(r.Context(), repo.ID, sha, 1000)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The history could not be read.")
		return
	}
	out := make([]map[string]any, 0, len(commits))
	for _, c := range commits {
		out = append(out, map[string]any{
			"id": c.SHA, "date": c.CreatedAt.UTC().Format(time.RFC3339), "title": c.Summary, "message": c.Description,
			"authors": []map[string]string{{"user": c.AuthorName, "avatarUrl": ""}},
		})
	}
	writeHFJSON(w, http.StatusOK, out)
}

// apiBranch is POST/DELETE /api/{type}/{id}/branch/{branch}.
func (s *Server) apiBranch(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p, ok := s.writableRepo(w, r, route)
	if !ok {
		return
	}
	if len(route.Rest) != 2 || !validPublishedRefName(route.Rest[1]) {
		hfError(w, http.StatusBadRequest, "", "Invalid branch name.")
		return
	}
	name := route.Rest[1]
	if r.Method == http.MethodDelete {
		err := s.db.DeleteLocalRef(r.Context(), repo.ID, "branch", name)
		switch {
		case errors.Is(err, db.ErrRevisionNotFound):
			hfError(w, http.StatusNotFound, "RevisionNotFound", "Branch not found: "+name)
		case err != nil:
			hfError(w, http.StatusBadRequest, "", err.Error())
		default:
			s.audit(r.Context(), p.ID, "repository.branch.deleted", "repository", repo.ID, map[string]any{"branch": name})
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	var in struct {
		StartingPoint string `json:"startingPoint"`
	}
	if r.ContentLength != 0 {
		if !decodeLenient(w, r, &in) {
			return
		}
	}
	from := in.StartingPoint
	if from == "" {
		from = "main"
	}
	err := s.db.CreateLocalRef(r.Context(), repo.ID, "branch", name, from)
	switch {
	case errors.Is(err, db.ErrExists):
		hfError(w, http.StatusConflict, "", "Branch already exists: "+name)
	case errors.Is(err, db.ErrRevisionNotFound):
		hfError(w, http.StatusNotFound, "RevisionNotFound", "Invalid rev id: "+from)
	case err != nil:
		hfError(w, http.StatusInternalServerError, "", "The branch could not be created.")
	default:
		s.audit(r.Context(), p.ID, "repository.branch.created", "repository", repo.ID, map[string]any{"branch": name, "from": from})
		w.WriteHeader(http.StatusOK)
	}
}

// apiTag is POST /api/{type}/{id}/tag/{revision} and DELETE .../tag/{tag}.
func (s *Server) apiTag(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p, ok := s.writableRepo(w, r, route)
	if !ok {
		return
	}
	if len(route.Rest) != 2 {
		hfError(w, http.StatusBadRequest, "", "Name the revision or tag.")
		return
	}
	if r.Method == http.MethodDelete {
		err := s.db.DeleteLocalRef(r.Context(), repo.ID, "tag", route.Rest[1])
		switch {
		case errors.Is(err, db.ErrRevisionNotFound):
			hfError(w, http.StatusNotFound, "RevisionNotFound", "Tag not found: "+route.Rest[1])
		case err != nil:
			hfError(w, http.StatusInternalServerError, "", "The tag could not be deleted.")
		default:
			s.audit(r.Context(), p.ID, "repository.tag.deleted", "repository", repo.ID, map[string]any{"tag": route.Rest[1]})
			w.WriteHeader(http.StatusOK)
		}
		return
	}
	var in struct {
		Tag     string `json:"tag"`
		Message string `json:"message"`
	}
	if !decodeLenient(w, r, &in) {
		return
	}
	if !validPublishedRefName(in.Tag) {
		hfError(w, http.StatusBadRequest, "", "Invalid tag name.")
		return
	}
	err := s.db.CreateLocalRef(r.Context(), repo.ID, "tag", in.Tag, route.Rest[1])
	switch {
	case errors.Is(err, db.ErrExists):
		hfError(w, http.StatusConflict, "", "Tag already exists: "+in.Tag)
	case errors.Is(err, db.ErrRevisionNotFound):
		hfError(w, http.StatusNotFound, "RevisionNotFound", "Invalid rev id: "+route.Rest[1])
	case err != nil:
		hfError(w, http.StatusInternalServerError, "", "The tag could not be created.")
	default:
		s.audit(r.Context(), p.ID, "repository.tag.created", "repository", repo.ID, map[string]any{"tag": in.Tag, "from": route.Rest[1]})
		w.WriteHeader(http.StatusOK)
	}
}

// validRefName accepts the branch and tag names Git does, minus the awkward ones.
func validPublishedRefName(name string) bool {
	if name == "" || len(name) > 200 || strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".lock") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") || name == "@" {
		return false
	}
	if strings.HasPrefix(name, "refs/") {
		return false
	}
	for _, r := range name {
		if r <= 0x20 || r == 0x7f || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return !validCommit(name)
}

// queryFlag reads a boolean query parameter the way the Hub's clients write it
// (Python sends "True").
func queryFlag(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}
