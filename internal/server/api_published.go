package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/bytesize"
	"github.com/hfproxy/hfproxy/internal/db"
)

// The console's view of the models and datasets published on this proxy: the
// list, a repository's files, history and card, and the actions a person with
// the right on its namespace can take (upload, delete, change visibility). It
// works on browser sessions; the Hub API (local_hub.go) is for tokens.

const (
	readmeShownBytes  = 256 << 10
	treeEntriesShown  = 5000
	commitsShownLimit = 200
	consoleUploadMax  = 4 << 30
)

func (s *Server) apiPublished(w http.ResponseWriter, r *http.Request, rest string) {
	p, err := s.authenticate(r)
	if err != nil || p == nil || !p.Authenticated {
		writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "Sign in to see the published models and datasets.")
		return
	}
	rest = strings.Trim(rest, "/")
	if rest == "" {
		switch r.Method {
		case http.MethodGet:
			s.listPublished(w, r, p)
		case http.MethodPost:
			s.createPublished(w, r, p)
		default:
			methodNotAllowedWith(w, "GET, POST")
		}
		return
	}
	parts := strings.Split(rest, "/")
	repo, ok := s.loadPublished(w, r, p, parts[0])
	if !ok {
		return
	}
	switch {
	case len(parts) == 1:
		switch r.Method {
		case http.MethodGet:
			s.publishedDetail(w, r, p, repo)
		case http.MethodPatch:
			s.updatePublished(w, r, p, repo)
		case http.MethodDelete:
			s.deletePublished(w, r, p, repo)
		default:
			methodNotAllowedWith(w, "GET, PATCH, DELETE")
		}
	case len(parts) == 2 && parts[1] == "tree" && r.Method == http.MethodGet:
		s.publishedTree(w, r, repo)
	case len(parts) == 2 && parts[1] == "commits" && r.Method == http.MethodGet:
		s.publishedCommits(w, r, repo)
	case len(parts) == 2 && parts[1] == "files" && r.Method == http.MethodPost:
		s.uploadPublishedFiles(w, r, p, repo)
	case len(parts) == 2 && parts[1] == "delete-files" && r.Method == http.MethodPost:
		s.deletePublishedFiles(w, r, p, repo)
	default:
		writeProblem(w, r, http.StatusNotFound, "Not found", "This hfproxy API route is not implemented.")
	}
}

type publishedItem struct {
	ID        string    `json:"id"`
	RepoType  string    `json:"repo_type"`
	RepoID    string    `json:"repo_id"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	Private   bool      `json:"private"`
	Owner     string    `json:"owner"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CanWrite  bool      `json:"can_write"`
}

func itemFor(repoPK, repoType, repoID, policy, owner string, bytes int64, created, updated time.Time) publishedItem {
	namespace, name, _ := strings.Cut(repoID, "/")
	return publishedItem{ID: repoPK, RepoType: repoType, RepoID: repoID, Namespace: namespace, Name: name, Private: policy != "public", Owner: owner, Bytes: bytes, CreatedAt: created, UpdatedAt: updated}
}

// canWrite says whether p may change what is published under a namespace.
func (s *Server) canWrite(ctx context.Context, p *Principal, repoID string) bool {
	if !s.currentConfig().CustomRepos {
		return false
	}
	namespace, _, _ := strings.Cut(repoID, "/")
	_, status, _ := s.authorizeNamespace(ctx, p, namespace)
	return status == 0
}

func (s *Server) listPublished(w http.ResponseWriter, r *http.Request, p *Principal) {
	list, err := s.db.ListPublished(r.Context(), p.Role, p.ID, 500)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to list the published repositories.")
		return
	}
	namespaces := map[string]bool{}
	items := make([]publishedItem, 0, len(list))
	for _, repo := range list {
		item := itemFor(repo.ID, repo.Type, repo.RepoID, repo.Policy, repo.OwnerName, repo.Bytes, repo.CreatedAt, repo.UpdatedAt)
		allowed, known := namespaces[item.Namespace]
		if !known {
			allowed = s.canWrite(r.Context(), p, repo.RepoID)
			namespaces[item.Namespace] = allowed
		}
		item.CanWrite = allowed
		items = append(items, item)
	}
	groups, _ := s.db.UserGroupNames(r.Context(), p.ID)
	writeJSON(w, http.StatusOK, map[string]any{
		"items":      items,
		"enabled":    s.currentConfig().CustomRepos,
		"namespaces": append([]string{p.Username}, groups...),
		"max_file":   s.currentConfig().MaxUploadFileSize(),
		"quota":      s.currentConfig().UploadUserQuota,
	})
}

