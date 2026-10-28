package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
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

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/nicolas/hfproxy/internal/db"
	"github.com/nicolas/hfproxy/internal/storage"
)

type artifact struct {
	ID          string
	Repository  string
	Commit      string
	Path        string
	BlobID      string
	StorageKey  string
	SHA256      string
	Size        int64
	ETag        string
	ContentType string
	LastMod     sql.NullTime
}

func shouldStreamCacheMiss(r *http.Request, expectedLFS string) bool {
	return expectedLFS == "" && !isPreloadFetch(r.Context()) && r.Method == http.MethodGet &&
		r.Header.Get("Range") == "" && r.Header.Get("If-Range") == "" &&
		r.Header.Get("If-None-Match") == "" && r.Header.Get("If-Modified-Since") == ""
}

func cachePublishContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), 30*time.Minute)
}

func writeStreamAwareProblem(w http.ResponseWriter, r *http.Request, streamed bool, status int, title, detail string) {
	if streamed {
		panic(http.ErrAbortHandler)
	}
	writeProblem(w, r, status, title, detail)
}

type resolvePath struct{ RepoType, RepoID, Revision, FilePath string }

type downloadProgressWriter struct {
	ctx         context.Context
	db          *sql.DB
	id          string
	baseBytes   int64
	destination io.Writer
	bytes       int64
	lastPersist time.Time
}

func (w *downloadProgressWriter) Write(chunk []byte) (int, error) {
	n, err := w.destination.Write(chunk)
	w.bytes += int64(n)
	if time.Since(w.lastPersist) >= time.Second {
		w.lastPersist = time.Now()
		_, _ = w.db.ExecContext(w.ctx, `UPDATE downloads SET bytes=$2 WHERE id=$1 AND status='downloading'`, w.id, w.baseBytes+w.bytes)
	}
	return n, err
}

func upstreamResolvePath(parsed resolvePath) string {
	repoTypePrefix := ""
	if parsed.RepoType != "models" {
		repoTypePrefix = parsed.RepoType + "/"
	}
	return "/" + repoTypePrefix + escapeRepoPath(parsed.RepoID) + "/resolve/" + url.PathEscape(parsed.Revision) + "/" + escapeFilePath(parsed.FilePath)
}

