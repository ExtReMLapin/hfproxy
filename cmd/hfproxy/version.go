package main

import (
	"fmt"
	"io"

	"github.com/hfproxy/hfproxy/internal/emojiver"
	"github.com/hfproxy/hfproxy/internal/server"
)

// versionCommand prints the version, by default in ASCII, or one EmojiVer form.
func versionCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		_, err := fmt.Fprintf(out, "hfproxy %s (commit %s)\n", version, commit)
		return err
	}
	if len(args) != 1 {
		return fmt.Errorf("usage: hfproxy version [--emoji|--a11y|--tag|--semver]")
	}
	if args[0] == "--semver" {
		_, err := fmt.Fprintln(out, version)
		return err
	}
	release, err := emojiver.Resolve(version, commit, server.Emoji)
	if err != nil {
		if release, err = emojiver.Resolve(version, commit, ""); err != nil {
			return fmt.Errorf("version %s has no EmojiVer form: %w", version, err)
		}
	}
	switch args[0] {
	case "--emoji":
		_, err = fmt.Fprintln(out, release.String())
	case "--a11y":
		_, err = fmt.Fprintln(out, release.A11y())
	case "--tag":
		_, err = fmt.Fprintln(out, release.Tag())
	default:
		err = fmt.Errorf("usage: hfproxy version [--emoji|--a11y|--tag|--semver]")
	}
	return err
}