func (s *Server) createPublished(w http.ResponseWriter, r *http.Request, p *Principal) {
	var in struct {
		RepoType  string `json:"repo_type"`
		Namespace string `json:"namespace"`
		Name      string `json:"name"`
		Private   bool   `json:"private"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.currentConfig().CustomRepos {
		writeProblem(w, r, http.StatusForbidden, "Publishing is off", "Publishing models and datasets is turned off on this proxy.")
		return
	}
	if p.TokenID != "" && !hasScope(p, "publish") {
		writeProblem(w, r, http.StatusForbidden, "Not allowed", "This token does not have the publish scope.")
		return
	}
	repoType := strings.TrimSuffix(in.RepoType, "s") + "s"
	if repoType != "models" && repoType != "datasets" {
		writeProblem(w, r, http.StatusBadRequest, "Invalid repository", "The type must be model or dataset.")
		return
	}
	namespace := strings.TrimSpace(in.Namespace)
	if namespace == "" {
		namespace = p.Username
	}
	id, status, message := s.createPublishedRepo(r.Context(), p, repoType, namespace, strings.TrimSpace(in.Name), in.Private, "console")
	if status != 0 {
		writeProblem(w, r, status, "Repository not created", message)
		return
	}
	repo, err := s.db.GetRepository(r.Context(), id)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to load the new repository.")
		return
	}
	item := itemFor(id, repo.Type, repo.RepoID, repo.Policy, p.Username, 0, repo.CreatedAt, repo.UpdatedAt)
	item.CanWrite = true
	writeJSON(w, http.StatusCreated, item)
}

// loadPublished finds a published repository the caller may read; to everyone
// else it does not exist.
func (s *Server) loadPublished(w http.ResponseWriter, r *http.Request, p *Principal, id string) (db.Repository, bool) {
	repo, err := s.db.GetRepository(r.Context(), id)
	if err == nil && repo.Origin == "local" {
		if _, accessErr := s.repositoryAccessInfo(r.Context(), p, nil, repo.Type, repo.RepoID); accessErr == nil {
			return repo, true
		}
	}
	if err != nil && !errors.Is(err, db.ErrNotFound) {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to load the repository.")
		return db.Repository{}, false
	}
	writeProblem(w, r, http.StatusNotFound, "Not found", "This published repository does not exist.")
	return db.Repository{}, false
}

// requireWrite stops a request that changes a repository unless p may.
func (s *Server) requireWrite(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) bool {
	if !s.currentConfig().CustomRepos {
		writeProblem(w, r, http.StatusForbidden, "Publishing is off", "Publishing models and datasets is turned off on this proxy.")
		return false
	}
	if p.TokenID != "" && !hasScope(p, "publish") {
		writeProblem(w, r, http.StatusForbidden, "Not allowed", "This token does not have the publish scope.")
		return false
	}
	namespace, _, _ := strings.Cut(repo.RepoID, "/")
	if _, status, message := s.authorizeNamespace(r.Context(), p, namespace); status != 0 {
		writeProblem(w, r, status, "Not allowed", message)
		return false
	}
	return true
}

func (s *Server) publishedDetail(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) {
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	sha, err := s.db.ResolveLocalRevision(r.Context(), repo.ID, revision)
	if errors.Is(err, db.ErrRevisionNotFound) {
		writeProblem(w, r, http.StatusNotFound, "Revision not found", "This branch, tag or commit does not exist.")
		return
	}
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the revision.")
		return
	}
	head, err := s.db.LocalCommitBySHA(r.Context(), repo.ID, sha)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the commit.")
		return
	}
	refs, err := s.db.LocalRefs(r.Context(), repo.ID)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the references.")
		return
	}
	bytes, _ := s.db.LocalRepositoryBytes(r.Context(), repo.ID)
	owner := ""
	if repo.OwnerID != "" {
		owner = s.usernameOf(r.Context(), repo.OwnerID)
	}
	item := itemFor(repo.ID, repo.Type, repo.RepoID, repo.Policy, owner, bytes, repo.CreatedAt, repo.UpdatedAt)
	item.CanWrite = s.canWrite(r.Context(), p, repo.RepoID)

	branches, tags := []string{}, []string{}
	for _, ref := range refs {
		if ref.Kind == "branch" {
			branches = append(branches, ref.Name)
		} else {
			tags = append(tags, ref.Name)
		}
	}
	out := map[string]any{
		"repository": item, "revision": revision, "branches": branches, "tags": tags,
		"head":      map[string]any{"sha": head.SHA, "summary": head.Summary, "author": head.AuthorName, "date": head.CreatedAt},
		"files_url": s.publicBase(r) + "/" + repoURLPrefix(repo.Type) + repo.RepoID + "/resolve/",
	}
	if readme, err := s.db.LocalFileAt(r.Context(), repo.ID, sha, "README.md"); err == nil {
		out["readme"] = s.readmeFor(r.Context(), readme)
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) usernameOf(ctx context.Context, userID string) string {
	var name string
	if err := s.db.SQL.QueryRowContext(ctx, `SELECT username FROM users WHERE id=$1`, userID).Scan(&name); err != nil {
		return ""
	}
	return name
}

// readmeFor returns a repository's README for display: its Markdown, without the
// metadata block, and the metadata separately.
func (s *Server) readmeFor(ctx context.Context, file db.LocalFile) map[string]any {
	out := map[string]any{"size": file.Size}
	objects := s.currentStorage()
	if objects == nil || file.Size > readmeShownBytes {
		out["truncated"] = file.Size > readmeShownBytes
		return out
	}
	body, err := objects.Get(ctx, file.StorageKey, "")
	if err != nil {
		return out
	}
	defer body.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(body.Body, readmeShownBytes+1))
	if err != nil {
		return out
	}
	data, markdown, err := parseCard(string(raw))
	if err != nil {
		out["card_error"] = err.Error()
		out["content"] = string(raw)
		return out
	}
	out["content"] = markdown
	out["card"] = data
	return out
}

type treeItem struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Type string `json:"type"`
	Size int64  `json:"size"`
	LFS  bool   `json:"lfs"`
}

func (s *Server) publishedTree(w http.ResponseWriter, r *http.Request, repo db.Repository) {
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	sha, err := s.db.ResolveLocalRevision(r.Context(), repo.ID, revision)
	if errors.Is(err, db.ErrRevisionNotFound) {
		writeProblem(w, r, http.StatusNotFound, "Revision not found", "This branch, tag or commit does not exist.")
		return
	}
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the revision.")
		return
	}
	folder := strings.Trim(r.URL.Query().Get("path"), "/")
	files, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to list the files.")
		return
	}
	prefix := ""
	if folder != "" {
		prefix = folder + "/"
	}
	dirs := map[string]bool{}
	var items []treeItem
	for _, f := range files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest := f.Path[len(prefix):]
		if name, _, nested := strings.Cut(rest, "/"); nested {
			dirs[name] = true
			continue
		}
		items = append(items, treeItem{Name: rest, Path: f.Path, Type: "file", Size: f.Size, LFS: f.LFS})
	}
	for name := range dirs {
		items = append(items, treeItem{Name: name, Path: prefix + name, Type: "directory"})
	}
	if folder != "" && len(items) == 0 {
		writeProblem(w, r, http.StatusNotFound, "Not found", "This folder does not exist at this revision.")
		return
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Type != items[j].Type {
			return items[i].Type == "directory"
		}
		return items[i].Name < items[j].Name
	})
	truncated := false
	if len(items) > treeEntriesShown {
		items, truncated = items[:treeEntriesShown], true
	}
	if items == nil {
		items = []treeItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "path": folder, "revision": revision, "commit": sha, "truncated": truncated})
}

func (s *Server) publishedCommits(w http.ResponseWriter, r *http.Request, repo db.Repository) {
	revision := r.URL.Query().Get("revision")
	if revision == "" {
		revision = "main"
	}
	sha, err := s.db.ResolveLocalRevision(r.Context(), repo.ID, revision)
	if errors.Is(err, db.ErrRevisionNotFound) {
		writeProblem(w, r, http.StatusNotFound, "Revision not found", "This branch, tag or commit does not exist.")
		return
	}
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the revision.")
		return
	}
	commits, err := s.db.LocalHistory(r.Context(), repo.ID, sha, commitsShownLimit)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to read the history.")
		return
	}
	items := make([]map[string]any, 0, len(commits))
	for _, c := range commits {
		items = append(items, map[string]any{"sha": c.SHA, "summary": c.Summary, "description": c.Description, "author": c.AuthorName, "date": c.CreatedAt})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) updatePublished(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) {
	if !s.requireWrite(w, r, p, repo) {
		return
	}
	var in struct {
		Private *bool `json:"private"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Private == nil {
		writeProblem(w, r, http.StatusBadRequest, "Nothing to change", "Provide private: true or false.")
		return
	}
	namespace, _, _ := strings.Cut(repo.RepoID, "/")
	ns, status, message := s.authorizeNamespace(r.Context(), p, namespace)
	if status != 0 {
		writeProblem(w, r, status, "Not allowed", message)
		return
	}
	if err := s.applyVisibility(r.Context(), repo.ID, *in.Private, ns, repo.OwnerID); err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to change the visibility.")
		return
	}
	s.forgetCredentials(r.Context())
	s.audit(r.Context(), p.ID, "repository.visibility", "repository", repo.ID, map[string]any{"private": *in.Private, "source": "console"})
	writeJSON(w, http.StatusOK, map[string]any{"id": repo.ID, "private": *in.Private})
}

