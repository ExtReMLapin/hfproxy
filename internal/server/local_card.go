package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"go.yaml.in/yaml/v3"

	"github.com/hfproxy/hfproxy/internal/db"
)

// A model or dataset card is the README.md of the repository: Markdown, with a
// block of YAML metadata (license, tags, pipeline, languages, ...) at the top.
// The Hub's clients ask the server to validate that block before uploading a
// README, and read the metadata back from the repository's description.

const (
	cardMaxBytes   = 256 << 10
	cardCacheLimit = 256
)

// splitFrontMatter separates the YAML block between two "---" lines at the top of
// a README from the Markdown after it. has is false when there is no block.
func splitFrontMatter(content string) (front, body string, has bool, err error) {
	content = strings.TrimPrefix(content, "\ufeff")
	if !strings.HasPrefix(content, "---\n") && !strings.HasPrefix(content, "---\r\n") {
		return "", content, false, nil
	}
	rest := content[strings.Index(content, "\n")+1:]
	for offset := 0; offset <= len(rest); {
		end := strings.Index(rest[offset:], "\n")
		line := rest[offset:]
		next := len(rest) + 1
		if end >= 0 {
			line, next = rest[offset:offset+end], offset+end+1
		}
		if strings.TrimRight(line, "\r") == "---" {
			return rest[:offset], rest[min(next, len(rest)):], true, nil
		}
		offset = next
	}
	return "", content, true, fmt.Errorf("the metadata block starting with --- is not closed by another ---")
}

// parseCard reads the metadata block of a README.
func parseCard(content string) (data map[string]any, body string, err error) {
	front, body, has, err := splitFrontMatter(content)
	if err != nil {
		return nil, body, err
	}
	if !has || strings.TrimSpace(front) == "" {
		return map[string]any{}, body, nil
	}
	var parsed any
	if err := yaml.Unmarshal([]byte(front), &parsed); err != nil {
		return nil, body, fmt.Errorf("the metadata is not valid YAML: %s", firstLine(err.Error()))
	}
	if parsed == nil {
		return map[string]any{}, body, nil
	}
	asMap, ok := normalizeYAML(parsed).(map[string]any)
	if !ok {
		return nil, body, fmt.Errorf("the metadata must be a set of key: value pairs")
	}
	return asMap, body, nil
}

func firstLine(text string) string {
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		return text[:i]
	}
	return text
}

// normalizeYAML turns the maps YAML produces into ones JSON can hold.
func normalizeYAML(value any) any {
	switch v := value.(type) {
	case map[string]any:
		for key, item := range v {
			v[key] = normalizeYAML(item)
		}
		return v
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprint(key)] = normalizeYAML(item)
		}
		return out
	case []any:
		for i, item := range v {
			v[i] = normalizeYAML(item)
		}
		return v
	}
	return value
}

// apiValidateYAML is POST /api/validate-yaml, which huggingface_hub calls before
// it uploads a README.md.
func (s *Server) apiValidateYAML(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		hfError(w, http.StatusMethodNotAllowed, "", "Use POST.")
		return
	}
	var in struct {
		Content string `json:"content"`
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		hfError(w, http.StatusBadRequest, "", "The request body is not valid JSON.")
		return
	}
	data, _, err := parseCard(in.Content)
	if err != nil {
		writeHFJSON(w, http.StatusBadRequest, map[string]any{"errors": []map[string]string{{"message": err.Error()}}, "warnings": []any{}})
		return
	}
	warnings := []map[string]string{}
	if len(data) == 0 {
		warnings = append(warnings, map[string]string{"message": "empty or missing yaml metadata in repo card"})
	}
	writeHFJSON(w, http.StatusOK, map[string]any{"errors": []any{}, "warnings": warnings})
}

// cardInfo is what a repository's description takes from its README.
type cardInfo struct {
	Data map[string]any
	Tags []string
}

var cardCache = struct {
	sync.Mutex
	entries map[string]cardInfo
}{entries: map[string]cardInfo{}}

// readCard reads and parses the README.md of a commit, if it has one. The
// content of a file never changes, so the result is kept by its hash.
func (s *Server) readCard(ctx context.Context, files []db.LocalFile) cardInfo {
	for _, f := range files {
		if f.Path != "README.md" || f.Size > cardMaxBytes {
			continue
		}
		cardCache.Lock()
		cached, ok := cardCache.entries[f.SHA256]
		cardCache.Unlock()
		if ok {
			return cached
		}
		objects := s.currentStorage()
		if objects == nil {
			return cardInfo{}
		}
		out, err := objects.Get(ctx, f.StorageKey, "")
		if err != nil {
			return cardInfo{}
		}
		raw, err := io.ReadAll(io.LimitReader(out.Body, cardMaxBytes+1))
		out.Body.Close()
		if err != nil || len(raw) > cardMaxBytes {
			return cardInfo{}
		}
		data, _, err := parseCard(string(raw))
		if err != nil {
			data = map[string]any{}
		}
		info := cardInfo{Data: data, Tags: cardTags(data)}
		cardCache.Lock()
		if len(cardCache.entries) >= cardCacheLimit {
			for key := range cardCache.entries {
				delete(cardCache.entries, key)
				break
			}
		}
		cardCache.entries[f.SHA256] = info
		cardCache.Unlock()
		return info
	}
	return cardInfo{}
}

// cardTags builds the tag list the Hub shows: the card's own tags plus one for
// the license, the pipeline, the library and each language and dataset.
func cardTags(data map[string]any) []string {
	seen := map[string]bool{}
	var tags []string
	add := func(tag string) {
		tag = strings.TrimSpace(tag)
		if tag != "" && len(tag) <= 100 && !seen[tag] && len(tags) < 200 {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	list := func(value any) []string {
		switch v := value.(type) {
		case string:
			return []string{v}
		case []any:
			out := make([]string, 0, len(v))
			for _, item := range v {
				if text, ok := item.(string); ok {
					out = append(out, text)
				}
			}
			return out
		}
		return nil
	}
	for _, tag := range list(data["tags"]) {
		add(tag)
	}
	for _, license := range list(data["license"]) {
		add("license:" + license)
	}
	for _, key := range []string{"language", "datasets", "base_model"} {
		for _, value := range list(data[key]) {
			add(key + ":" + value)
		}
	}
	for _, key := range []string{"pipeline_tag", "library_name"} {
		for _, value := range list(data[key]) {
			add(value)
		}
	}
	sort.Strings(tags)
	return tags
}