func (s *Server) resolveFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	parsed, err := parseResolvePath(r.URL.EscapedPath())
	if err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	p, authErr := s.authenticate(r)
	repoPK, policy, err := s.repositoryAccess(r.Context(), p, authErr, parsed.RepoType, parsed.RepoID)
	if err != nil {
		s.accessProblem(w, r, err)
		return
	}
	resolvedCommit := ""
	staleCommit := ""
	if validCommit(parsed.Revision) {
		resolvedCommit = parsed.Revision
	} else {
		_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2 AND resolved_at>now()-($3 * interval '1 second')`, repoPK, parsed.Revision, s.currentConfig().CacheRefTTL.Seconds()).Scan(&resolvedCommit)
		if !validCommit(resolvedCommit) {
			resolvedCommit = ""
			// Remember the last known commit, however old, so the cache can answer
			// if the Hub cannot confirm the ref.
			_ = s.db.SQL.QueryRowContext(r.Context(), `SELECT commit_sha FROM revisions WHERE repository_id=$1 AND requested_ref=$2`, repoPK, parsed.Revision).Scan(&staleCommit)
			if !validCommit(staleCommit) {
				staleCommit = ""
			}
		}
	}
	if resolvedCommit != "" {
		if cached, lookupErr := s.findArtifact(r.Context(), s.db.SQL, repoPK, resolvedCommit, parsed.FilePath); lookupErr == nil {
			_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1 AND last_accessed_at < now()-interval '1 minute'`, cached.ID)
			s.recordDownload(r.Context(), repoPK, resolvedCommit, parsed.FilePath, "hit", cached.Size, "")
			s.serveOrAcknowledgePreload(w, r, cached, "HIT")
			return
		} else if !isNoRows(lookupErr) {
			writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
			return
		}
	}
	requestURL := upstreamResolvePath(parsed)
	negativeKey := repoPK + "|" + parsed.Revision + "|" + parsed.FilePath
	if status, headers, ok := s.negativeLookup(negativeKey); ok {
		if s.metrics != nil {
			s.metrics.cacheDecision(r.Context(), "bypass")
		}
		s.hubStatus(w, r, status, headers)
		return
	}
	upstream, firstHeaders, err := s.fetchHub(r.Context(), http.MethodHead, requestURL, safeResolveQuery(r.URL.Query()), nil)
	if err != nil {
		if s.serveStale(w, r, repoPK, staleCommit, parsed) {
			return
		}
		s.hubFailure(w, r, err)
		return
	}
	defer upstream.Body.Close()
	if upstream.StatusCode == http.StatusTooManyRequests || upstream.StatusCode >= 500 {
		if s.serveStale(w, r, repoPK, staleCommit, parsed) {
			return
		}
	}
	if upstream.StatusCode < 200 || upstream.StatusCode >= 300 {
		if upstream.StatusCode == http.StatusNotFound || upstream.StatusCode == http.StatusGone {
			s.negativeStore(negativeKey, upstream.StatusCode, upstream.Header)
		}
		s.hubStatus(w, r, upstream.StatusCode, upstream.Header)
		return
	}
	commit := firstHeaders.Get("X-Repo-Commit")
	if commit == "" {
		commit = upstream.Header.Get("X-Repo-Commit")
	}
	if commit == "" && validCommit(parsed.Revision) {
		commit = parsed.Revision
	}
	if commit == "" || !validCommit(commit) {
		commit, err = s.resolveCommit(r.Context(), parsed)
		if err != nil {
			s.hubFailure(w, r, err)
			return
		}
	}
	if !validCommit(parsed.Revision) {
		_, _ = s.db.SQL.ExecContext(r.Context(), `INSERT INTO revisions(id,repository_id,requested_ref,commit_sha,resolved_at) VALUES($1,$2,$3,$4,now())
			ON CONFLICT(repository_id,requested_ref) DO UPDATE SET commit_sha=EXCLUDED.commit_sha,resolved_at=now()`, db.NewID(), repoPK, parsed.Revision, commit)
	}
	meta := artifact{Repository: repoPK, Commit: commit, Path: parsed.FilePath, ETag: upstream.Header.Get("ETag"), ContentType: upstream.Header.Get("Content-Type")}
	if linked := firstHeaders.Get("X-Linked-ETag"); lfsSHA256(linked) != "" {
		meta.ETag = linked
	} else if meta.ETag == "" {
		meta.ETag = linked
	}
	if lm := upstream.Header.Get("Last-Modified"); lm != "" {
		if t, e := http.ParseTime(lm); e == nil {
			meta.LastMod = sql.NullTime{Time: t, Valid: true}
		}
	}
	sizeKnown := false
	if size := firstHeaders.Get("X-Linked-Size"); size != "" {
		if parsedSize, parseErr := strconv.ParseInt(size, 10, 64); parseErr == nil && parsedSize >= 0 {
			meta.Size = parsedSize
			sizeKnown = true
		}
	}
	if !sizeKnown && upstream.ContentLength >= 0 {
		meta.Size = upstream.ContentLength
		sizeKnown = true
	}
	if meta.ContentType == "" {
		meta.ContentType = "application/octet-stream"
	}
	cached, cacheErr := s.findArtifact(r.Context(), s.db.SQL, repoPK, commit, parsed.FilePath)
	if cacheErr != nil && !isNoRows(cacheErr) {
		writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
		return
	}
	if cacheErr == nil {
		_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1 AND last_accessed_at < now()-interval '1 minute'`, cached.ID)
		s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
		s.serveOrAcknowledgePreload(w, r, cached, "HIT")
		return
	}
	if r.Method == http.MethodHead {
		if ifNoneMatch(r.Header.Get("If-None-Match"), meta.ETag) || (r.Header.Get("If-None-Match") == "" && meta.LastMod.Valid && notModifiedSince(r.Header.Get("If-Modified-Since"), meta.LastMod.Time)) {
			s.setArtifactHeaders(w, meta, upstream.Header)
			w.Header().Set("X-Repo-Commit", commit)
			w.Header().Set("X-Cache", "MISS")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		s.setArtifactHeaders(w, meta, upstream.Header)
		w.Header().Set("X-Cache", "MISS")
		w.Header().Set("X-Repo-Commit", commit)
		w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	if policy == "admin_only" && p == nil {
		writeProblem(w, r, 401, "Authentication required", "Sign in to access this repository.")
		return
	}
	tx, err := s.db.SQL.BeginTx(r.Context(), nil)
	if err != nil {
		writeProblem(w, r, 500, "Database error", "Unable to coordinate this download.")
		return
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()
	if cached, err = s.findArtifact(r.Context(), tx, repoPK, commit, parsed.FilePath); err == nil {
		if err := tx.Commit(); err != nil {
			writeProblem(w, r, 500, "Database error", "Unable to finish cache lookup.")
			return
		}
		s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
		s.serveOrAcknowledgePreload(w, r, cached, "HIT")
		return
	} else if !isNoRows(err) {
		writeProblem(w, r, 500, "Cache metadata error", "Unable to read cache metadata.")
		return
	}
	// Do not hold a database connection while downloading from the Hub or
	// uploading to object storage; progress writes use the pool separately.
	_ = tx.Rollback()
	tx = nil
	missKey := repoPK + "|" + commit + "|" + parsed.FilePath
	expectedLFS := lfsSHA256(firstHeaders.Get("X-Linked-ETag"))
	leader, done := s.joinCacheMiss(missKey)
	if r.Method == http.MethodGet && expectedLFS == "" && r.Header.Get("Range") != "" {
		if leader {
			defer s.finishCacheMiss(missKey)
		}
		s.streamUncachedMiss(w, r, parsed, meta, commit)
		return
	}
	if !leader {
		if r.Method == http.MethodGet && expectedLFS == "" {
			s.streamUncachedMiss(w, r, parsed, meta, commit)
			return
		}
		select {
		case <-done:
			s.resolveFile(w, r)
		case <-r.Context().Done():
		}
		return
	}
	defer s.finishCacheMiss(missKey)
	releaseSlot, slotErr := s.acquireFillSlot(r.Context())
	if slotErr != nil {
		if errors.Is(slotErr, errFillBusy) {
			w.Header().Set("Retry-After", "5")
			writeProblem(w, r, http.StatusServiceUnavailable, "Cache fill capacity exhausted", "Too many files are being fetched right now; retry shortly.")
		}
		return
	}
	defer releaseSlot()
	downloadID := db.NewID()
	caller := callerOf(r.Context())
	_, _ = s.db.SQL.ExecContext(r.Context(), `INSERT INTO downloads(id,repository_id,commit_sha,path,status,bytes,user_id,token_id,client_ip) VALUES($1,$2,$3,$4,'downloading',0,NULLIF($5,''),NULLIF($6,''),NULLIF($7,''))`, downloadID, repoPK, commit, parsed.FilePath, caller.UserID, caller.TokenID, caller.ClientIP)
	objectStore := s.currentStorage()
	if meta.Size > s.currentConfig().CacheMaxFileSize {
		s.finishDownload(r.Context(), downloadID, 0, "size_limit")
		writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
		return
	}
	if sizeKnown && meta.Size > 0 {
		releaseStaging, stagingErr := s.reserveStaging(meta.Size)
		if stagingErr != nil {
			s.finishDownload(r.Context(), downloadID, 0, "staging_space")
			w.Header().Set("Retry-After", "30")
			writeProblem(w, r, http.StatusInsufficientStorage, "Insufficient staging space", "The proxy does not have enough free disk space to stage this file right now.")
			return
		}
		defer releaseStaging()
	}
	var tmp *os.File
	var body *http.Response
	var size int64
	var digest string
	var resume *downloadResume
	streamed := false
	if sizeKnown {
		unlockResume, lockErr := s.lockDownloadResume(r.Context(), repoPK, commit, parsed.FilePath)
		if lockErr != nil {
			s.finishDownload(r.Context(), downloadID, 0, "resume_lock")
			writeProblem(w, r, http.StatusInternalServerError, "Cache metadata error", "Unable to coordinate the cache fill.")
			return
		}
		defer unlockResume()
		if cached, lookupErr := s.findArtifact(r.Context(), s.db.SQL, repoPK, commit, parsed.FilePath); lookupErr == nil {
			_, _ = s.db.SQL.ExecContext(r.Context(), `UPDATE artifacts SET last_accessed_at=now() WHERE id=$1 AND last_accessed_at < now()-interval '1 minute'`, cached.ID)
			s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
			s.serveOrAcknowledgePreload(w, r, cached, "HIT")
			return
		} else if !isNoRows(lookupErr) {
			writeProblem(w, r, http.StatusInternalServerError, "Cache metadata error", "Unable to recheck cache metadata.")
			return
		}
	}
	if sizeKnown {
		resumeETag := upstream.Header.Get("ETag")
		if resumeETag == "" {
			resumeETag = firstHeaders.Get("ETag")
		}
		var stream io.Writer
		if shouldStreamCacheMiss(r, expectedLFS) {
			s.setArtifactHeaders(w, meta, firstHeaders)
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Repo-Commit", commit)
			w.Header().Set("Content-Length", strconv.FormatInt(meta.Size, 10))
			w.WriteHeader(http.StatusOK)
			stream = asStreamingWriter(w)
			streamed = true
		}
		tmp, digest, resume, err = s.downloadResumable(r.Context(), objectStore, downloadID, repoPK, commit, parsed, meta.Size, resumeETag, meta.ContentType, stream)
		size = meta.Size
		if err != nil {
			completed := int64(0)
			if resume != nil {
				completed = resume.CompletedBytes
			}
			s.finishDownload(r.Context(), downloadID, completed, "upstream_read")
			var statusErr *upstreamStatusError
			if errors.As(err, &statusErr) {
				s.hubStatus(w, r, statusErr.status, statusErr.header)
				return
			}
			writeProblem(w, r, http.StatusBadGateway, "Upstream transfer failed", "The file could not be completed. Retry the preload to continue from the last stored block.")
			return
		}
		defer func() {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}()
	} else {
		fetchPath := upstreamResolvePath(resolvePath{RepoType: parsed.RepoType, RepoID: parsed.RepoID, Revision: commit, FilePath: parsed.FilePath})
		var headers http.Header
		body, headers, err = s.fetchHub(r.Context(), http.MethodGet, fetchPath, safeResolveQuery(r.URL.Query()), nil)
		if err != nil {
			s.finishDownload(r.Context(), downloadID, 0, "upstream")
			s.hubFailure(w, r, err)
			return
		}
		defer body.Body.Close()
		if body.StatusCode < 200 || body.StatusCode >= 300 {
			s.finishDownload(r.Context(), downloadID, 0, "upstream_status")
			s.hubStatus(w, r, body.StatusCode, body.Header)
			return
		}
		actualCommit := headers.Get("X-Repo-Commit")
		if actualCommit == "" {
			actualCommit = body.Header.Get("X-Repo-Commit")
		}
		if actualCommit != "" && actualCommit != commit {
			s.finishDownload(r.Context(), downloadID, 0, "revision_changed")
			writeProblem(w, r, http.StatusConflict, "Revision changed", "The requested branch moved during the download; retry against the resolved revision.")
			return
		}
		if body.ContentLength > s.currentConfig().CacheMaxFileSize {
			s.finishDownload(r.Context(), downloadID, 0, "size_limit")
			writeProblem(w, r, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
			return
		}
		tmp, err = s.createStagingFile("hfproxy-staging-*")
		if err != nil {
			s.finishDownload(r.Context(), downloadID, 0, "staging")
			writeProblem(w, r, 500, "Cache error", "Unable to stage the upstream file.")
			return
		}
		defer func() {
			name := tmp.Name()
			_ = tmp.Close()
			_ = os.Remove(name)
		}()
		hash := sha256.New()
		limited := io.LimitReader(body.Body, s.currentConfig().CacheMaxFileSize+1)
		destination := io.Writer(io.MultiWriter(tmp, hash))
		if shouldStreamCacheMiss(r, expectedLFS) {
			s.setArtifactHeaders(w, meta, headers)
			w.Header().Set("X-Cache", "MISS")
			w.Header().Set("X-Repo-Commit", commit)
			if body.ContentLength >= 0 {
				w.Header().Set("Content-Length", strconv.FormatInt(body.ContentLength, 10))
			}
			w.WriteHeader(http.StatusOK)
			destination = io.MultiWriter(tmp, hash, asStreamingWriter(w))
			streamed = true
		}
		progress := &downloadProgressWriter{ctx: r.Context(), db: s.db.SQL, id: downloadID, destination: destination, lastPersist: time.Now()}
		size, err = io.Copy(progress, limited)
		if err != nil {
			s.finishDownload(r.Context(), downloadID, size, "upstream_read")
			writeStreamAwareProblem(w, r, streamed, 502, "Upstream transfer failed", "The Hub response could not be completely downloaded.")
			return
		}
		if size > s.currentConfig().CacheMaxFileSize {
			s.finishDownload(r.Context(), downloadID, size, "size_limit")
			writeStreamAwareProblem(w, r, streamed, 413, "File too large", "The upstream file exceeds the configured cache file limit.")
			return
		}
		digest = hex.EncodeToString(hash.Sum(nil))
	}
	if expectedLFS != "" && digest != expectedLFS {
		if resume != nil {
			_ = s.resetDownloadResume(r.Context(), objectStore, resume, resume.ETag, resume.TotalSize, resume.ContentType)
		}
		s.finishDownload(r.Context(), downloadID, size, "integrity")
		writeProblem(w, r, http.StatusBadGateway, "Integrity check failed", "The downloaded file did not match the Hub's advertised SHA-256.")
		return
	}
	key := objectStore.Key(digest)
	contentType := meta.ContentType
	if body != nil && body.Header.Get("Content-Type") != "" {
		contentType = body.Header.Get("Content-Type")
	}
	if contentType == "" {
		contentType = mime.TypeByExtension(path.Ext(parsed.FilePath))
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	// Once the upstream body is complete, cache publication belongs to the
	// proxy even if the client closes its connection after receiving the body.
	publishCtx, cancelPublish := cachePublishContext(r.Context())
	defer cancelPublish()
	if resume == nil {
		if err := tmp.Sync(); err != nil {
			s.finishDownload(publishCtx, downloadID, size, "staging")
			writeStreamAwareProblem(w, r, streamed, 500, "Cache error", "Unable to finalize the staged file.")
			return
		}
	}
	// S3 calls must not run inside a database transaction: each transaction
	// reserves a pool connection, and slow uploads can exhaust the pool while
	// their progress writers need connections of their own.
	var blobExists bool
	if err = s.db.SQL.QueryRowContext(publishCtx, `SELECT EXISTS(SELECT 1 FROM blobs WHERE sha256=$1)`, digest).Scan(&blobExists); err != nil {
		s.finishDownload(publishCtx, downloadID, size, "metadata")
		writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to check blob metadata.")
		return
	}
	if !blobExists {
		if err := objectStore.UploadFile(publishCtx, tmp, key, contentType, size); err != nil {
			s.finishDownload(publishCtx, downloadID, size, "storage")
			writeStreamAwareProblem(w, r, streamed, 502, "Storage error", "The downloaded file could not be published to object storage.")
			return
		}
	}
	tx, err = s.db.SQL.BeginTx(publishCtx, nil)
	if err != nil {
		s.finishDownload(publishCtx, downloadID, size, "metadata")
		writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to open cache metadata transaction.")
		return
	}
	var blobID, existingKey string
	err = tx.QueryRowContext(publishCtx, `SELECT id,storage_key FROM blobs WHERE sha256=$1 FOR UPDATE`, digest).Scan(&blobID, &existingKey)
	if err == sql.ErrNoRows {
		err = tx.QueryRowContext(publishCtx, `INSERT INTO blobs(id,sha256,size_bytes,storage_key,content_type,verified_at) VALUES($1,$2,$3,$4,$5,now()) ON CONFLICT(sha256) DO UPDATE SET orphaned_at=NULL RETURNING id,storage_key`, db.NewID(), digest, size, key, contentType).Scan(&blobID, &existingKey)
		if err != nil {
			s.finishDownload(publishCtx, downloadID, size, "metadata")
			writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to publish cache metadata.")
			return
		}
	} else if err != nil {
		s.finishDownload(publishCtx, downloadID, size, "metadata")
		writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to read blob metadata.")
		return
	} else {
		key = existingKey
		if _, headErr := objectStore.Head(publishCtx, key); headErr != nil {
			if !storage.IsNotFound(headErr) {
				s.finishDownload(publishCtx, downloadID, size, "storage_check")
				writeStreamAwareProblem(w, r, streamed, http.StatusBadGateway, "Storage error", "The existing cached object could not be checked.")
				return
			}
			if err = objectStore.UploadFile(publishCtx, tmp, key, contentType, size); err != nil {
				s.finishDownload(publishCtx, downloadID, size, "storage")
				writeStreamAwareProblem(w, r, streamed, http.StatusBadGateway, "Storage error", "The missing cached object could not be restored.")
				return
			}
		}
		if _, err = tx.ExecContext(publishCtx, `UPDATE blobs SET orphaned_at=NULL WHERE id=$1`, blobID); err != nil {
			s.finishDownload(publishCtx, downloadID, size, "metadata")
			writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to restore cache metadata.")
			return
		}
	}
	meta.Size = size
	meta.ContentType = contentType
	if meta.ETag == "" && body != nil {
		meta.ETag = body.Header.Get("ETag")
	}
	if body != nil {
		if lm := body.Header.Get("Last-Modified"); lm != "" {
			if t, e := http.ParseTime(lm); e == nil {
				meta.LastMod = sql.NullTime{Time: t, Valid: true}
			}
		}
	}
	artifactID := db.NewID()
	if _, err = tx.ExecContext(publishCtx, `INSERT INTO artifacts(id,repository_id,commit_sha,path,blob_id,etag,last_modified,upstream_size) VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT(repository_id,commit_sha,path) DO UPDATE SET blob_id=EXCLUDED.blob_id,etag=EXCLUDED.etag,last_modified=EXCLUDED.last_modified,upstream_size=EXCLUDED.upstream_size,cached_at=now(),last_accessed_at=now()`, artifactID, repoPK, commit, parsed.FilePath, blobID, meta.ETag, nullTime(meta.LastMod), size); err != nil {
		s.finishDownload(publishCtx, downloadID, size, "metadata")
		writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to publish artifact metadata.")
		return
	}
	if err = tx.Commit(); err != nil {
		s.finishDownload(publishCtx, downloadID, size, "metadata")
		writeStreamAwareProblem(w, r, streamed, 500, "Cache metadata error", "Unable to commit artifact metadata.")
		return
	}
	if resume != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		s.finishDownloadResume(cleanupCtx, objectStore, resume)
		cancel()
	}
	s.finishDownload(publishCtx, downloadID, size, "")
	if streamed {
		return
	}
	cached, err = s.findArtifact(publishCtx, s.db.SQL, repoPK, commit, parsed.FilePath)
	if err != nil {
		writeProblem(w, r, 500, "Cache metadata error", "The artifact was stored but could not be read back.")
		return
	}
	s.serveOrAcknowledgePreload(w, r, cached, "MISS")
}

// serveStale answers from the cache when the Hub cannot confirm a branch or tag
// that was resolved before. It only applies when the last known commit has the
// file cached, and never to preload fetches, which exist to refresh the cache.
func (s *Server) serveStale(w http.ResponseWriter, r *http.Request, repoPK, commit string, parsed resolvePath) bool {
	if commit == "" || isPreloadFetch(r.Context()) {
		return false
	}
	cached, err := s.findArtifact(r.Context(), s.db.SQL, repoPK, commit, parsed.FilePath)
	if err != nil {
		return false
	}
	s.logger.Warn("serving cached file while the Hub is unavailable", "request_id", requestID(r.Context()))
	s.recordDownload(r.Context(), repoPK, commit, parsed.FilePath, "hit", cached.Size, "")
	s.serveOrAcknowledgePreload(w, r, cached, "STALE")
	return true
}

func parseResolvePath(escaped string) (resolvePath, error) {
	if len(escaped) > 8192 {
		return resolvePath{}, fmt.Errorf("request path exceeds the configured limit")
	}
	parts := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	repoStart := 0
	repoType := "models"
	if len(parts) > 0 && validRepoType(parts[0]) {
		repoType = parts[0]
		repoStart = 1
	}
	if len(parts) < repoStart+4 {
		return resolvePath{}, fmt.Errorf("expected /{repo}/resolve/{revision}/{file} or /{models|datasets|spaces}/{repo}/resolve/{revision}/{file}")
	}
	marker := -1
	for i := repoStart + 1; i+2 < len(parts); i++ {
		if parts[i] == "resolve" {
			marker = i
			break
		}
	}
	if marker < 0 {
		return resolvePath{}, fmt.Errorf("resolve route is incomplete")
	}
	repoParts := make([]string, marker-repoStart)
	for i := repoStart; i < marker; i++ {
		if strings.Contains(strings.ToLower(parts[i]), "%2f") || strings.Contains(strings.ToLower(parts[i]), "%5c") {
			return resolvePath{}, fmt.Errorf("encoded path separators are not allowed in repository identifiers")
		}
		decoded, err := url.PathUnescape(parts[i])
		if err != nil {
			return resolvePath{}, fmt.Errorf("invalid URL encoding")
		}
		repoParts[i-repoStart] = decoded
	}
	repoID := strings.Join(repoParts, "/")
	if !validRepoID(repoID) {
		return resolvePath{}, fmt.Errorf("repository identifier is invalid")
	}
	revisionRaw := parts[marker+1]
	revision, err := url.PathUnescape(revisionRaw)
	if err != nil || !validRevision(revision) {
		return resolvePath{}, fmt.Errorf("revision is invalid")
	}
	fileParts := make([]string, 0, len(parts)-marker-2)
	for _, raw := range parts[marker+2:] {
		if strings.Contains(strings.ToLower(raw), "%2f") || strings.Contains(strings.ToLower(raw), "%5c") {
			return resolvePath{}, fmt.Errorf("encoded separators are not allowed in file paths")
		}
		part, err := url.PathUnescape(raw)
		if err != nil {
			return resolvePath{}, fmt.Errorf("invalid URL encoding")
		}
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, "\\\x00") || hasControl(part) {
			return resolvePath{}, fmt.Errorf("file path contains an invalid segment")
		}
		fileParts = append(fileParts, part)
	}
	filePath := strings.Join(fileParts, "/")
	if filePath == "" || len(filePath) > 4096 {
		return resolvePath{}, fmt.Errorf("file path is empty or too long")
	}
	return resolvePath{RepoType: repoType, RepoID: repoID, Revision: revision, FilePath: filePath}, nil
}

func validRevision(revision string) bool {
	if revision == "" || len(revision) > 512 || strings.HasPrefix(revision, "/") || strings.HasSuffix(revision, "/") || strings.ContainsAny(revision, "\\\x00") || hasControl(revision) {
		return false
	}
	for _, p := range strings.Split(revision, "/") {
		if p == "" || p == "." || p == ".." {
			return false
		}
	}
	return true
}
func validCommit(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}

func lfsSHA256(value string) string {
	value = strings.TrimPrefix(strings.TrimSpace(value), "W/")
	value = strings.Trim(value, `"`)
	if len(value) != sha256.Size*2 {
		return ""
	}
	for _, r := range value {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F') {
			return ""
		}
	}
	return strings.ToLower(value)
}

