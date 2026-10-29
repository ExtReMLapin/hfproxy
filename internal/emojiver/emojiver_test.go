package emojiver

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

const testCommit = "4fcdcfc2b1f0a6c1d3e5f7a9b0c2d4e6f8a0b1c3"

var released = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func TestFromSemverParsesCoreAndPrerelease(t *testing.T) {
	cases := []struct {
		in   string
		want Release
	}{
		{"0.1.0", Release{Major: 0, Minor: 1, Patch: 0, Channel: Stable}},
		{"2.10.7", Release{Major: 2, Minor: 10, Patch: 7, Channel: Stable}},
		{"0.2.0-rc.3", Release{Major: 0, Minor: 2, Patch: 0, Channel: RC, Pre: 3}},
		{"1.0.0-beta.1", Release{Major: 1, Minor: 0, Patch: 0, Channel: Beta, Pre: 1}},
		{"1.0.0-alpha.12", Release{Major: 1, Minor: 0, Patch: 0, Channel: Alpha, Pre: 12}},
		{"1.0.0-nightly.20261029", Release{Major: 1, Minor: 0, Patch: 0, Channel: Nightly, Pre: 20261029}},
	}
	for _, c := range cases {
		got, err := FromSemver(c.in)
		if err != nil {
			t.Fatalf("FromSemver(%q): %v", c.in, err)
		}
		if got.Major != c.want.Major || got.Minor != c.want.Minor || got.Patch != c.want.Patch || got.Channel != c.want.Channel || got.Pre != c.want.Pre {
			t.Errorf("FromSemver(%q) = %+v, want %+v", c.in, got, c.want)
		}
		if got.Semver() != c.in {
			t.Errorf("Semver() = %q, want %q", got.Semver(), c.in)
		}
	}
}

func TestFromSemverRejectsWhatEmojiVerCannotRepresent(t *testing.T) {
	for _, in := range []string{"", "v0.1.0", "0.1", "01.2.3", "0.1.0-rc", "0.1.0-rc.0", "0.1.0-pre.1", "0.1.0+build.5", "0.1.0-rc.1.2", "99.0.0"} {
		if _, err := FromSemver(in); err == nil {
			t.Errorf("FromSemver(%q) accepted a version EmojiVer cannot represent", in)
		}
	}
}

func TestBuildRendersTheDocumentedLayout(t *testing.T) {
	r, err := Build("0.1.0", testCommit, released, Feature, Green)
	if err != nil {
		t.Fatal(err)
	}
	s := r.String()
	prefix := "🐙1️⃣🔹0️⃣✨" + Moon(released) + "🟢👍🏻"
	if !strings.HasPrefix(s, prefix) {
		t.Fatalf("String() = %q, want prefix %q", s, prefix)
	}
	if fruits := strings.TrimPrefix(s, prefix); len([]rune(fruits)) != 4 {
		t.Fatalf("checksum part %q must be 4 fruit emojis", fruits)
	}
}

func TestPrereleaseUsesSkinToneAndTestTube(t *testing.T) {
	r, err := Build("0.2.0-beta.2", testCommit, released, Fix, Yellow)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.String(), "🟡👍🏽🧪2️⃣") {
		t.Fatalf("String() = %q, want the beta tone followed by 🧪2️⃣", r.String())
	}
}

func TestUnknownBuildFieldsHaveTheirOwnGlyphs(t *testing.T) {
	r, err := Build("0.1.0", "", time.Time{}, Unknown, HealthUnknown)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.String(), "❔🌚⚪") {
		t.Fatalf("String() = %q, want ❔🌚⚪ for an unknown nature, date and health", r.String())
	}
}

