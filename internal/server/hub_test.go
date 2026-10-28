package server

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/config"
)

func TestCachePublishContextOutlivesClientContext(t *testing.T) {
	clientCtx, cancelClient := context.WithCancel(context.Background())
	publishCtx, cancelPublish := cachePublishContext(clientCtx)
	defer cancelPublish()
	cancelClient()
	select {
	case <-publishCtx.Done():
		t.Fatalf("cache publish context canceled with client: %v", publishCtx.Err())
	default:
	}
}

func TestParseResolvePath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    resolvePath
		wantErr bool
	}{
		{
			name: "organization repo and nested file",
			path: "/models/acme/encoder/resolve/main/config/model.json",
			want: resolvePath{RepoType: "models", RepoID: "acme/encoder", Revision: "main", FilePath: "config/model.json"},
		},
		{
			name: "encoded branch separator",
			path: "/datasets/acme/corpus/resolve/feature%2Fv2/data/train.parquet",
			want: resolvePath{RepoType: "datasets", RepoID: "acme/corpus", Revision: "feature/v2", FilePath: "data/train.parquet"},
		},
		{name: "unsupported repository type", path: "/packages/acme/model/resolve/main/config.json", wantErr: true},
		{name: "encoded file separator", path: "/models/acme/model/resolve/main/a%2Fb", wantErr: true},
		{name: "dot dot segment", path: "/models/acme/model/resolve/main/%2e%2e/secret", wantErr: true},
		{name: "control character", path: "/models/acme/model/resolve/main/a%00b", wantErr: true},
		{name: "missing file", path: "/models/acme/model/resolve/main", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseResolvePath(tt.path)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseResolvePath() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("parseResolvePath() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestResolveRouteMatrixSupportsAllRepositoryTypes(t *testing.T) {
	for _, repoType := range []string{"models", "datasets", "spaces"} {
		t.Run(repoType, func(t *testing.T) {
			path := "/" + repoType + "/org/repo/resolve/main/path/to/file.bin"
			got, err := parseResolvePath(path)
			if err != nil {
				t.Fatalf("parseResolvePath(%q) error = %v", path, err)
			}
			want := resolvePath{RepoType: repoType, RepoID: "org/repo", Revision: "main", FilePath: "path/to/file.bin"}
			if got != want {
				t.Fatalf("parseResolvePath(%q) = %#v, want %#v", path, got, want)
			}
			wantUpstream := "/org/repo/resolve/main/path/to/file.bin"
			if repoType != "models" {
				wantUpstream = "/" + repoType + wantUpstream
			}
			if upstream := upstreamResolvePath(got); upstream != wantUpstream {
				t.Fatalf("upstreamResolvePath(%#v) = %q", got, upstream)
			}
		})
	}

	untagged, err := parseResolvePath("/org/repo/resolve/main/config.json")
	if err != nil {
		t.Fatalf("untyped model route error = %v", err)
	}
	if untagged.RepoType != "models" {
		t.Fatalf("untyped model route type = %q, want models", untagged.RepoType)
	}
}

func TestHubMetadataRouteMatrixSupportsAllOperations(t *testing.T) {
	for _, operation := range []string{"", "revision/main", "tree/main", "tree/main/config", "commits/main"} {
		for _, repoType := range []string{"models", "datasets", "spaces"} {
			t.Run(repoType+"/"+operation, func(t *testing.T) {
				path := "/api/" + repoType + "/org/repo"
				if operation != "" {
					path += "/" + operation
				}
				got, upstream, _, err := parseHubAPIPath(path, nil)
				if err != nil {
					t.Fatalf("parseHubAPIPath(%q) error = %v", path, err)
				}
				if got.RepoType != repoType || got.RepoID != "org/repo" || upstream != path {
					t.Fatalf("parseHubAPIPath(%q) = %#v, %q", path, got, upstream)
				}
			})
		}
	}
}

func TestFetchHubPrivateTokenIsNotSentToCDN(t *testing.T) {
	var requests []string
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		requests = append(requests, r.URL.Host+" Authorization="+r.Header.Get("Authorization"))
		switch r.URL.Host {
		case "huggingface.co":
			return &http.Response{
				StatusCode: http.StatusFound,
				Header: http.Header{
					"Location":      []string{"https://cdn-lfs.hf.co/model/config.json"},
					"X-Linked-Etag": []string{`"private-lfs-digest"`},
					"X-Linked-Size": []string{"42"},
				},
				Body: io.NopCloser(strings.NewReader("")),
			}, nil
		case "cdn-lfs.hf.co":
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("ok")),
			}, nil
		default:
			return nil, fmt.Errorf("unexpected upstream host %q", r.URL.Host)
		}
	})}
	s := &Server{}
	s.runtime.Store(&runtimeState{
		config: config.Config{
			HFBaseURL:      "https://huggingface.co",
			HFToken:        "private-read-token",
			HFAllowedHosts: []string{"cdn-lfs.hf.co"},
			HFTimeout:      time.Second,
		},
		httpClient: client,
	})

	response, first, err := s.fetchHub(context.Background(), http.MethodHead, "/models/org/private/resolve/main/config.json", nil, nil)
	if err != nil {
		t.Fatalf("fetchHub() error = %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("final status = %d, want 200", response.StatusCode)
	}
	if first.Get("X-Linked-Etag") != `"private-lfs-digest"` {
		t.Fatalf("first response headers lost LFS identity: %v", first)
	}
	want := []string{
		"huggingface.co Authorization=Bearer private-read-token",
		"cdn-lfs.hf.co Authorization=",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("upstream requests = %v, want %v", requests, want)
	}
}

