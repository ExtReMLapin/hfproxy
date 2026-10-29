// Package logging builds the process logger, whose level can be changed while
// the service runs.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"strings"
)

// Levels are the accepted level names, from the most to the least verbose.
var Levels = []string{"debug", "info", "warn", "error"}

// Normalize returns the canonical name of a level ("warning" is accepted as an
// alias of "warn"), or an error for an unknown one.
func Normalize(name string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "debug":
		return "debug", nil
	case "info", "":
		return "info", nil
	case "warn", "warning":
		return "warn", nil
	case "error":
		return "error", nil
	}
	return "", fmt.Errorf("log level must be one of %s", strings.Join(Levels, ", "))
}

// Parse converts a level name to a slog level.
func Parse(name string) (slog.Level, error) {
	canonical, err := Normalize(name)
	if err != nil {
		return slog.LevelInfo, err
	}
	switch canonical {
	case "debug":
		return slog.LevelDebug, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return slog.LevelInfo, nil
}

// New returns a JSON logger writing to w and the variable that controls its
// level. An unknown name falls back to info; configuration validation rejects
// it before this point.
func New(w io.Writer, level string) (*slog.Logger, *slog.LevelVar) {
	var variable slog.LevelVar
	Set(&variable, level)
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: &variable})), &variable
}

// Set changes the level of a logger created by New. It reports the previous
// and the new canonical names.
func Set(variable *slog.LevelVar, name string) (previous, current string) {
	previous = Name(variable.Level())
	level, err := Parse(name)
	if err != nil {
		level = slog.LevelInfo
	}
	variable.Set(level)
	return previous, Name(level)
}

// Name is the canonical name of a level.
func Name(level slog.Level) string {
	switch {
	case level <= slog.LevelDebug:
		return "debug"
	case level < slog.LevelWarn:
		return "info"
	case level < slog.LevelError:
		return "warn"
	}
	return "error"
}
