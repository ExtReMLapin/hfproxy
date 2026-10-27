package server

import (
	"net/url"
	"testing"
)

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
		{name: "multiple ranges unsupported", value: "bytes=0-1,4-5", valid: false},
		{name: "start beyond object", value: "bytes=10-", valid: false},
		{name: "reversed range", value: "bytes=8-3", valid: false},
		{name: "zero suffix", value: "bytes=-0", valid: false},
		{name: "unknown unit", value: "items=0-1", valid: false},
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