func TestAllowedRedirectHostRejectsUntrustedDestinations(t *testing.T) {
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{
		HFBaseURL:      "https://huggingface.co",
		HFAllowedHosts: []string{"huggingface.co", "*.hf.co"},
	}})
	for _, tt := range []struct {
		host string
		want bool
	}{
		{host: "huggingface.co", want: true},
		{host: "cdn-lfs.hf.co", want: true},
		{host: "cdn-lfs.hf.co.", want: true},
		{host: "hf.co.evil.example", want: false},
		{host: "127.0.0.1", want: false},
		{host: "169.254.169.254", want: false},
	} {
		t.Run(tt.host, func(t *testing.T) {
			if got := s.allowedRedirectHost(tt.host); got != tt.want {
				t.Fatalf("allowedRedirectHost(%q) = %v, want %v", tt.host, got, tt.want)
			}
		})
	}
}

func TestFetchHubRedirectPolicyPreservesSignedURLAndRejectsUnsafeTargets(t *testing.T) {
	unsafeLocations := []string{
		"http://attacker.example/model",
		"https://attacker.example/model",
		"https://attacker.example:8443/model",
		"https://user:pass@cdn-lfs.hf.co/model",
		"https://127.0.0.1/model",
	}
	for _, location := range unsafeLocations {
		t.Run(location, func(t *testing.T) {
			client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})}
			s := &Server{}
			s.runtime.Store(&runtimeState{config: config.Config{
				HFBaseURL:      "https://huggingface.co",
				HFAllowedHosts: []string{"huggingface.co", "*.hf.co"},
				HFTimeout:      time.Second,
			}, httpClient: client})
			if _, _, err := s.fetchHub(context.Background(), http.MethodHead, "/org/repo/resolve/main/file.bin", nil, nil); err == nil {
				t.Fatalf("fetchHub accepted unsafe redirect %q", location)
			}
		})
	}

	var seenQuery string
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "huggingface.co" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://cdn-lfs.hf.co/object?X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=60&X-Amz-Signature=abc"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		seenQuery = r.URL.RawQuery
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{
		HFBaseURL:      "https://huggingface.co",
		HFToken:        "hub-token",
		HFAllowedHosts: []string{"huggingface.co", "*.hf.co"},
		HFTimeout:      time.Second,
	}, httpClient: client})
	response, _, err := s.fetchHub(context.Background(), http.MethodGet, "/org/repo/resolve/main/file.bin", nil, nil)
	if err != nil {
		t.Fatalf("fetchHub signed redirect: %v", err)
	}
	response.Body.Close()
	if seenQuery != "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Expires=60&X-Amz-Signature=abc" {
		t.Fatalf("signed redirect query = %q", seenQuery)
	}
}

