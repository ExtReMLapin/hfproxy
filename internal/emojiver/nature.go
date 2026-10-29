package emojiver

import (
	"regexp"
	"strings"
)

var conventional = regexp.MustCompile(`^([a-zA-Z]+)(?:\(([^)]*)\))?(!)?: `)

// Imperative verbs used by commits that do not follow Conventional Commits.
var verbs = map[string]Nature{
	"fix": Fix, "fixes": Fix, "repair": Fix, "correct": Fix, "handle": Fix, "prevent": Fix, "cap": Fix,
	"add": Feature, "support": Feature, "allow": Feature, "introduce": Feature, "implement": Feature, "translate": Feature, "enable": Feature,
	"harden": Security, "escape": Security, "sanitize": Security,
}

// NatureFromCommits classifies the commit messages since the previous
// release and returns the most significant nature (Chore when empty).
func NatureFromCommits(messages []string) Nature {
	result := Chore
	for _, m := range messages {
		if n := classify(m); n > result {
			result = n
		}
	}
	return result
}

func classify(message string) Nature {
	if strings.Contains(message, "BREAKING CHANGE:") || strings.Contains(message, "BREAKING-CHANGE:") {
		return Breaking
	}
	subject, _, _ := strings.Cut(message, "\n")
	if m := conventional.FindStringSubmatch(subject); m != nil {
		kind, scope := strings.ToLower(m[1]), strings.ToLower(m[2])
		switch {
		case m[3] == "!":
			return Breaking
		case kind == "security" || scope == "security":
			return Security
		case kind == "feat":
			return Feature
		case kind == "fix":
			return Fix
		}
		return Chore
	}
	first, _, _ := strings.Cut(strings.TrimSpace(subject), " ")
	if n, ok := verbs[strings.ToLower(first)]; ok {
		return n
	}
	return Chore
}
