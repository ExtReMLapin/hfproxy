package server

import (
	"bytes"
	"context"
	"crypto/sha1" //nolint:gosec // Git blob names are SHA-1; they identify content, they do not protect it.
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/db"
)

const (
	// regularFileLimit is the size above which the Hub wants a file sent through
	// Git LFS instead of inside the commit.
	regularFileLimit = 10 << 20
	// inlineFileLimit is what one file inside a commit may hold at most.
	inlineFileLimit = 32 << 20
	// commitBodyLimit bounds the whole commit request (inline files are base64).
	commitBodyLimit = 512 << 20
	// uploadKeep is how long content uploaded but not yet part of a commit is
	// kept: the commit follows the upload within minutes for the Hub's clients.
	uploadKeep = 24 * time.Hour
	// uploadURLLifetime is how long an upload link works.
	uploadURLLifetime = 12 * time.Hour
	// uploadIdle is how long a body upload may stall before it is cut off.
	uploadIdle = 2 * time.Minute
)

// lfsSuffixes are the file types the Hub's default .gitattributes sends through
// Git LFS whatever their size.
var lfsSuffixes = []string{
	".7z", ".arrow", ".bin", ".bz2", ".ckpt", ".ftz", ".gz", ".h5", ".joblib", ".lz4", ".mlmodel", ".model",
	".msgpack", ".npy", ".npz", ".onnx", ".ot", ".parquet", ".pb", ".pickle", ".pkl", ".pt", ".pth", ".rar",
	".safetensors", ".tar", ".tflite", ".tgz", ".wasm", ".xz", ".zip", ".zst", ".gguf", ".ggml", ".pdparams",
}

// usesLFS decides whether a file is sent through Git LFS: by size, and by the
// types the Hub treats that way.
func usesLFS(filePath string, size int64) bool {
	if size > regularFileLimit {
		return true
	}
	lower := strings.ToLower(filePath)
	for _, suffix := range lfsSuffixes {
		if strings.HasSuffix(lower, suffix) {
			return true
		}
	}
	return strings.Contains(lower, "tfevents") || strings.HasPrefix(lower, "saved_model/") || strings.Contains(path.Base(lower), ".tar.")
}

// validRepoFilePath accepts paths a Git tree can hold and a file system can
// round-trip without surprises.
func validRepoFilePath(p string) bool {
	if p == "" || len(p) > 1024 || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") || strings.Contains(p, "\\") || hasControl(p) {
		return false
	}
	for _, segment := range strings.Split(p, "/") {
		if segment == "" || segment == "." || segment == ".." || strings.EqualFold(segment, ".git") {
			return false
		}
	}
	return true
}

