package server

import (
	"fmt"
	"io"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/storage"
)

func (s *Server) setArtifactHeaders(w http.ResponseWriter, a artifact, upstream http.Header) {
	if a.ContentType != "" {
		w.Header().Set("Content-Type", a.ContentType)
	}
	if a.ETag != "" {
		w.Header().Set("ETag", a.ETag)
	}
	if a.LastMod.Valid {
		w.Header().Set("Last-Modified", a.LastMod.Time.UTC().Format(http.TimeFormat))
	}
	// Repository files are untrusted and share an origin with the admin
	// console: sandbox them so a cached HTML/SVG file cannot run script there.
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	w.Header().Set("X-Repo-Commit", a.Commit)
	w.Header().Set("X-Cache", "HIT")
	for _, key := range []string{"Content-Disposition", "X-Linked-ETag", "X-Linked-Size", "X-Xet-Hash"} {
		if value := upstream.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if activeContentType(a.ContentType) || activeContentType(mime.TypeByExtension(path.Ext(a.Path))) {
		// Never let a browser render a repository's HTML, SVG, XML or script
		// inline from the proxy's origin.
		w.Header().Set("Content-Disposition", attachmentDisposition(a.Path))
	}
	if w.Header().Get("X-Linked-ETag") == "" {
		if digest := lfsSHA256(a.ETag); digest != "" {
			w.Header().Set("X-Linked-ETag", `"`+digest+`"`)
		}
	}
	if w.Header().Get("X-Linked-Size") == "" && w.Header().Get("X-Linked-ETag") != "" {
		w.Header().Set("X-Linked-Size", strconv.FormatInt(a.Size, 10))
	}
}

func activeContentType(contentType string) bool {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = strings.ToLower(strings.TrimSpace(contentType))
	}
	switch mediaType {
	case "text/html", "application/xhtml+xml", "image/svg+xml", "text/xml", "application/xml",
		"text/javascript", "application/javascript", "application/x-javascript", "text/xsl", "text/css":
		return true
	}
	return false
}

func attachmentDisposition(filePath string) string {
	name := path.Base(filePath)
	if name == "." || name == "/" || name == "" {
		return "attachment"
	}
	if disposition := mime.FormatMediaType("attachment", map[string]string{"filename": name}); disposition != "" {
		return disposition
	}
	return "attachment"
}

// writeArtifact answers with a cached file. served, when not nil, is called once
// the file is certain to be answered (before the response starts). It reports
// true, without having written anything, when the catalog lists the file but its object is gone from
// the bucket, so that the caller can repair the entry instead of failing.
func (s *Server) writeArtifact(w http.ResponseWriter, r *http.Request, a artifact, cacheResult string, served func()) (objectMissing bool) {
	setHeaders := func() {
		if served != nil {
			served()
			served = nil
		}
		s.setArtifactHeaders(w, a, nil)
		w.Header().Set("X-Cache", cacheResult)
	}
	if ifNoneMatch(r.Header.Get("If-None-Match"), a.ETag) || (r.Header.Get("If-None-Match") == "" && a.LastMod.Valid && notModifiedSince(r.Header.Get("If-Modified-Since"), a.LastMod.Time)) {
		setHeaders()
		w.WriteHeader(http.StatusNotModified)
		return false
	}
	start, end, partial, rangeOK := parseByteRange(r.Header.Get("Range"), a.Size)
	if r.Header.Get("If-Range") != "" && partial {
		if !ifRangeMatches(r.Header.Get("If-Range"), a) {
			partial = false
		}
	}
	if r.Header.Get("Range") != "" && !rangeOK {
		setHeaders()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", a.Size))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return false
	}
	byteRange := ""
	status := http.StatusOK
	if partial {
		byteRange = fmt.Sprintf("bytes=%d-%d", start, end)
		status = http.StatusPartialContent
	}
	if r.Method == http.MethodHead {
		setHeaders()
		if partial {
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, a.Size))
			w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
		} else {
			w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
		}
		w.WriteHeader(status)
		return false
	}
	out, err := s.currentStorage().Get(r.Context(), a.StorageKey, byteRange)
	if err != nil {
		// A missing object (as opposed to an unreachable store) means the catalog
		// and the bucket disagree for this file; the caller repairs the entry.
		missing := storage.IsNotFound(err)
		s.logger.Error("cache object read failed", "request_id", requestID(r.Context()), "sha256", a.SHA256, "object_missing", missing, "error", safeLogValue(err.Error(), 200))
		if missing {
			return true
		}
		writeProblem(w, r, 502, "Storage error", "The cached artifact is temporarily unavailable.")
		return false
	}
	defer out.Body.Close()
	setHeaders()
	if partial {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, a.Size))
		w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	} else {
		w.Header().Set("Content-Length", strconv.FormatInt(a.Size, 10))
	}
	w.WriteHeader(status)
	n, _ := io.Copy(w, out.Body)
	if s.metrics != nil {
		s.metrics.add("hfproxy_cache_bytes_served_total", []string{"source=cache"}, float64(n))
	}
	return false
}

func parseByteRange(value string, size int64) (int64, int64, bool, bool) {
	if value == "" {
		return 0, 0, false, true
	}
	if !strings.HasPrefix(value, "bytes=") {
		// Unsupported range units are ignored, as allowed by HTTP semantics.
		return 0, 0, false, true
	}
	if strings.Contains(value, ",") {
		// The proxy does not build multipart/byteranges responses. HTTP lets a
		// server ignore a Range header it does not want to honour, so a request
		// for several ranges gets the whole file with a 200.
		return 0, 0, false, true
	}
	parts := strings.SplitN(strings.TrimPrefix(value, "bytes="), "-", 2)
	if len(parts) != 2 || size <= 0 {
		return 0, 0, false, false
	}
	var start, end int64
	var err error
	if parts[0] == "" {
		suffix, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || suffix <= 0 {
			return 0, 0, false, false
		}
		if suffix > size {
			suffix = size
		}
		start = size - suffix
		end = size - 1
	} else {
		start, err = strconv.ParseInt(parts[0], 10, 64)
		if err != nil || start < 0 || start >= size {
			return 0, 0, false, false
		}
		if parts[1] == "" {
			end = size - 1
		} else {
			end, err = strconv.ParseInt(parts[1], 10, 64)
			if err != nil || end < start {
				return 0, 0, false, false
			}
			if end >= size {
				end = size - 1
			}
		}
	}
	return start, end, true, true
}
func ifNoneMatch(header, etag string) bool {
	if header == "" || etag == "" {
		return false
	}
	if header == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		candidate = strings.TrimSpace(candidate)
		candidate = strings.TrimPrefix(candidate, "W/")
		expected := strings.TrimPrefix(etag, "W/")
		if candidate == expected {
			return true
		}
	}
	return false
}
func notModifiedSince(header string, last time.Time) bool {
	if header == "" {
		return false
	}
	t, err := http.ParseTime(header)
	return err == nil && !last.Truncate(time.Second).After(t)
}
func ifRangeMatches(value string, a artifact) bool {
	if strings.HasPrefix(value, "W/") {
		return false
	}
	if strings.HasPrefix(value, "\"") {
		return value == a.ETag && !strings.HasPrefix(a.ETag, "W/")
	}
	if a.LastMod.Valid {
		if t, err := http.ParseTime(value); err == nil {
			return !a.LastMod.Time.Truncate(time.Second).After(t)
		}
	}
	return false
}
