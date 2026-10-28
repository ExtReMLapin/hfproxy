package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/nicolas/hfproxy/internal/db"
)

func TestCachePinCommands(t *testing.T) {
	sqlDB := rotationDatabase(t)
	ctx := context.Background()
	if _, err := sqlDB.Exec(`INSERT INTO repositories(id,repo_type,repo_id,access_policy) VALUES($1,'models','org/model','public')`, db.NewID()); err != nil {
		t.Fatal(err)
	}
	const commit = "0123456789abcdef0123456789abcdef01234567"
	var out bytes.Buffer
	run := func(pin bool, repoType, repo, sha string) error {
		out.Reset()
		return changePin(ctx, &out, sqlDB, pin, repoType, repo, sha)
	}

	if err := run(true, "models", "org/model", ""); err != nil || !strings.Contains(out.String(), "every revision of models/org/model") {
		t.Fatalf("pin repository: %v %q", err, out.String())
	}
	if err := run(true, "models", "org/model", ""); err != nil {
		t.Fatalf("pinning twice must be harmless: %v", err)
	}
	if err := run(true, "models", "org/model", commit); err != nil || !strings.Contains(out.String(), "commit "+commit) {
		t.Fatalf("pin commit: %v %q", err, out.String())
	}
	out.Reset()
	if err := listPins(ctx, &out, sqlDB); err != nil || strings.Count(out.String(), "models/org/model") != 2 || !strings.Contains(out.String(), "every revision") || !strings.Contains(out.String(), "commit "+commit) {
		t.Fatalf("pins: %v\n%s", err, out.String())
	}

	for name, call := range map[string]func() error{
		"missing repo":       func() error { return run(true, "models", "", "") },
		"bad type":           func() error { return run(true, "things", "org/model", "") },
		"short commit":       func() error { return run(true, "models", "org/model", "abc") },
		"branch name":        func() error { return run(true, "models", "org/model", "main") },
		"unregistered repo":  func() error { return run(true, "models", "org/other", "") },
		"unpin missing pin":  func() error { return run(false, "models", "org/model", strings.Repeat("f", 40)) },
		"unpin unregistered": func() error { return run(false, "models", "org/other", "") },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}

	if err := run(false, "models", "org/model", commit); err != nil || !strings.Contains(out.String(), "Unpinned commit") {
		t.Fatalf("unpin commit: %v %q", err, out.String())
	}
	if err := run(false, "models", "org/model", ""); err != nil {
		t.Fatalf("unpin repository: %v", err)
	}
	out.Reset()
	if err := listPins(ctx, &out, sqlDB); err != nil || !strings.Contains(out.String(), "No pins.") {
		t.Fatalf("empty list: %v %q", err, out.String())
	}
}