func (s *Server) deletePublished(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) {
	if !s.requireWrite(w, r, p, repo) {
		return
	}
	if err := s.db.DeleteLocalRepository(r.Context(), repo.ID); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeProblem(w, r, http.StatusNotFound, "Not found", "This published repository does not exist.")
			return
		}
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to delete the repository.")
		return
	}
	s.forgetCredentials(r.Context())
	s.audit(r.Context(), p.ID, "repository.deleted", "repository", repo.ID, map[string]any{"repo_type": repo.Type, "repo_id": repo.RepoID, "source": "console"})
	w.WriteHeader(http.StatusNoContent)
}

// uploadPublishedFiles takes files from the console's upload form and records
// them as one commit. The form is read as a stream: each file goes to the
// staging disk as it arrives, so a big file never sits in memory.
func (s *Server) uploadPublishedFiles(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) {
	if !s.requireWrite(w, r, p, repo) {
		return
	}
	branch := r.URL.Query().Get("revision")
	if branch == "" {
		branch = "main"
	}
	directory := strings.Trim(r.URL.Query().Get("directory"), "/")
	if directory != "" && !validRepoFilePath(directory) {
		writeProblem(w, r, http.StatusBadRequest, "Invalid folder", "The destination folder is not a valid path.")
		return
	}
	message := strings.TrimSpace(r.URL.Query().Get("message"))
	if message == "" {
		message = "Upload files via the console"
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, io.NopCloser(&deadlineReader{r: r.Body, controller: controller, idle: uploadIdle}), consoleUploadMax)
	reader, err := r.MultipartReader()
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid upload", "Send the files as multipart/form-data.")
		return
	}

	limit := s.currentConfig().MaxUploadFileSize()
	var ops []db.CommitOp
	sizes := map[string]int64{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeProblem(w, r, http.StatusBadRequest, "Upload interrupted", "The upload was cut off or is not valid.")
			return
		}
		// FileName() keeps only the last element; a folder upload names each file
		// with its relative path, which validRepoFilePath then checks.
		rawName := uploadedFileName(part)
		if rawName == "" {
			continue
		}
		name := strings.TrimLeft(strings.ReplaceAll(rawName, "\\", "/"), "/")
		if directory != "" {
			name = directory + "/" + name
		}
		if !validRepoFilePath(name) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid file name", "The file name "+safeLogValue(rawName, 120)+" cannot be used as a path in the repository.")
			return
		}
		op, status, detail := s.stageUploadedPart(r.Context(), part, name, limit)
		if status != 0 {
			writeProblem(w, r, status, "Upload refused", detail)
			return
		}
		ops = append(ops, op)
		sizes[op.SHA256] = op.Size
	}
	if len(ops) == 0 {
		writeProblem(w, r, http.StatusBadRequest, "No files", "Choose at least one file.")
		return
	}
	commit, failure := s.commitOps(r.Context(), p, repo.ID, repo.OwnerID, branch, "", p.Username, message, "", ops, sizes)
	if failure != nil {
		writeProblem(w, r, failure.status, "Upload refused", failure.message)
		return
	}
	s.audit(r.Context(), p.ID, "repository.commit", "repository", repo.ID, map[string]any{"commit": commit.SHA, "branch": branch, "operations": len(ops), "summary": safeLogValue(commit.Summary, 120), "source": "console"})
	writeJSON(w, http.StatusCreated, map[string]any{"commit": commit.SHA, "files": len(ops)})
}