func TestFetchHubRedirectLimitIsEnforced(t *testing.T) {
	requests := 0
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		location := fmt.Sprintf("https://cdn-%d.hf.co/object", requests)
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{location}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	s := &Server{}
	s.runtime.Store(&runtimeState{config: config.Config{
		HFBaseURL:      "https://huggingface.co",
		HFAllowedHosts: []string{"huggingface.co", "*.hf.co"},
		HFTimeout:      time.Second,
	}, httpClient: client})
	if _, _, err := s.fetchHub(context.Background(), http.MethodGet, "/org/repo/resolve/main/file.bin", nil, nil); err == nil {
		t.Fatal("fetchHub accepted more than the configured redirect limit")
	}
	if requests != 6 {
		t.Fatalf("redirect requests = %d, want 6 (five followed redirects plus the rejected sixth)", requests)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCacheHitHeadersPreserveLFSIdentity(t *testing.T) {
	digest := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	const size = int64(1234)
	rec := httptest.NewRecorder()
	(&Server{}).setArtifactHeaders(rec, artifact{Commit: "0123456789abcdef0123456789abcdef01234567", ETag: `"` + digest + `"`, Size: size}, nil)
	if got := rec.Header().Get("ETag"); got != `"`+digest+`"` {
		t.Fatalf("ETag = %q, want LFS digest", got)
	}
	if got := rec.Header().Get("X-Linked-Etag"); got != `"`+digest+`"` {
		t.Fatalf("X-Linked-Etag = %q, want LFS digest", got)
	}
	if got := rec.Header().Get("X-Linked-Size"); got != "1234" {
		t.Fatalf("X-Linked-Size = %q, want 1234", got)
	}
}

func TestParseByteRange(t *testing.T) {
	tests := []struct {
		name           string
		value          string
		start, end     int64
		partial, valid bool
	}{
		{name: "no range", value: "", valid: true},
		{name: "single byte", value: "bytes=0-0", start: 0, end: 0, partial: true, valid: true},
		{name: "open end", value: "bytes=5-", start: 5, end: 9, partial: true, valid: true},
		{name: "suffix", value: "bytes=-3", start: 7, end: 9, partial: true, valid: true},
		{name: "clamp end", value: "bytes=8-99", start: 8, end: 9, partial: true, valid: true},
		{name: "multiple ranges ignored", value: "bytes=0-1,4-5", valid: true},
		{name: "start beyond object", value: "bytes=10-", valid: false},
		{name: "reversed range", value: "bytes=8-3", valid: false},
		{name: "zero suffix", value: "bytes=-0", valid: false},
		{name: "unknown unit ignored", value: "items=0-1", valid: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			start, end, partial, valid := parseByteRange(tt.value, 10)
			if start != tt.start || end != tt.end || partial != tt.partial || valid != tt.valid {
				t.Errorf("parseByteRange(%q, 10) = (%d, %d, %v, %v), want (%d, %d, %v, %v)", tt.value, start, end, partial, valid, tt.start, tt.end, tt.partial, tt.valid)
			}
		})
	}
}

func TestParseHubAPIPath(t *testing.T) {
	query := url.Values{
		"recursive": {"true"},
		"limit":     {"50"},
		"cursor":    {"next"},
		"token":     {"must-not-forward"},
	}
	gotRepo, gotPath, gotQuery, err := parseHubAPIPath("/api/models/acme/encoder/tree/main/config", query)
	if err != nil {
		t.Fatalf("parseHubAPIPath() error = %v", err)
	}
	if gotRepo != (hubMetadataPath{RepoType: "models", RepoID: "acme/encoder"}) {
		t.Errorf("repo = %#v", gotRepo)
	}
	if gotPath != "/api/models/acme/encoder/tree/main/config" {
		t.Errorf("upstream path = %q", gotPath)
	}
	if gotQuery.Get("recursive") != "true" || gotQuery.Get("limit") != "50" || gotQuery.Get("cursor") != "next" {
		t.Errorf("allowed query parameters were not preserved: %v", gotQuery)
	}
	if gotQuery.Has("token") {
		t.Errorf("sensitive query parameter was forwarded: %v", gotQuery)
	}

	for _, path := range []string{
		"/api/models/acme/encoder/tree/main/%2e%2e/secret",
		"/api/models/acme%2fencoder",
		"/api/models/acme/encoder/revision",
		"/api/models/acme/encoder/unknown/main",
	} {
		t.Run(path, func(t *testing.T) {
			if _, _, _, err := parseHubAPIPath(path, nil); err == nil {
				t.Errorf("parseHubAPIPath(%q) unexpectedly succeeded", path)
			}
		})
	}
}

func TestIfNoneMatch(t *testing.T) {
	tests := []struct {
		header string
		etag   string
		want   bool
	}{
		{header: `"abc"`, etag: `"abc"`, want: true},
		{header: `W/"abc"`, etag: `"abc"`, want: true},
		{header: `"other", W/"abc"`, etag: `"abc"`, want: true},
		{header: "*", etag: `"abc"`, want: true},
		{header: `"other"`, etag: `"abc"`, want: false},
		{header: `"abc"`, etag: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.header+"/"+tt.etag, func(t *testing.T) {
			if got := ifNoneMatch(tt.header, tt.etag); got != tt.want {
				t.Errorf("ifNoneMatch(%q, %q) = %v, want %v", tt.header, tt.etag, got, tt.want)
			}
		})
	}
}

func TestIfRangeRequiresStrongETag(t *testing.T) {
	a := artifact{ETag: `"abc"`, LastMod: sql.NullTime{}}
	if ifRangeMatches(`W/"abc"`, a) {
		t.Fatal("weak If-Range ETag must not match")
	}
	if !ifRangeMatches(`"abc"`, a) {
		t.Fatal("strong If-Range ETag should match")
	}
	if ifRangeMatches(`"abc"`, artifact{ETag: `W/"abc"`, LastMod: sql.NullTime{}}) {
		t.Fatal("strong If-Range must not match a weak artifact ETag")
	}
}

func TestCacheMissSingleflight(t *testing.T) {
	s := &Server{}
	leader, done := s.joinCacheMiss("repo|commit|file")
	if !leader {
		t.Fatal("first requester should become the cache-miss leader")
	}
	follower, followerDone := s.joinCacheMiss("repo|commit|file")
	if follower || followerDone != done {
		t.Fatal("second requester should wait for the existing cache miss")
	}
	s.finishCacheMiss("repo|commit|file")
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cache-miss followers were not released")
	}
}