func TestParseIsTheInverseOfString(t *testing.T) {
	for _, v := range []string{"0.0.3", "0.1.0", "3.141.59", "1.0.0-rc.1", "15.0.0-nightly.7"} {
		for _, n := range []Nature{Unknown, Chore, Fix, Feature, Security, Breaking} {
			r, err := Build(v, testCommit, released.Add(time.Duration(n)*72*time.Hour), n, Red)
			if err != nil {
				t.Fatal(err)
			}
			back, err := Parse(r.String())
			if err != nil {
				t.Fatalf("Parse(%q): %v", r.String(), err)
			}
			if back != r {
				t.Fatalf("Parse(String()) = %+v, want %+v", back, r)
			}
		}
	}
}

func TestParseRejectsMalformedInput(t *testing.T) {
	good, err := Build("0.1.0", testCommit, released, Feature, Green)
	if err != nil {
		t.Fatal(err)
	}
	s := good.String()
	cases := map[string]string{
		"empty":                    "",
		"ascii":                    "0.1.0",
		"keycap without FE0F":      strings.Replace(s, "1️⃣", "1⃣", 1),
		"missing separator":        strings.Replace(s, "🔹", "", 1),
		"leading zero":             strings.Replace(s, "🔹0️⃣", "🔹0️⃣0️⃣", 1),
		"unknown codename":         strings.Replace(s, "🐙", "🦖", 1),
		"truncated checksum":       string([]rune(s)[:len([]rune(s))-1]),
		"trailing garbage":         s + "🍕",
		"test tube on stable tone": strings.Replace(s, "👍🏻", "👍🏻🧪1️⃣", 1),
		"prerelease without tube":  strings.Replace(s, "👍🏻", "👍🏼", 1),
	}
	for name, in := range cases {
		if _, err := Parse(in); err == nil {
			t.Errorf("%s: Parse(%q) accepted malformed input", name, in)
		}
	}
}

func TestParseExplainsAMissingVariationSelector(t *testing.T) {
	_, err := Parse("🐙1⃣🔹0️⃣")
	if err == nil || !strings.Contains(err.Error(), "U+FE0F") {
		t.Fatalf("error = %v, want a hint about U+FE0F", err)
	}
}

func TestVerifyDetectsTyposAndWrongCommits(t *testing.T) {
	r, err := Build("0.1.0", testCommit, released, Feature, Green)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Verify(testCommit); err != nil {
		t.Fatalf("Verify(own commit) = %v", err)
	}
	if err := r.Verify("deadbeef"); err == nil {
		t.Fatal("Verify accepted another commit")
	}
	tampered := r
	tampered.Patch = 1
	if err := tampered.Verify(testCommit); err == nil {
		t.Fatal("Verify accepted a tampered patch number")
	}
}

func TestMoonPhases(t *testing.T) {
	ref := time.Date(2000, 1, 6, 18, 14, 0, 0, time.UTC)
	month := time.Duration(SynodicMonth * float64(24*time.Hour))
	for i, want := range []string{"🌑", "🌒", "🌓", "🌔", "🌕", "🌖", "🌗", "🌘"} {
		at := ref.Add(month * time.Duration(i) / 8)
		if got := Moon(at); got != want {
			t.Errorf("Moon(new moon + %d/8 month) = %s, want %s", i, got, want)
		}
	}
	// Full moon of 23 April 2024, 23:49 UTC.
	if got := Moon(time.Date(2024, 4, 23, 23, 49, 0, 0, time.UTC)); got != "🌕" {
		t.Errorf("Moon(2024-04-23 full moon) = %s, want 🌕", got)
	}
	if got := Moon(time.Time{}); got != "🌚" {
		t.Errorf("Moon(zero time) = %s, want 🌚", got)
	}
}

func TestNatureFromCommits(t *testing.T) {
	cases := []struct {
		subjects []string
		want     Nature
	}{
		{nil, Chore},
		{[]string{"docs: typo", "ci: bump action"}, Chore},
		{[]string{"fix(hub): keep X-Linked-Etag", "docs: x"}, Fix},
		{[]string{"Fix the cursor encoding"}, Fix},
		{[]string{"feat: preload queue", "fix: race"}, Feature},
		{[]string{"Add the 0.1.0 release notes"}, Feature},
		{[]string{"feat: x", "fix(security): escape LDAP filter"}, Security},
		{[]string{"security: rotate keys"}, Security},
		{[]string{"feat!: drop /dashboard", "fix(security): y"}, Breaking},
		{[]string{"refactor: x\n\nBREAKING CHANGE: config renamed"}, Breaking},
	}
	for _, c := range cases {
		if got := NatureFromCommits(c.subjects); got != c.want {
			t.Errorf("NatureFromCommits(%q) = %v, want %v", c.subjects, got, c.want)
		}
	}
}

