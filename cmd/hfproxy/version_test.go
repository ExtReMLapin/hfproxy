package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/hfproxy/hfproxy/internal/emojiver"
	"github.com/hfproxy/hfproxy/internal/server"
)

func withVersion(t *testing.T, v, c, emoji string) {
	t.Helper()
	oldV, oldC, oldE := version, commit, server.Emoji
	version, commit, server.Emoji = v, c, emoji
	t.Cleanup(func() { version, commit, server.Emoji = oldV, oldC, oldE })
}

func TestVersionCommandFormats(t *testing.T) {
	const sha = "4fcdcfc2b1f0a6c1d3e5f7a9b0c2d4e6f8a0b1c3"
	built, err := emojiver.Build("0.1.0", sha, time.Date(2024, 4, 23, 23, 49, 0, 0, time.UTC), emojiver.Feature, emojiver.Green)
	if err != nil {
		t.Fatal(err)
	}
	withVersion(t, "0.1.0", sha, built.String())
	cases := map[string]string{
		"":         "hfproxy 0.1.0 (commit " + sha + ")\n",
		"--emoji":  built.String() + "\n",
		"--a11y":   built.A11y() + "\n",
		"--tag":    built.Tag() + "\n",
		"--semver": "0.1.0\n",
	}
	for flag, want := range cases {
		var out bytes.Buffer
		args := []string{}
		if flag != "" {
			args = append(args, flag)
		}
		if err := versionCommand(args, &out); err != nil {
			t.Fatalf("version %s: %v", flag, err)
		}
		if out.String() != want {
			t.Errorf("version %s = %q, want %q", flag, out.String(), want)
		}
	}
}

func TestVersionEmojiFailsForDevBuilds(t *testing.T) {
	withVersion(t, "dev", "unknown", "")
	var out bytes.Buffer
	if err := versionCommand([]string{"--emoji"}, &out); err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("err = %v, want an error naming the dev build", err)
	}
	if err := versionCommand([]string{"--bogus"}, &out); err == nil {
		t.Fatal("unknown flag accepted")
	}
}
