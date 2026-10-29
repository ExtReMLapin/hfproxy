// Command emojiver encodes, decodes and verifies EmojiVer release strings.
// The release workflow uses it; see docs/emojiver.md.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hfproxy/hfproxy/internal/emojiver"
)

const usage = `usage:
  emojiver encode --version X.Y.Z --commit SHA [--date RFC3339] [--nature auto|NAME] [--open-regressions N] [--format emoji|tag|a11y]
  emojiver decode EMOJI|xn--TAG
  emojiver verify --commit SHA EMOJI|xn--TAG

--nature auto reads NUL-separated commit messages on stdin (git log -z --format=%B).`

func main() {
	if err := run(os.Args[1:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "emojiver:", err)
		os.Exit(1)
	}
}

func run(args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	switch args[0] {
	case "encode":
		return encode(args[1:], stdin, stdout)
	case "decode":
		if len(args) != 2 {
			return errors.New(usage)
		}
		r, err := parseAny(args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(stdout).Encode(map[string]any{
			"semver": r.Semver(), "channel": r.Channel.String(), "nature": r.Nature.String(), "health": r.Health.String(),
			"moon_phase": r.Phase, "checksum": fmt.Sprintf("%04x", r.Sum), "emoji": r.String(), "tag": r.Tag(), "a11y": r.A11y(),
		})
	case "verify":
		flags := flag.NewFlagSet("verify", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		commit := flags.String("commit", "", "commit SHA")
		if err := flags.Parse(args[1:]); err != nil || *commit == "" || flags.NArg() != 1 {
			return errors.New(usage)
		}
		r, err := parseAny(flags.Arg(0))
		if err != nil {
			return err
		}
		if err := r.Verify(*commit); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "ok %s\n", r.Semver())
		return nil
	}
	return errors.New(usage)
}

func encode(args []string, stdin io.Reader, stdout io.Writer) error {
	flags := flag.NewFlagSet("encode", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	version := flags.String("version", "", "SemVer version")
	commit := flags.String("commit", "", "commit SHA")
	date := flags.String("date", "", "release date (RFC 3339)")
	natureName := flags.String("nature", "unknown", "auto, or one of unknown, chore, fix, feature, security, breaking")
	regressions := flags.Int("open-regressions", -1, "open regressions at release time (-1: unknown)")
	format := flags.String("format", "emoji", "emoji, tag or a11y")
	if err := flags.Parse(args); err != nil || *version == "" || *commit == "" {
		return errors.New(usage)
	}
	var at time.Time
	if *date != "" {
		var err error
		if at, err = time.Parse(time.RFC3339, *date); err != nil {
			return fmt.Errorf("--date: %w", err)
		}
	}
	nature, err := emojiver.ParseNature(*natureName)
	if *natureName == "auto" {
		log, readErr := io.ReadAll(stdin)
		if readErr != nil {
			return readErr
		}
		var messages []string
		for _, m := range strings.Split(string(log), "\x00") {
			if strings.TrimSpace(m) != "" {
				messages = append(messages, strings.TrimSpace(m))
			}
		}
		nature, err = emojiver.NatureFromCommits(messages), nil
	}
	if err != nil {
		return err
	}
	r, err := emojiver.Build(*version, *commit, at, nature, emojiver.HealthFromRegressions(*regressions))
	if err != nil {
		return err
	}
	switch *format {
	case "emoji":
		fmt.Fprintln(stdout, r.String())
	case "tag":
		fmt.Fprintln(stdout, r.Tag())
	case "a11y":
		fmt.Fprintln(stdout, r.A11y())
	default:
		return fmt.Errorf("--format: unknown format %q", *format)
	}
	return nil
}

func parseAny(s string) (emojiver.Release, error) {
	if strings.HasPrefix(s, "xn--") {
		return emojiver.ParseTag(s)
	}
	return emojiver.Parse(s)
}