// uploadedFileName is the file name of a form part exactly as the browser sent
// it (Part.FileName strips the folders).
func uploadedFileName(part *multipart.Part) string {
	_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
	if err != nil {
		return ""
	}
	return params["filename"]
}

// stageUploadedPart stores one uploaded file and returns the commit operation
// that adds it, or the status and message of the refusal.
func (s *Server) stageUploadedPart(ctx context.Context, part *multipart.Part, name string, limit int64) (db.CommitOp, int, string) {
	file, err := s.createStagingFile("console-*")
	if err != nil {
		return db.CommitOp{}, http.StatusInternalServerError, "The upload could not be staged."
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(part, limit+1))
	if err != nil {
		return db.CommitOp{}, http.StatusBadRequest, "The upload was interrupted."
	}
	if size > limit {
		return db.CommitOp{}, http.StatusRequestEntityTooLarge, name + " is larger than the " + bytesize.Format(limit) + " allowed for one file."
	}
	sum := hex.EncodeToString(hash.Sum(nil))
	lfs := size > 0 && usesLFS(name, size)
	oid := sum
	if !lfs {
		content, err := os.ReadFile(file.Name())
		if err != nil {
			return db.CommitOp{}, http.StatusInternalServerError, "The upload could not be read back."
		}
		oid = gitBlobOID(content)
	}
	if !s.blobIsKept(ctx, sum, size) {
		if err := s.storeUploadedBlob(ctx, file, sum, size); err != nil {
			s.logger.Error("uploaded file could not be stored", "request_id", requestID(ctx), "error", safeLogValue(err.Error(), 300))
			return db.CommitOp{}, http.StatusBadGateway, "The file " + name + " could not be stored."
		}
		if s.metrics != nil {
			s.metrics.add("hfproxy_published_bytes_total", nil, float64(size))
		}
	}
	return db.CommitOp{Kind: db.OpAdd, Path: path.Clean(name), SHA256: sum, OID: oid, Size: size, LFS: lfs, ContentType: contentTypeFor(name)}, 0, ""
}