func hasControl(value string) bool {
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
func escapeRepoPath(repo string) string {
	parts := strings.Split(repo, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
func escapeFilePath(file string) string {
	parts := strings.Split(file, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

type accessError struct {
	status        int
	title, detail string
}

func (e accessError) Error() string { return e.detail }
func (s *Server) repositoryAccess(ctx context.Context, p *Principal, authErr error, repoType, repoID string) (string, string, error) {
	var id, policy string
	err := s.db.SQL.QueryRowContext(ctx, `SELECT id,access_policy FROM repositories WHERE repo_type=$1 AND repo_id=$2 AND deleted_at IS NULL`, repoType, repoID).Scan(&id, &policy)
	if err == sql.ErrNoRows {
		return "", "", accessError{http.StatusForbidden, "Repository not registered", "This repository has no local access policy."}
	}
	if err != nil {
		return "", "", err
	}
	if p == nil {
		if authErr != errNoCredentials || !s.currentConfig().AnonymousPublicRead || policy != "public" {
			return "", "", accessError{http.StatusUnauthorized, "Authentication required", "Sign in to access this repository."}
		}
		return id, policy, nil
	}
	if p.Role == "admin" {
		return id, policy, nil
	}
	switch policy {
	case "public":
		return id, policy, nil
	case "admin_only":
		return "", "", accessError{http.StatusForbidden, "Forbidden", "This repository is restricted to administrators."}
	case "allowlist":
		var allowed bool
		err := s.db.SQL.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM repo_grants g WHERE g.repository_id=$1 AND g.permission IN ('read','write','admin') AND (
			(g.subject_type='user' AND g.subject_id=$2) OR (g.subject_type='group' AND EXISTS(SELECT 1 FROM group_members m WHERE m.group_id=g.subject_id AND m.user_id=$2))))`, id, p.ID).Scan(&allowed)
		if err != nil {
			return "", "", err
		}
		if !allowed {
			return "", "", accessError{http.StatusForbidden, "Forbidden", "You do not have read access to this repository."}
		}
		return id, policy, nil
	}
	return "", "", accessError{http.StatusForbidden, "Forbidden", "Repository access denied."}
}

func (s *Server) accessProblem(w http.ResponseWriter, r *http.Request, err error) {
	if a, ok := err.(accessError); ok {
		if a.status == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
		}
		writeProblem(w, r, a.status, a.title, a.detail)
		return
	}
	writeProblem(w, r, 500, "Authorization error", "Unable to evaluate repository access.")
}

// streamUncachedMiss serves requests that cannot wait for a cache fill, such as
// cold range requests and followers of an in-progress fill. LFS objects with
// an advertised digest stay on the verified cache-fill path.
func (s *Server) streamUncachedMiss(w http.ResponseWriter, r *http.Request, parsed resolvePath, meta artifact, commit string) {
	headers := make(http.Header)
	for _, name := range []string{"Range", "If-Range", "If-None-Match", "If-Modified-Since"} {
		if value := r.Header.Get(name); value != "" {
			headers.Set(name, value)
		}
	}
	pinned := resolvePath{RepoType: parsed.RepoType, RepoID: parsed.RepoID, Revision: commit, FilePath: parsed.FilePath}
	response, _, err := s.fetchHub(r.Context(), http.MethodGet, upstreamResolvePath(pinned), safeResolveQuery(r.URL.Query()), headers)
	if err != nil {
		s.hubFailure(w, r, err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		s.hubStatus(w, r, response.StatusCode, response.Header)
		return
	}
	s.setArtifactHeaders(w, meta, response.Header)
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("X-Repo-Commit", commit)
	for _, name := range []string{"Content-Range", "Content-Encoding"} {
		if value := response.Header.Get(name); value != "" {
			w.Header().Set(name, value)
		}
	}
	if response.ContentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(response.ContentLength, 10))
	}
	w.WriteHeader(response.StatusCode)
	if _, err := io.Copy(w, response.Body); err != nil {
		panic(http.ErrAbortHandler)
	}
}

func (s *Server) findArtifact(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, repoID, commit, file string) (artifact, error) {
	ctx, span := otel.Tracer("github.com/nicolas/hfproxy/internal/server").Start(ctx, "cache.lookup", trace.WithAttributes(attribute.String("cache.operation", "artifact_lookup")))
	defer span.End()
	started := time.Now()
	var a artifact
	err := q.QueryRowContext(ctx, `SELECT a.id,a.repository_id,a.commit_sha,a.path,a.blob_id,b.storage_key,b.sha256,b.size_bytes,a.etag,b.content_type,a.last_modified FROM artifacts a JOIN blobs b ON b.id=a.blob_id WHERE a.repository_id=$1 AND a.commit_sha=$2 AND a.path=$3`, repoID, commit, file).Scan(&a.ID, &a.Repository, &a.Commit, &a.Path, &a.BlobID, &a.StorageKey, &a.SHA256, &a.Size, &a.ETag, &a.ContentType, &a.LastMod)
	if s.metrics != nil {
		result := "hit"
		if isNoRows(err) {
			result = "miss"
		} else if err != nil {
			result = "error"
			span.SetStatus(codes.Error, "cache lookup failed")
		}
		span.SetAttributes(attribute.String("cache.result", result))
		s.metrics.cacheDecision(ctx, result)
		s.metrics.observe("hfproxy_cache_lookup_duration_seconds", nil, time.Since(started).Seconds())
	}
	return a, err
}
func isNoRows(err error) bool { return err == sql.ErrNoRows }
func nullTime(t sql.NullTime) any {
	if !t.Valid {
		return nil
	}
	return t.Time
}

func (s *Server) hubFailure(w http.ResponseWriter, r *http.Request, err error) {
	s.logger.Warn("upstream request failed", "request_id", requestID(r.Context()), "class", fmt.Sprintf("%T", err))
	writeProblem(w, r, 502, "Hugging Face upstream unavailable", "The upstream request failed or returned invalid metadata.")
}
func (s *Server) hubStatus(w http.ResponseWriter, r *http.Request, status int, headers http.Header) {
	w.Header().Del("Content-Length")
	if status < 400 || status > 599 {
		status = 502
	}
	if retry := headers.Get("Retry-After"); retry != "" {
		if seconds, err := strconv.Atoi(retry); err == nil && seconds >= 0 && seconds <= 3600 {
			w.Header().Set("Retry-After", retry)
		}
	}
	for _, key := range []string{"X-Error-Code", "X-Error-Message", "X-Repo-Commit"} {
		if value := safeUpstreamHeader(headers.Get(key), 1024); value != "" {
			w.Header().Set(key, value)
		}
	}
	titles := map[int]string{400: "Bad request", 401: "Upstream authentication required", 403: "Upstream access denied", 404: "File or repository not found", 410: "Resource gone", 429: "Upstream rate limit", 500: "Upstream error", 502: "Upstream unavailable", 503: "Upstream unavailable", 504: "Upstream timeout"}
	title, ok := titles[status]
	if !ok {
		title = "Upstream error"
	}
	writeProblem(w, r, status, title, "Hugging Face returned an error for this resource.")
}

func safeUpstreamHeader(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func (s *Server) recordDownload(ctx context.Context, repoID, commit, file, status string, bytes int64, errorClass string) {
	caller := callerOf(ctx)
	_, err := s.db.SQL.ExecContext(ctx, "INSERT INTO downloads(id,repository_id,commit_sha,path,status,completed_at,bytes,error_class,user_id,token_id,client_ip) VALUES($1,$2,$3,$4,$5,now(),$6,$7,NULLIF($8,''),NULLIF($9,''),NULLIF($10,''))", db.NewID(), repoID, commit, file, status, bytes, errorClass, caller.UserID, caller.TokenID, caller.ClientIP)
	if err != nil {
		s.logger.Warn("download event could not be recorded")
	}
}

// callerOf returns who is making the request in ctx, or an empty value for
// background work such as preloads.
func callerOf(ctx context.Context) requestInfo {
	if info := requestInfoFrom(ctx); info != nil {
		return *info
	}
	return requestInfo{}
}

func (s *Server) finishDownload(ctx context.Context, id string, bytes int64, errorClass string) {
	status := "miss"
	if errorClass != "" {
		status = "failed"
	}
	_, err := s.db.SQL.ExecContext(ctx, "UPDATE downloads SET status=$2,completed_at=now(),bytes=$3,error_class=$4 WHERE id=$1", id, status, bytes, errorClass)
	if err != nil {
		s.logger.Warn("download completion could not be recorded")
	}
}

func safeResolveQuery(values url.Values) url.Values {
	result := url.Values{}
	for _, key := range []string{"download", "raw"} {
		for _, value := range values[key] {
			if len(value) <= 16 {
				result.Add(key, value)
			}
		}
	}
	return result
}

func (s *Server) resolveCommit(ctx context.Context, parsed resolvePath) (string, error) {
	requestPath := "/api/" + parsed.RepoType + "/" + escapeRepoPath(parsed.RepoID) + "/revision/" + url.PathEscape(parsed.Revision)
	response, _, err := s.fetchHub(ctx, http.MethodGet, requestPath, nil, nil)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", fmt.Errorf("Hub revision lookup failed with status %d", response.StatusCode)
	}
	var payload struct {
		SHA       string
		Commit    string
		CommitSHA string
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&payload); err != nil {
		return "", fmt.Errorf("Hub revision response was invalid")
	}
	for _, value := range []string{payload.SHA, payload.Commit, payload.CommitSHA} {
		if validCommit(value) {
			return value, nil
		}
	}
	return "", fmt.Errorf("Hub did not return an immutable commit reference")
}