func TestImmutableAssetRequiresHash(t *testing.T) {
	if !immutableAsset("main-12345678.js") {
		t.Fatal("hashed build asset should be immutable")
	}
	if immutableAsset("favicon.ico") || immutableAsset("logo.svg") {
		t.Fatal("unhashed public assets must not be immutable")
	}
}

func TestEgressIPPolicyRejectsSpecialAndEmbeddedPrivateIPs(t *testing.T) {
	for _, value := range []string{"100.100.100.200", "198.18.0.1", "64:ff9b::a00:1", "2002:0a00:0001::1"} {
		ip, err := netip.ParseAddr(value)
		if err != nil {
			t.Fatal(err)
		}
		if egressIPAllowed(ip, false) {
			t.Errorf("egressIPAllowed(%s) accepted a blocked destination", value)
		}
	}
}

func TestNAT64UnwrapsAndRechecksEmbeddedIPv4(t *testing.T) {
	for _, raw := range []string{"64:ff9b:1::a00:1", "64:ff9b:1::a9fe:a9fe", "64:ff9b:1::7f00:1"} {
		if egressIPAllowed(netip.MustParseAddr(raw), false) {
			t.Errorf("local-use NAT64 address %s wraps a blocked IPv4 destination", raw)
		}
	}
	if !egressIPAllowed(netip.MustParseAddr("64:ff9b::d21:2121"), false) {
		t.Fatal("well-known NAT64 address wrapping public IPv4 13.33.33.33 was rejected")
	}
}

func TestIdleBodyClosesAStalledRead(t *testing.T) {
	reader, writer := io.Pipe()
	body := newIdleBody(reader, 10*time.Millisecond)
	defer writer.Close()
	defer body.Close()
	result := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 1))
		result <- err
	}()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled read unexpectedly succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("idle body did not interrupt the stalled read")
	}
}

func TestIdleBodyDoesNotTimeOutBetweenReads(t *testing.T) {
	reader, writer := io.Pipe()
	body := newIdleBody(reader, 10*time.Millisecond)
	defer writer.Close()
	defer body.Close()
	firstWrite := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte("a"))
		firstWrite <- err
	}()
	buf := make([]byte, 1)
	if n, err := body.Read(buf); err != nil || n != 1 || buf[0] != 'a' {
		t.Fatalf("first read = %q, %v", buf[:n], err)
	}
	if err := <-firstWrite; err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // Downstream/client delay is not upstream idleness.
	secondWrite := make(chan error, 1)
	go func() {
		_, err := writer.Write([]byte("b"))
		secondWrite <- err
	}()
	if n, err := body.Read(buf); err != nil || n != 1 || buf[0] != 'b' {
		t.Fatalf("second read after client delay = %q, %v", buf[:n], err)
	}
	if err := <-secondWrite; err != nil {
		t.Fatal(err)
	}
}

func TestValidRepoID(t *testing.T) {
	tests := []struct {
		value string
		want  bool
	}{
		{value: "model", want: true},
		{value: "organization/model.v2", want: true},
		{value: "", want: false},
		{value: "../model", want: false},
		{value: "org//model", want: false},
		{value: "org/team/model", want: false},
		{value: "org\\model", want: false},
		{value: "org/model?download=1", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if got := validRepoID(tt.value); got != tt.want {
				t.Errorf("validRepoID(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
