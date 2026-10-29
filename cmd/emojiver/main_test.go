package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

const commit = "4fcdcfc2b1f0a6c1d3e5f7a9b0c2d4e6f8a0b1c3"

func runCLI(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run(args, strings.NewReader(stdin), &out)
	return strings.TrimSpace(out.String()), err
}

func TestEncodeClassifiesCommitsFromStdin(t *testing.T) {
	log := "docs: typo\x00feat: preload queue\n\nbody\x00fix: race\x00"
	got, err := runCLI(t, log, "encode", "--version", "0.1.0", "--commit", commit, "--date", "2024-04-23T23:49:00Z", "--nature", "auto", "--open-regressions", "0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, "🐙1️⃣🔹0️⃣✨🌕🟢👍🏻") {
		t.Fatalf("encode = %q", got)
	}
}

func TestEncodeMapsOpenRegressionsToHealth(t *testing.T) {
	for n, want := range map[string]string{"0": "🟢", "1": "🟡", "5": "🟡", "6": "🔴"} {
		got, err := runCLI(t, "", "encode", "--version", "0.1.0", "--commit", commit, "--nature", "fix", "--open-regressions", n)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(got, "🌚"+want) {
			t.Errorf("--open-regressions %s gave %q, want health %s", n, got, want)
		}
	}
}

func TestEncodeFormats(t *testing.T) {
	tag, err := runCLI(t, "", "encode", "--version", "0.1.0", "--commit", commit, "--format", "tag")
	if err != nil || !strings.HasPrefix(tag, "xn--") {
		t.Fatalf("tag format = %q, %v", tag, err)
	}
	a11y, err := runCLI(t, "", "encode", "--version", "0.1.0", "--commit", commit, "--format", "a11y")
	if err != nil || !strings.HasPrefix(a11y, "octopus, one, dot, zero") {
		t.Fatalf("a11y format = %q, %v", a11y, err)
	}
}

func TestDecodeAcceptsEmojiAndTagForms(t *testing.T) {
	emoji, _ := runCLI(t, "", "encode", "--version", "1.2.3-rc.4", "--commit", commit, "--nature", "security", "--open-regressions", "2")
	tag, _ := runCLI(t, "", "encode", "--version", "1.2.3-rc.4", "--commit", commit, "--nature", "security", "--open-regressions", "2", "--format", "tag")
	for _, in := range []string{emoji, tag} {
		got, err := runCLI(t, "", "decode", in)
		if err != nil {
			t.Fatalf("decode %q: %v", in, err)
		}
		var doc map[string]any
		if err := json.Unmarshal([]byte(got), &doc); err != nil {
			t.Fatalf("decode output %q is not JSON: %v", got, err)
		}
		if doc["semver"] != "1.2.3-rc.4" || doc["nature"] != "security" || doc["health"] != "yellow" || doc["channel"] != "rc" || doc["emoji"] != emoji || doc["tag"] != tag {
			t.Fatalf("decode %q = %v", in, doc)
		}
	}
}

func TestVerify(t *testing.T) {
	emoji, _ := runCLI(t, "", "encode", "--version", "0.1.0", "--commit", commit)
	if _, err := runCLI(t, "", "verify", "--commit", commit, emoji); err != nil {
		t.Fatalf("verify own commit: %v", err)
	}
	if _, err := runCLI(t, "", "verify", "--commit", "deadbeef", emoji); err == nil {
		t.Fatal("verify accepted another commit")
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{nil, {"nope"}, {"encode"}, {"encode", "--version", "0.1.0", "--commit", commit, "--nature", "vibes"}, {"decode"}, {"verify", "x"}} {
		if _, err := runCLI(t, "", args...); err == nil {
			t.Errorf("run(%q) succeeded", args)
		}
	}
}