// gitBlobOID is the Git object name of a file's content.
func gitBlobOID(content []byte) string {
	h := sha1.New() //nolint:gosec // see the import.
	fmt.Fprintf(h, "blob %d\x00", len(content))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func validOID(oid string) bool {
	if len(oid) != 64 {
		return false
	}
	for _, r := range oid {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// contentTypeFor picks a content type from the file name. The client's own
// claim is never trusted: this is what a browser would be told about the file.
func contentTypeFor(filePath string) string {
	if t := mime.TypeByExtension(strings.ToLower(path.Ext(filePath))); t != "" {
		return t
	}
	return "application/octet-stream"
}

// apiPreupload is POST /api/{type}/{id}/preupload/{revision}: for each file, how
// it should be sent. Files already in the repository come back with their
// object name so that unchanged ones are skipped.
func (s *Server) apiPreupload(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	if _, ok := s.writableRepo(w, r, route); !ok {
		return
	}
	if len(route.Rest) != 2 {
		hfError(w, http.StatusNotFound, "", "Revision route is incomplete.")
		return
	}
	if r.URL.Query().Get("create_pr") != "" && r.URL.Query().Get("create_pr") != "0" {
		hfError(w, http.StatusBadRequest, "", "Pull requests are not supported on this proxy.")
		return
	}
	var in struct {
		Files []struct {
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"files"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		hfError(w, http.StatusBadRequest, "", "The request body is not valid JSON.")
		return
	}
	sha, ok := s.resolveRevisionOr404(w, r, repo.ID, route.Rest[1])
	if !ok {
		return
	}
	existing, err := s.db.LocalFilesAt(r.Context(), repo.ID, sha)
	if err != nil {
		hfError(w, http.StatusInternalServerError, "", "The files could not be listed.")
		return
	}
	byPath := make(map[string]db.LocalFile, len(existing))
	for _, f := range existing {
		byPath[f.Path] = f
	}
	limit := s.currentConfig().MaxUploadFileSize()
	files := make([]map[string]any, 0, len(in.Files))
	for _, f := range in.Files {
		if !validRepoFilePath(f.Path) || f.Size < 0 {
			hfError(w, http.StatusBadRequest, "", fmt.Sprintf("Invalid file path or size: %q", f.Path))
			return
		}
		if f.Size > limit {
			hfError(w, http.StatusRequestEntityTooLarge, "", fmt.Sprintf("%s is %d bytes: files cannot exceed %d bytes on this proxy.", f.Path, f.Size, limit))
			return
		}
		mode := "regular"
		if f.Size > 0 && usesLFS(f.Path, f.Size) {
			mode = "lfs"
		}
		entry := map[string]any{"path": f.Path, "uploadMode": mode, "shouldIgnore": false}
		if current, found := byPath[f.Path]; found {
			entry["oid"] = current.OID
		}
		files = append(files, entry)
	}
	writeHFJSON(w, http.StatusOK, map[string]any{"files": files, "commitOid": sha})
}

// lfsRoute is /{[datasets/]namespace/name}.git/info/lfs/...
type lfsRoute struct {
	Type, RepoID string
	Rest         []string
}

func parseLFSPath(escaped string) (lfsRoute, bool) {
	trimmed := strings.TrimPrefix(escaped, "/")
	repoType := "models"
	if strings.HasPrefix(trimmed, "datasets/") {
		repoType, trimmed = "datasets", strings.TrimPrefix(trimmed, "datasets/")
	}
	i := strings.Index(trimmed, ".git/info/lfs/")
	if i < 0 {
		return lfsRoute{}, false
	}
	repoPart, err := url.PathUnescape(trimmed[:i])
	if err != nil || hasControl(repoPart) || strings.Count(repoPart, "/") != 1 || !validRepoID(repoPart) {
		return lfsRoute{}, false
	}
	var rest []string
	for _, part := range strings.Split(trimmed[i+len(".git/info/lfs/"):], "/") {
		value, err := url.PathUnescape(part)
		if err != nil || hasControl(value) {
			return lfsRoute{}, false
		}
		rest = append(rest, value)
	}
	return lfsRoute{Type: repoType, RepoID: repoPart, Rest: rest}, true
}

// serveLFS handles the Git LFS endpoints of a published repository: the batch
// negotiation, the upload of an object, and its verification.
func (s *Server) serveLFS(w http.ResponseWriter, r *http.Request) bool {
	route, ok := parseLFSPath(r.URL.EscapedPath())
	if !ok {
		return false
	}
	repo, err := s.db.FindRepository(r.Context(), route.Type, route.RepoID)
	if err != nil || repo.Origin != "local" {
		lfsError(w, http.StatusNotFound, "Repository not found")
		return true
	}
	local := localRoute{Type: route.Type, RepoID: route.RepoID, Namespace: strings.SplitN(route.RepoID, "/", 2)[0], Name: strings.SplitN(route.RepoID, "/", 2)[1]}
	switch {
	case len(route.Rest) == 2 && route.Rest[0] == "objects" && route.Rest[1] == "batch" && r.Method == http.MethodPost:
		s.lfsBatch(w, r, local, repo)
	case len(route.Rest) == 2 && route.Rest[0] == "objects" && validOID(route.Rest[1]) && r.Method == http.MethodPut:
		s.lfsUpload(w, r, local, repo, route.Rest[1])
	case len(route.Rest) == 1 && route.Rest[0] == "verify" && r.Method == http.MethodPost:
		s.lfsVerify(w, r, local, repo)
	default:
		lfsError(w, http.StatusNotFound, "Unsupported Git LFS route")
	}
	return true
}

func lfsError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
}

// lfsObjectURL builds the link an object is uploaded to, signed so that the
// upload, which the Hub's clients send without credentials, is still tied to
// the person and the object it was granted for.
func (s *Server) lfsObjectURL(r *http.Request, route localRoute, repoPK, oid string, size int64, userID string) string {
	expires := time.Now().Add(uploadURLLifetime).Unix()
	signature := hex.EncodeToString(s.keyRing().Hash(lfsMessage(repoPK, oid, size, expires, userID)))
	query := url.Values{"size": {strconv.FormatInt(size, 10)}, "exp": {strconv.FormatInt(expires, 10)}, "u": {userID}, "sig": {signature}}
	return s.publicBase(r) + "/" + repoURLPrefix(route.Type) + route.RepoID + ".git/info/lfs/objects/" + oid + "?" + query.Encode()
}

func lfsMessage(repoPK, oid string, size, expires int64, userID string) string {
	return fmt.Sprintf("lfs-upload\x00%s\x00%s\x00%d\x00%d\x00%s", repoPK, oid, size, expires, userID)
}

func (s *Server) checkLFSSignature(r *http.Request, repoPK, oid string) (userID string, size int64, ok bool) {
	q := r.URL.Query()
	size, errSize := strconv.ParseInt(q.Get("size"), 10, 64)
	expires, errExp := strconv.ParseInt(q.Get("exp"), 10, 64)
	signature, err := hex.DecodeString(q.Get("sig"))
	if errSize != nil || errExp != nil || err != nil || size < 0 || time.Now().Unix() > expires {
		return "", 0, false
	}
	userID = q.Get("u")
	for _, candidate := range s.keyRing().HashCandidates(lfsMessage(repoPK, oid, size, expires, userID)) {
		if subtle.ConstantTimeCompare(candidate, signature) == 1 {
			return userID, size, true
		}
	}
	return "", 0, false
}

// ownerQuotaExceeded says whether adding the listed contents (hash and size)
// would take the repository's owner over the storage quota. Content the owner's
// repositories already hold is not counted again.
func (s *Server) ownerQuotaExceeded(ctx context.Context, ownerID string, sizes map[string]int64) (bool, int64, error) {
	quota := s.currentConfig().UploadUserQuota
	if quota <= 0 || ownerID == "" {
		return false, 0, nil
	}
	used, err := s.db.OwnerLocalBytes(ctx, ownerID)
	if err != nil {
		return false, 0, err
	}
	shas := make([]string, 0, len(sizes))
	for sha := range sizes {
		shas = append(shas, sha)
	}
	owned, err := s.db.OwnedContent(ctx, ownerID, shas)
	if err != nil {
		return false, 0, err
	}
	extra := int64(0)
	for sha, size := range sizes {
		if !owned[sha] {
			extra += size
		}
	}
	return used+extra > quota, quota, nil
}

// lfsBatch is POST .../info/lfs/objects/batch.
func (s *Server) lfsBatch(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p := s.publisherLFS(w, r)
	if p == nil {
		return
	}
	if _, status, message := s.authorizeNamespace(r.Context(), p, route.Namespace); status != 0 {
		lfsError(w, status, message)
		return
	}
	var in struct {
		Operation string `json:"operation"`
		Objects   []struct {
			OID  string `json:"oid"`
			Size int64  `json:"size"`
		} `json:"objects"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		lfsError(w, http.StatusBadRequest, "The request body is not valid JSON.")
		return
	}
	if in.Operation != "upload" {
		lfsError(w, http.StatusUnprocessableEntity, "Only uploads go through Git LFS here: download files from their /resolve/ address.")
		return
	}
	limit := s.currentConfig().MaxUploadFileSize()
	sizes := map[string]int64{}
	for _, o := range in.Objects {
		if validOID(o.OID) && o.Size > 0 && o.Size <= limit {
			sizes[o.OID] = o.Size
		}
	}
	overQuota, quota, err := s.ownerQuotaExceeded(r.Context(), repo.OwnerID, sizes)
	if err != nil {
		lfsError(w, http.StatusInternalServerError, "The storage quota could not be checked.")
		return
	}
	objects := make([]map[string]any, 0, len(in.Objects))
	for _, o := range in.Objects {
		entry := map[string]any{"oid": o.OID, "size": o.Size}
		switch {
		case !validOID(o.OID) || o.Size < 0:
			entry["error"] = map[string]any{"code": 422, "message": "Invalid object name or size."}
		case o.Size > limit:
			entry["error"] = map[string]any{"code": 413, "message": fmt.Sprintf("Files cannot exceed %d bytes on this proxy.", limit)}
		case s.blobIsKept(r.Context(), o.OID, o.Size):
			// Already here: no actions tells the client there is nothing to send.
		case overQuota:
			entry["error"] = map[string]any{"code": 413, "message": fmt.Sprintf("This would take the owner of the repository over their storage quota of %d bytes.", quota)}
		default:
			entry["authenticated"] = true
			entry["actions"] = map[string]any{
				"upload": map[string]any{"href": s.lfsObjectURL(r, route, repo.ID, o.OID, o.Size, p.ID), "header": map[string]string{}},
				"verify": map[string]any{"href": s.publicBase(r) + "/" + repoURLPrefix(route.Type) + route.RepoID + ".git/info/lfs/verify", "header": map[string]string{}},
			}
		}
		objects = append(objects, entry)
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_ = json.NewEncoder(w).Encode(map[string]any{"transfer": "basic", "objects": objects, "hash_algo": "sha256"})
}

// publisherLFS is publisher() answering in Git LFS's error format.
func (s *Server) publisherLFS(w http.ResponseWriter, r *http.Request) *Principal {
	recorder := &errorCapture{header: http.Header{}}
	p := s.publisher(recorder, r)
	if p == nil {
		for key, values := range recorder.header {
			if key == "Www-Authenticate" {
				w.Header()[key] = values
			}
		}
		lfsError(w, recorder.status, recorder.message())
		return nil
	}
	return p
}

// errorCapture catches the Hub-format error publisher() writes so it can be
// re-sent in another format.
type errorCapture struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (e *errorCapture) Header() http.Header         { return e.header }
func (e *errorCapture) WriteHeader(status int)      { e.status = status }
func (e *errorCapture) Write(p []byte) (int, error) { return e.body.Write(p) }
func (e *errorCapture) message() string {
	var out struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(e.body.Bytes(), &out)
	if out.Error == "" {
		return "Not allowed"
	}
	return out.Error
}

// blobIsKept says whether the content is already stored and will stay: known to
// the catalog with this size, and not about to be collected.
func (s *Server) blobIsKept(ctx context.Context, sha string, size int64) bool {
	stored, _, err := s.db.BlobBySHA256(ctx, sha)
	if err != nil || stored != size {
		return false
	}
	ok, err := s.db.BlobIsRetained(ctx, sha)
	return err == nil && ok
}

// deadlineReader keeps pushing a request's read deadline forward while data
// arrives, so a big upload is not cut by the API's overall read timeout but a
// stalled one still is.
type deadlineReader struct {
	r          io.Reader
	controller *http.ResponseController
	idle       time.Duration
}

func (d *deadlineReader) Read(p []byte) (int, error) {
	_ = d.controller.SetReadDeadline(time.Now().Add(d.idle))
	return d.r.Read(p)
}

// lfsUpload is PUT .../info/lfs/objects/{oid}: the bytes of one file. The link
// carries its own signature because the Hub's clients send no credentials with it.
func (s *Server) lfsUpload(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess, oid string) {
	userID, size, ok := s.checkLFSSignature(r, repo.ID, oid)
	if !ok {
		lfsError(w, http.StatusForbidden, "This upload link is invalid or has expired. Start the upload again.")
		return
	}
	if !s.currentConfig().CustomRepos {
		lfsError(w, http.StatusForbidden, "Publishing is turned off on this proxy.")
		return
	}
	if size > s.currentConfig().MaxUploadFileSize() {
		lfsError(w, http.StatusRequestEntityTooLarge, "File too large.")
		return
	}
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	body := &deadlineReader{r: r.Body, controller: controller, idle: uploadIdle}

	if s.blobIsKept(r.Context(), oid, size) {
		_, _ = io.Copy(io.Discard, io.LimitReader(body, size+1))
		w.WriteHeader(http.StatusOK)
		return
	}
	release, err := s.reserveStaging(size)
	if err != nil {
		w.Header().Set("Retry-After", "30")
		lfsError(w, http.StatusInsufficientStorage, "Not enough staging space on the proxy right now. Try again shortly.")
		return
	}
	defer release()
	file, err := s.createStagingFile("upload-*")
	if err != nil {
		lfsError(w, http.StatusInternalServerError, "The upload could not be staged.")
		return
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(file, hash), io.LimitReader(body, size+1))
	if err != nil {
		s.logger.Warn("upload interrupted", "request_id", requestID(r.Context()), "user_id", userID, "received", n)
		lfsError(w, http.StatusBadRequest, "The upload was interrupted.")
		return
	}
	if n != size {
		lfsError(w, http.StatusBadRequest, fmt.Sprintf("Expected %d bytes, received %d.", size, n))
		return
	}
	if hex.EncodeToString(hash.Sum(nil)) != oid {
		lfsError(w, http.StatusUnprocessableEntity, "The content does not match its SHA-256 object name.")
		return
	}
	if err := s.storeUploadedBlob(r.Context(), file, oid, size); err != nil {
		s.logger.Error("uploaded object could not be stored", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
		lfsError(w, http.StatusBadGateway, "The object could not be stored.")
		return
	}
	if s.metrics != nil {
		s.metrics.add("hfproxy_published_bytes_total", nil, float64(size))
	}
	w.WriteHeader(http.StatusOK)
}

// storeUploadedBlob puts staged content in object storage and records it.
func (s *Server) storeUploadedBlob(ctx context.Context, file *os.File, sha string, size int64) error {
	objects := s.currentStorage()
	if objects == nil {
		return errors.New("object storage is not configured")
	}
	key := objects.Key(sha)
	if err := objects.UploadFile(ctx, file, key, "application/octet-stream", size); err != nil {
		return err
	}
	return s.db.RegisterUploadedBlob(ctx, sha, key, "application/octet-stream", size, uploadKeep)
}

// lfsVerify is POST .../info/lfs/verify: the client asks whether the object it
// sent is there.
func (s *Server) lfsVerify(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	if p := s.publisherLFS(w, r); p == nil {
		return
	}
	var in struct {
		OID  string `json:"oid"`
		Size int64  `json:"size"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || !validOID(in.OID) {
		lfsError(w, http.StatusBadRequest, "Invalid verification request.")
		return
	}
	if !s.blobIsKept(r.Context(), in.OID, in.Size) {
		lfsError(w, http.StatusNotFound, "The object is not stored.")
		return
	}
	w.Header().Set("Content-Type", "application/vnd.git-lfs+json")
	_, _ = w.Write([]byte("{}"))
}

// commitItem is one line of the commit request: {"key": ..., "value": {...}}.
type commitItem struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// apiCommit is POST /api/{type}/{id}/commit/{revision}: the Hub's commit API,
// a stream of JSON lines (a header, then one per file added or removed).
func (s *Server) apiCommit(w http.ResponseWriter, r *http.Request, route localRoute, repo db.RepositoryAccess) {
	p, ok := s.writableRepo(w, r, route)
	if !ok {
		return
	}
	if len(route.Rest) != 2 {
		hfError(w, http.StatusNotFound, "", "Revision route is incomplete.")
		return
	}
	if q := r.URL.Query().Get("create_pr"); q != "" && q != "0" {
		hfError(w, http.StatusBadRequest, "", "Pull requests are not supported on this proxy.")
		return
	}
	branch := strings.TrimPrefix(route.Rest[1], "refs/heads/")
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, io.NopCloser(&deadlineReader{r: r.Body, controller: controller, idle: uploadIdle}), commitBodyLimit)
	decoder := json.NewDecoder(r.Body)

	var header struct {
		Summary       string `json:"summary"`
		Description   string `json:"description"`
		ParentCommit  string `json:"parentCommit"`
		CreatePR      bool   `json:"createPr"`
		AuthorName    string `json:"authorName"`
		AuthorEmail   string `json:"authorEmail"`
		HasHeaderItem bool   `json:"-"`
	}
	var ops []db.CommitOp
	var pendingCopies []struct{ Dest, Source, Revision string }
	limit := s.currentConfig().MaxUploadFileSize()
	sizes := map[string]int64{}
	for decoder.More() {
		var item commitItem
		if err := decoder.Decode(&item); err != nil {
			hfError(w, http.StatusBadRequest, "", "The commit request is not valid JSON lines: "+err.Error())
			return
		}
		switch item.Key {
		case "header":
			if err := json.Unmarshal(item.Value, &header); err != nil {
				hfError(w, http.StatusBadRequest, "", "Invalid commit header.")
				return
			}
			header.HasHeaderItem = true
		case "file":
			var f struct {
				Path     string `json:"path"`
				Content  string `json:"content"`
				Encoding string `json:"encoding"`
			}
			if err := json.Unmarshal(item.Value, &f); err != nil || !validRepoFilePath(f.Path) {
				hfError(w, http.StatusBadRequest, "", "Invalid file in the commit.")
				return
			}
			var content []byte
			switch strings.ToLower(f.Encoding) {
			case "base64":
				decoded, err := base64.StdEncoding.DecodeString(f.Content)
				if err != nil {
					hfError(w, http.StatusBadRequest, "", "File "+f.Path+" is not valid base64.")
					return
				}
				content = decoded
			case "", "utf-8", "utf8":
				content = []byte(f.Content)
			default:
				hfError(w, http.StatusBadRequest, "", "Unknown encoding for "+f.Path+".")
				return
			}
			if len(content) > inlineFileLimit || int64(len(content)) > limit {
				hfError(w, http.StatusRequestEntityTooLarge, "", "File "+f.Path+" is too large to send inside a commit: use Git LFS.")
				return
			}
			sum := sha256.Sum256(content)
			sha := hex.EncodeToString(sum[:])
			if err := s.storeInlineBlob(r.Context(), content, sha); err != nil {
				s.logger.Error("committed file could not be stored", "request_id", requestID(r.Context()), "error", safeLogValue(err.Error(), 300))
				hfError(w, http.StatusBadGateway, "", "The file "+f.Path+" could not be stored.")
				return
			}
			ops = append(ops, db.CommitOp{Kind: db.OpAdd, Path: f.Path, SHA256: sha, OID: gitBlobOID(content), Size: int64(len(content)), ContentType: contentTypeFor(f.Path)})
			sizes[sha] = int64(len(content))
		case "lfsFile":
			var f struct {
				Path string `json:"path"`
				Algo string `json:"algo"`
				OID  string `json:"oid"`
				Size int64  `json:"size"`
			}
			if err := json.Unmarshal(item.Value, &f); err != nil || !validRepoFilePath(f.Path) || !validOID(f.OID) || (f.Algo != "" && f.Algo != "sha256") {
				hfError(w, http.StatusBadRequest, "", "Invalid LFS file in the commit.")
				return
			}
			size, _, err := s.db.BlobBySHA256(r.Context(), f.OID)
			if err != nil || (f.Size != 0 && f.Size != size) {
				hfError(w, http.StatusBadRequest, "", "The content of "+f.Path+" was not uploaded (object "+f.OID+").")
				return
			}
			ops = append(ops, db.CommitOp{Kind: db.OpAdd, Path: f.Path, SHA256: f.OID, OID: f.OID, Size: size, LFS: true, ContentType: contentTypeFor(f.Path)})
			sizes[f.OID] = size
		case "deletedFile", "deletedFolder":
			var f struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(item.Value, &f); err != nil || !validRepoFilePath(strings.TrimSuffix(f.Path, "/")) {
				hfError(w, http.StatusBadRequest, "", "Invalid path to delete.")
				return
			}
			kind := db.OpDeleteFile
			if item.Key == "deletedFolder" {
				kind = db.OpDeleteFolder
			}
			ops = append(ops, db.CommitOp{Kind: kind, Path: strings.TrimSuffix(f.Path, "/")})
		case "copyFile":
			var f struct {
				Path        string `json:"path"`
				SrcPath     string `json:"srcPath"`
				SrcRevision string `json:"srcRevision"`
			}
			if err := json.Unmarshal(item.Value, &f); err != nil || !validRepoFilePath(f.Path) || !validRepoFilePath(f.SrcPath) {
				hfError(w, http.StatusBadRequest, "", "Invalid file copy in the commit.")
				return
			}
			pendingCopies = append(pendingCopies, struct{ Dest, Source, Revision string }{f.Path, f.SrcPath, f.SrcRevision})
			ops = append(ops, db.CommitOp{Kind: db.OpAdd, Path: f.Path}) // filled below, in order
		default:
			hfError(w, http.StatusBadRequest, "", "Unknown commit operation: "+item.Key)
			return
		}
	}
	if !header.HasHeaderItem || strings.TrimSpace(header.Summary) == "" {
		hfError(w, http.StatusBadRequest, "", "A commit needs a header with a summary.")
		return
	}
	if header.CreatePR {
		hfError(w, http.StatusBadRequest, "", "Pull requests are not supported on this proxy.")
		return
	}
	if err := s.resolveCopies(r.Context(), repo.ID, branch, ops, pendingCopies); err != nil {
		hfError(w, http.StatusNotFound, "EntryNotFound", err.Error())
		return
	}
	author := header.AuthorName
	if author == "" {
		author = p.Username
	}
	commit, failure := s.commitOps(r.Context(), p, repo.ID, repo.OwnerID, branch, header.ParentCommit, author, header.Summary, header.Description, ops, sizes)
	if failure != nil {
		hfError(w, failure.status, failure.code, failure.message)
		return
	}
	s.audit(r.Context(), p.ID, "repository.commit", "repository", repo.ID, map[string]any{"commit": commit.SHA, "branch": branch, "operations": len(ops), "summary": safeLogValue(commit.Summary, 120)})
	writeHFJSON(w, http.StatusOK, map[string]any{
		"success": true, "commitOid": commit.SHA, "hookOutput": "",
		"commitUrl": s.publicBase(r) + "/" + repoURLPrefix(route.Type) + route.RepoID + "/commit/" + commit.SHA,
	})
}

// commitFailure is why a commit was refused, in the Hub's terms.
type commitFailure struct {
	status        int
	code, message string
}

// commitOps checks the storage quota and records a commit on a branch. sizes
// names the contents the commit adds (hash and size) for the quota. The commit
// API and the console's uploads both end here.
func (s *Server) commitOps(ctx context.Context, p *Principal, repoPK, ownerID, branch, parent, author, summary, description string, ops []db.CommitOp, sizes map[string]int64) (db.LocalCommit, *commitFailure) {
	if strings.TrimSpace(summary) == "" {
		return db.LocalCommit{}, &commitFailure{http.StatusBadRequest, "", "A commit needs a summary."}
	}
	overQuota, quota, err := s.ownerQuotaExceeded(ctx, ownerID, sizes)
	if err != nil {
		return db.LocalCommit{}, &commitFailure{http.StatusInternalServerError, "", "The storage quota could not be checked."}
	}
	if overQuota {
		return db.LocalCommit{}, &commitFailure{http.StatusRequestEntityTooLarge, "", fmt.Sprintf("This would take the owner of the repository over their storage quota of %d bytes.", quota)}
	}
	commit, err := s.db.CommitLocal(ctx, db.CommitInput{
		RepositoryID: repoPK, Branch: branch, ParentCommit: parent,
		AuthorID: p.ID, AuthorName: author, Summary: strings.TrimSpace(summary), Description: description, Ops: ops,
	})
	var missing *db.BlobMissingError
	switch {
	case errors.Is(err, db.ErrRevisionNotFound):
		return db.LocalCommit{}, &commitFailure{http.StatusNotFound, "RevisionNotFound", "Invalid rev id: " + branch + " (commits go to a branch)"}
	case errors.Is(err, db.ErrParentMismatch):
		return db.LocalCommit{}, &commitFailure{http.StatusPreconditionFailed, "", "The branch has moved since the parent commit you gave. Fetch it again and retry."}
	case errors.Is(err, db.ErrPathConflict):
		return db.LocalCommit{}, &commitFailure{http.StatusBadRequest, "", err.Error()}
	case errors.As(err, &missing):
		return db.LocalCommit{}, &commitFailure{http.StatusBadRequest, "", "The content " + missing.SHA256 + " was not uploaded."}
	case err != nil:
		s.logger.Error("commit failed", "request_id", requestID(ctx), "error", safeLogValue(err.Error(), 300))
		return db.LocalCommit{}, &commitFailure{http.StatusInternalServerError, "", "The commit could not be recorded."}
	}
	if s.metrics != nil {
		s.metrics.add("hfproxy_published_commits_total", nil, 1)
	}
	return commit, nil
}

// storeInlineBlob stores the content of a file sent inside a commit.
func (s *Server) storeInlineBlob(ctx context.Context, content []byte, sha string) error {
	if s.blobIsKept(ctx, sha, int64(len(content))) {
		return nil
	}
	release, err := s.reserveStaging(int64(len(content)))
	if err != nil {
		return err
	}
	defer release()
	file, err := s.createStagingFile("inline-*")
	if err != nil {
		return err
	}
	defer func() {
		file.Close()
		os.Remove(file.Name())
	}()
	if _, err := file.Write(content); err != nil {
		return err
	}
	return s.storeUploadedBlob(ctx, file, sha, int64(len(content)))
}

// resolveCopies fills in the copy operations of a commit from the files they
// copy: another path of this repository, at the given revision (default: the
// branch being committed to).
func (s *Server) resolveCopies(ctx context.Context, repoPK, branch string, ops []db.CommitOp, copies []struct{ Dest, Source, Revision string }) error {
	if len(copies) == 0 {
		return nil
	}
	next := 0
	for i := range ops {
		if ops[i].Kind != db.OpAdd || ops[i].SHA256 != "" || next >= len(copies) || ops[i].Path != copies[next].Dest {
			continue
		}
		c := copies[next]
		next++
		revision := c.Revision
		if revision == "" {
			revision = branch
		}
		sha, err := s.db.ResolveLocalRevision(ctx, repoPK, revision)
		if err != nil {
			return fmt.Errorf("cannot copy %s: revision %q not found", c.Source, revision)
		}
		files, err := s.db.LocalFilesAt(ctx, repoPK, sha)
		if err != nil {
			return err
		}
		found := false
		for _, f := range files {
			if f.Path == c.Source {
				ops[i] = db.CommitOp{Kind: db.OpAdd, Path: c.Dest, SHA256: f.SHA256, OID: f.OID, Size: f.Size, LFS: f.LFS, ContentType: f.ContentType}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("cannot copy %s: it does not exist at %s", c.Source, revision)
		}
	}
	return nil
}
