package server

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

func (s *Server) hubAPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w)
		return
	}
	if r.URL.Path == "/api/health" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	if r.URL.Path == "/api/whoami-v2" {
		p, err := s.authenticate(r)
		if err != nil {
			w.Header().Set("WWW-Authenticate", `Bearer realm="hfproxy"`)
			writeProblem(w, r, http.StatusUnauthorized, "Authentication required", "This endpoint reports the local hfproxy identity.")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"name": p.Username, "fullname": p.DisplayName, "email": p.Email, "auth": map[string]any{"type": "hfproxy", "accessToken": map[string]string{"displayName": "hfproxy"}}})
		return
	}
	parsed, upstreamPath, query, err := parseHubAPIPath(r.URL.EscapedPath(), r.URL.Query())
	if err != nil {
		writeProblem(w, r, http.StatusNotFound, "Unsupported Hub route", err.Error())
		return
	}
	p, authErr := s.authenticate(r)
	_, _, err = s.repositoryAccess(r.Context(), p, authErr, parsed.RepoType, parsed.RepoID)
	if err != nil {
		s.accessProblem(w, r, err)
		return
	}
	if r.Method == http.MethodGet {
		if cache := s.metadataCache(); cache != nil {
			s.serveMetadataCached(w, r, cache, upstreamPath, query)
			return
		}
	}
	resp, _, err := s.fetchHub(r.Context(), r.Method, upstreamPath, query, nil)
	if err != nil {
		s.hubFailure(w, r, err)
		return
	}
	defer resp.Body.Close()
	copyHubHeaders(w, resp.Header, r, s.currentConfig().PublicURL)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		s.hubStatus(w, r, resp.StatusCode, resp.Header)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(resp.StatusCode)
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (2<<20)+1))
	if readErr != nil || len(body) > 2<<20 {
		writeProblem(w, r, http.StatusBadGateway, "Upstream response too large", "Hub metadata responses are limited to 2 MiB.")
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

const maxMetadataBody = 2 << 20

// serveMetadataCached answers a Hub metadata GET from memory when it can.
//
// The caller has already checked that the client may read the repository, so
// the cache never widens access. A fresh entry is served as is; an expired one
// is revalidated with its ETag; and when the Hub is failing, an expired entry
// younger than APICacheMaxStale is served marked X-Cache: STALE rather than
// returning an error. Concurrent requests for the same key share one Hub fetch.
func (s *Server) serveMetadataCached(w http.ResponseWriter, r *http.Request, cache *apiCache, upstreamPath string, query url.Values) {
	cfg := s.currentConfig()
	// The Hub URL is part of the key so changing it in the console cannot serve
	// answers that came from the previous one.
	key := cfg.HFBaseURL + "|" + upstreamPath + "?" + query.Encode()
	count := func(result string) {
		if s.metrics != nil {
			s.metrics.add("hfproxy_api_cache_requests_total", []string{"result=" + result}, 1)
		}
	}
	serve := func(entry apiCacheEntry, label string) {
		count(strings.ToLower(label))
		copyHubHeaders(w, entry.header, r, cfg.PublicURL)
		w.Header().Set("X-Cache", label)
		w.Header().Set("Content-Length", fmt.Sprint(len(entry.body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(entry.body)
	}
	fresh := func(entry apiCacheEntry) bool { return time.Since(entry.storedAt) <= cfg.CacheRefTTL }

	entry, cached := cache.get(key)
	if cached && fresh(entry) {
		serve(entry, "HIT")
		return
	}
	usable := cached && cfg.APICacheMaxStale > 0 && time.Since(entry.storedAt) <= cfg.APICacheMaxStale

	flight, leader := s.joinAPIFlight(key)
	if leader {
		defer s.finishAPIFlight(key, flight)
	} else {
		select {
		case <-flight.done:
		case <-r.Context().Done():
			return
		}
		if shared, ok := cache.get(key); ok && fresh(shared) {
			serve(shared, "HIT")
			return
		}
	}

	var conditional http.Header
	if usable && entry.etag != "" {
		conditional = http.Header{"If-None-Match": {entry.etag}}
	}
	resp, _, err := s.fetchHub(r.Context(), http.MethodGet, upstreamPath, query, conditional)
	if err != nil {
		if usable {
			serve(entry, "STALE")
			return
		}
		count("error")
		s.hubFailure(w, r, err)
		return
	}
	defer resp.Body.Close()
	if usable && (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) {
		serve(entry, "STALE")
		return
	}
	if resp.StatusCode == http.StatusNotModified && cached {
		cache.refresh(key, time.Now())
		serve(entry, "REVALIDATED")
		return
	}
	copyHubHeaders(w, resp.Header, r, cfg.PublicURL)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			cache.delete(key) // the repository or revision is gone, or access was withdrawn
		}
		count("bypass")
		s.hubStatus(w, r, resp.StatusCode, resp.Header)
		return
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxMetadataBody+1))
	if readErr != nil || len(body) > maxMetadataBody {
		writeProblem(w, r, http.StatusBadGateway, "Upstream response too large", "Hub metadata responses are limited to 2 MiB.")
		return
	}
	if resp.StatusCode == http.StatusOK {
		header := resp.Header.Clone()
		for _, name := range []string{"X-Request-Id", "Set-Cookie", "Content-Length"} {
			header.Del(name)
		}
		fresh := apiCacheEntry{key: key, header: header, body: body, etag: resp.Header.Get("ETag"), storedAt: time.Now()}
		cache.put(fresh)
		serve(fresh, "MISS")
		return
	}
	count("bypass")
	w.Header().Set("Content-Length", fmt.Sprint(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

type hubMetadataPath struct{ RepoType, RepoID string }

func parseHubAPIPath(escaped string, incoming url.Values) (hubMetadataPath, string, url.Values, error) {
	parts := strings.Split(strings.TrimPrefix(escaped, "/"), "/")
	if len(parts) < 3 || parts[0] != "api" || !validRepoType(parts[1]) {
		return hubMetadataPath{}, "", nil, fmt.Errorf("only repository metadata, revision, tree, and commit read routes are supported")
	}
	decoded := make([]string, len(parts))
	for i, part := range parts {
		value, err := url.PathUnescape(part)
		if err != nil || value == "." || value == ".." || hasControl(value) {
			return hubMetadataPath{}, "", nil, fmt.Errorf("invalid route path")
		}
		decoded[i] = value
	}
	repoEnd := len(decoded)
	if len(decoded) > 3 && (decoded[3] == "revision" || decoded[3] == "tree" || decoded[3] == "commits") {
		repoEnd = 3
	} else if len(decoded) > 4 && (decoded[4] == "revision" || decoded[4] == "tree" || decoded[4] == "commits") {
		repoEnd = 4
	}
	repoID := strings.Join(decoded[2:repoEnd], "/")
	for _, part := range decoded[2:repoEnd] {
		if strings.ContainsAny(part, "/\\") {
			return hubMetadataPath{}, "", nil, fmt.Errorf("encoded separators are not allowed in repository identifiers")
		}
	}
	if !validRepoID(repoID) {
		return hubMetadataPath{}, "", nil, fmt.Errorf("repository identifier is invalid")
	}
	suffix := decoded[repoEnd:]
	if len(suffix) > 0 {
		if suffix[0] == "revision" || suffix[0] == "commits" {
			if len(suffix) != 2 || !validRevision(suffix[1]) {
				return hubMetadataPath{}, "", nil, fmt.Errorf("revision or commit route is incomplete")
			}
		} else if suffix[0] == "tree" {
			if len(suffix) < 2 || !validRevision(suffix[1]) {
				return hubMetadataPath{}, "", nil, fmt.Errorf("tree route is incomplete")
			}
			if len(suffix) > 2 {
				for _, part := range suffix[2:] {
					for _, segment := range strings.Split(part, "/") {
						if segment == "" || segment == "." || segment == ".." || strings.ContainsAny(segment, "\\\x00") || hasControl(segment) {
							return hubMetadataPath{}, "", nil, fmt.Errorf("tree path is invalid")
						}
					}
				}
			}
		} else {
			return hubMetadataPath{}, "", nil, fmt.Errorf("this Hub API route is not supported")
		}
	}
	var pathParts []string
	for _, part := range decoded {
		pathParts = append(pathParts, url.PathEscape(part))
	}
	upstreamPath := "/" + strings.Join(pathParts, "/")
	query := url.Values{}
	allowed := map[string]bool{"recursive": true, "expand": true, "blobs": true, "files_metadata": true, "limit": true, "cursor": true, "sort": true, "direction": true, "full": true, "securityStatus": true, "include_prs": true}
	for key, values := range incoming {
		if allowed[key] {
			for _, value := range values {
				if len(value) <= 512 {
					query.Add(key, value)
				}
			}
		}
	}
	return hubMetadataPath{RepoType: decoded[1], RepoID: repoID}, upstreamPath, query, nil
}

func copyHubHeaders(w http.ResponseWriter, headers http.Header, r *http.Request, publicURL string) {
	for _, key := range []string{"Content-Type", "Content-Length", "ETag", "Last-Modified", "Cache-Control", "X-Repo-Commit", "X-Request-Id", "X-Total-Count"} {
		if value := headers.Get(key); value != "" {
			w.Header().Set(key, value)
		}
	}
	if link := rewritePaginationLink(headers.Get("Link"), r, publicURL); link != "" {
		w.Header().Set("Link", link)
	}
}

func rewritePaginationLink(value string, r *http.Request, publicURL string) string {
	if value == "" {
		return ""
	}
	parts := strings.Split(value, ",")
	kept := make([]string, 0, len(parts))
	for _, part := range parts {
		start, end := strings.IndexByte(part, '<'), strings.IndexByte(part, '>')
		if start < 0 || end <= start {
			continue
		}
		target := part[start+1 : end]
		u, err := url.Parse(target)
		if err != nil {
			continue
		}
		if u.IsAbs() {
			if !strings.HasPrefix(u.Path, "/api/") {
				continue
			}
			if base, err := url.Parse(publicURL); err == nil && base.IsAbs() && base.Host != "" {
				u.Scheme = base.Scheme
				u.Host = base.Host
				u.Path = strings.TrimRight(base.Path, "/") + u.Path
			} else {
				scheme := "http"
				if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
					scheme = "https"
				}
				u.Scheme, u.Host = scheme, r.Host
			}
			part = part[:start+1] + u.String() + part[end:]
		}
		kept = append(kept, strings.TrimSpace(part))
	}
	return strings.Join(kept, ", ")
}
