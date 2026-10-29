package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

func TestNormalizeAcceptsTheUsualNames(t *testing.T) {
	for in, want := range map[string]string{"DEBUG": "debug", "info": "info", "": "info", "Warning": "warn", "warn": "warn", " error ": "error"} {
		got, err := Normalize(in)
		if err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	if _, err := Normalize("verbose"); err == nil {
		t.Error("an unknown level must be rejected")
	}
}

func TestLevelChangesTakeEffectWithoutANewLogger(t *testing.T) {
	var out bytes.Buffer
	logger, level := New(&out, "warn")
	logger.Info("hidden")
	logger.Warn("shown-warn")
	if strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), "shown-warn") {
		t.Fatalf("at warn: %q", out.String())
	}
	previous, current := Set(level, "debug")
	if previous != "warn" || current != "debug" {
		t.Fatalf("Set returned %q -> %q", previous, current)
	}
	logger.Debug("now-visible")
	if !strings.Contains(out.String(), "now-visible") {
		t.Fatalf("at debug: %q", out.String())
	}
	Set(level, "error")
	logger.Warn("hidden-again")
	if strings.Contains(out.String(), "hidden-again") {
		t.Fatal("warn must be hidden at error level")
	}
	if !logger.Enabled(context.Background(), slog.LevelError) {
		t.Fatal("error must stay enabled")
	}
}