func TestA11yReadsEveryGlyph(t *testing.T) {
	r, err := Build("0.1.0-rc.2", testCommit, time.Date(2024, 4, 23, 23, 49, 0, 0, time.UTC), Security, Green)
	if err != nil {
		t.Fatal(err)
	}
	got := r.A11y()
	for _, want := range []string{"octopus", "one", "dot", "zero", "locked", "full moon", "green circle", "thumbs up: medium-light skin tone", "test tube", "two"} {
		if !strings.Contains(got, want) {
			t.Errorf("A11y() = %q, missing %q", got, want)
		}
	}
	if strings.ContainsFunc(got, func(r rune) bool { return r > 0x7f }) {
		t.Errorf("A11y() = %q must be plain ASCII for screen readers and logs", got)
	}
}

var ociTag = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127}$`)

func TestPunycodeTagIsAValidOCITagAndRoundTrips(t *testing.T) {
	for _, v := range []string{"0.1.0", "15.99.99-nightly.20261029"} {
		r, err := Build(v, testCommit, released, Breaking, Red)
		if err != nil {
			t.Fatal(err)
		}
		tag := r.Tag()
		if !ociTag.MatchString(tag) || !strings.HasPrefix(tag, "xn--") {
			t.Fatalf("Tag() = %q is not a valid OCI tag", tag)
		}
		back, err := ParseTag(tag)
		if err != nil || back != r {
			t.Fatalf("ParseTag(%q) = %+v, %v; want %+v", tag, back, err, r)
		}
	}
}

func TestHeaderValueUsesRFC8187ExtValue(t *testing.T) {
	got := HeaderValue("🐙1️⃣")
	if got != "UTF-8''%F0%9F%90%991%EF%B8%8F%E2%83%A3" {
		t.Fatalf("HeaderValue = %q", got)
	}
	if strings.ContainsFunc(got, func(r rune) bool { return r > 0x7e || r < 0x21 }) {
		t.Fatalf("HeaderValue = %q is not a valid ASCII header value", got)
	}
}

func FuzzRoundTrip(f *testing.F) {
	f.Add(uint8(0), uint16(1), uint16(0), uint8(0), uint32(0), uint8(3), int64(0), uint8(1), "abc")
	f.Add(uint8(15), uint16(999), uint16(12), uint8(4), uint32(20261029), uint8(5), int64(1_800_000_000), uint8(3), testCommit)
	f.Fuzz(func(t *testing.T, major uint8, minor, patch uint16, channel uint8, pre uint32, nature uint8, unix int64, health uint8, commit string) {
		r := Release{Major: int(major) % len(Emojidex()), Minor: int(minor), Patch: int(patch), Channel: Channel(channel % 5)}
		if r.Channel != Stable {
			r.Pre = int(pre%1_000_000_000) + 1
		}
		var date time.Time
		if unix > 0 {
			date = time.Unix(unix%4_000_000_000, 0).UTC()
		}
		built, err := Build(r.Semver(), commit, date, Nature(nature%6), Health(health%4))
		if err != nil {
			t.Fatalf("Build(%q): %v", r.Semver(), err)
		}
		back, err := Parse(built.String())
		if err != nil || back != built {
			t.Fatalf("Parse(%q) = %+v, %v; want %+v", built.String(), back, err, built)
		}
		viaTag, err := ParseTag(built.Tag())
		if err != nil || viaTag != built {
			t.Fatalf("ParseTag(%q) = %+v, %v", built.Tag(), viaTag, err)
		}
		if err := back.Verify(commit); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}
