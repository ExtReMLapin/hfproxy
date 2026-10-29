package emojiver

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// frozen lists every codename ever released or reserved. Entries may be
// appended to emojidex.json, never changed: a release's codename is forever.
var frozen = []string{"🐙", "🦊", "🦉", "🐢", "🦋", "🐳", "🦀", "🦔", "🐝", "🦜", "🐧", "🦒", "🦓", "🦘", "🦥", "🦦"}

func TestEmojidexIsAppendOnly(t *testing.T) {
	got := Emojidex()
	if len(got) < len(frozen) {
		t.Fatalf("emojidex has %d entries, %d are frozen: entries were removed", len(got), len(frozen))
	}
	for i, want := range frozen {
		if got[i] != want {
			t.Errorf("codename of major %d changed from %s to %s", i, want, got[i])
		}
	}
}

func TestEmojidexEntriesRenderEverywhere(t *testing.T) {
	for i, c := range codenames {
		r, size := utf8.DecodeRuneInString(c.emoji)
		if size != len(c.emoji) {
			t.Errorf("codename %d (%q) must be a single code point: no ZWJ, modifier or variation selector", i, c.emoji)
		}
		// Emoji 13.0 and older, in the blocks with default emoji presentation.
		if r < 0x1F300 || r > 0x1FAD6 {
			t.Errorf("codename %d (%q) is outside the supported emoji range", i, c.emoji)
		}
		if c.name == "" || strings.ContainsFunc(c.name, func(r rune) bool { return r > 0x7f }) {
			t.Errorf("codename %d needs an ASCII CLDR short name for A11y", i)
		}
	}
}