func (s *Server) deletePublishedFiles(w http.ResponseWriter, r *http.Request, p *Principal, repo db.Repository) {
	if !s.requireWrite(w, r, p, repo) {
		return
	}
	var in struct {
		Revision string   `json:"revision"`
		Paths    []string `json:"paths"`
		Message  string   `json:"message"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.Revision == "" {
		in.Revision = "main"
	}
	if len(in.Paths) == 0 || len(in.Paths) > 1000 {
		writeProblem(w, r, http.StatusBadRequest, "Nothing to delete", "Name between 1 and 1000 files or folders.")
		return
	}
	sha, err := s.db.ResolveLocalRevision(r.Context(), repo.ID, in.Revision)
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "Revision not found", "This branch does not exist.")
		return
	}
	files, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		s.fail(w, r, err, http.StatusInternalServerError, "Database error", "Unable to list the files.")
		return
	}
	isFile := map[string]bool{}
	for _, f := range files {
		isFile[f.Path] = true
	}
	var ops []db.CommitOp
	for _, raw := range in.Paths {
		target := strings.Trim(raw, "/")
		if !validRepoFilePath(target) {
			writeProblem(w, r, http.StatusBadRequest, "Invalid path", "One of the paths is not valid.")
			return
		}
		if isFile[target] {
			ops = append(ops, db.CommitOp{Kind: db.OpDeleteFile, Path: target})
		} else {
			ops = append(ops, db.CommitOp{Kind: db.OpDeleteFolder, Path: target})
		}
	}
	message := strings.TrimSpace(in.Message)
	if message == "" {
		message = "Delete files via the console"
	}
	commit, failure := s.commitOps(r.Context(), p, repo.ID, repo.OwnerID, in.Revision, "", p.Username, message, "", ops, nil)
	if failure != nil {
		writeProblem(w, r, failure.status, "Delete refused", failure.message)
		return
	}
	s.audit(r.Context(), p.ID, "repository.commit", "repository", repo.ID, map[string]any{"commit": commit.SHA, "branch": in.Revision, "operations": len(ops), "summary": safeLogValue(commit.Summary, 120), "source": "console"})
	writeJSON(w, http.StatusOK, map[string]any{"commit": commit.SHA})
}
