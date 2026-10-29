// Package emojiver renders hfproxy release versions as emoji (EmojiVer).
//
// The canonical version stays SemVer in ASCII: OCI tags, Go build flags and
// the upstream User-Agent cannot carry emoji. EmojiVer is a reversible
// presentation layer on top of it; docs/emojiver.md describes the format.
package emojiver

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Nature summarises the changes since the previous release. Higher values win.
type Nature int

const (
	Unknown Nature = iota
	Chore
	Fix
	Feature
	Security
	Breaking
)

// Health reflects CI and the number of known regressions at release time.
type Health int

const (
	HealthUnknown Health = iota
	Green
	Yellow
	Red
)

// Channel is the release channel, encoded as the skin tone of 👍.
type Channel int

const (
	Stable Channel = iota
	RC
	Beta
	Alpha
	Nightly
)

// Release is the decoded form of an EmojiVer string.
type Release struct {
	Major, Minor, Patch int
	Channel             Channel
	Pre                 int // prerelease number, 0 for stable releases
	Nature              Nature
	Phase               int // moon phase 0..7, or -1 when the release date is unknown
	Health              Health
	Sum                 uint16
}

type glyph struct{ emoji, name string }

const (
	separator = "🔹"
	thumb     = "👍"
	testTube  = "🧪"
)

var (
	keycaps      = [10]glyph{{"0️⃣", "zero"}, {"1️⃣", "one"}, {"2️⃣", "two"}, {"3️⃣", "three"}, {"4️⃣", "four"}, {"5️⃣", "five"}, {"6️⃣", "six"}, {"7️⃣", "seven"}, {"8️⃣", "eight"}, {"9️⃣", "nine"}}
	natures      = [...]glyph{{"❔", "white question mark"}, {"🧹", "broom"}, {"🐛", "bug"}, {"✨", "sparkles"}, {"🔒", "locked"}, {"💥", "collision"}}
	moons        = [8]glyph{{"🌑", "new moon"}, {"🌒", "waxing crescent moon"}, {"🌓", "first quarter moon"}, {"🌔", "waxing gibbous moon"}, {"🌕", "full moon"}, {"🌖", "waning gibbous moon"}, {"🌗", "last quarter moon"}, {"🌘", "waning crescent moon"}}
	unknownMoon  = glyph{"🌚", "new moon face"}
	healths      = [...]glyph{{"⚪", "white circle"}, {"🟢", "green circle"}, {"🟡", "yellow circle"}, {"🔴", "red circle"}}
	tones        = [...]glyph{{"🏻", "light skin tone"}, {"🏼", "medium-light skin tone"}, {"🏽", "medium skin tone"}, {"🏾", "medium-dark skin tone"}, {"🏿", "dark skin tone"}}
	channelNames = [...]string{"", "rc", "beta", "alpha", "nightly"}
	// fruits spell the checksum in hexadecimal, most significant nibble first.
	fruits = [16]glyph{{"🍏", "green apple"}, {"🍎", "red apple"}, {"🍐", "pear"}, {"🍊", "tangerine"}, {"🍋", "lemon"}, {"🍌", "banana"}, {"🍉", "watermelon"}, {"🍇", "grapes"}, {"🍓", "strawberry"}, {"🍈", "melon"}, {"🍒", "cherries"}, {"🍑", "peach"}, {"🍍", "pineapple"}, {"🥝", "kiwi fruit"}, {"🥥", "coconut"}, {"🍅", "tomato"}}
)

//go:embed emojidex.json
var emojidexJSON []byte

var codenames = func() []glyph {
	var doc struct {
		Codenames []struct{ Emoji, Name string } `json:"codenames"`
	}
	if err := json.Unmarshal(emojidexJSON, &doc); err != nil {
		panic("emojiver: invalid emojidex.json: " + err.Error())
	}
	out := make([]glyph, len(doc.Codenames))
	for i, c := range doc.Codenames {
		out[i] = glyph{c.Emoji, c.Name}
	}
	return out
}()

// Emojidex returns the codename emoji of each major version, in order.
func Emojidex() []string {
	out := make([]string, len(codenames))
	for i, c := range codenames {
		out[i] = c.emoji
	}
	return out
}

var semverPattern = regexp.MustCompile(`^(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(?:-(rc|beta|alpha|nightly)\.([1-9][0-9]{0,9}))?$`)

// FromSemver parses the subset of SemVer that EmojiVer can represent:
// MAJOR.MINOR.PATCH with an optional -rc.N, -beta.N, -alpha.N or -nightly.N.
func FromSemver(version string) (Release, error) {
	m := semverPattern.FindStringSubmatch(version)
	if m == nil {
		return Release{}, fmt.Errorf("emojiver: %q is not MAJOR.MINOR.PATCH[-(rc|beta|alpha|nightly).N]", version)
	}
	r := Release{Phase: -1}
	r.Major, _ = strconv.Atoi(m[1])
	r.Minor, _ = strconv.Atoi(m[2])
	r.Patch, _ = strconv.Atoi(m[3])
	if r.Major >= len(codenames) {
		return Release{}, fmt.Errorf("emojiver: major version %d has no codename yet; append one to emojidex.json", r.Major)
	}
	if m[4] != "" {
		for c, name := range channelNames {
			if name == m[4] {
				r.Channel = Channel(c)
			}
		}
		pre, err := strconv.ParseInt(m[5], 10, 32)
		if err != nil {
			return Release{}, fmt.Errorf("emojiver: prerelease number %q is too large", m[5])
		}
		r.Pre = int(pre)
	}
	return r, nil
}

// Semver returns the canonical ASCII version.
func (r Release) Semver() string {
	v := fmt.Sprintf("%d.%d.%d", r.Major, r.Minor, r.Patch)
	if r.Channel != Stable {
		v += fmt.Sprintf("-%s.%d", channelNames[r.Channel], r.Pre)
	}
	return v
}

// Checksum is the first 16 bits of SHA-256(semver + "\n" + commit).
func Checksum(semver, commit string) uint16 {
	sum := sha256.Sum256([]byte(semver + "\n" + commit))
	return binary.BigEndian.Uint16(sum[:2])
}

// Build assembles a release. A zero date, Unknown nature or HealthUnknown
// produce the dedicated "unknown" glyphs, for local builds.
func Build(semver, commit string, date time.Time, nature Nature, health Health) (Release, error) {
	r, err := FromSemver(semver)
	if err != nil {
		return Release{}, err
	}
	if nature < Unknown || nature > Breaking {
		return Release{}, fmt.Errorf("emojiver: invalid nature %d", nature)
	}
	if health < HealthUnknown || health > Red {
		return Release{}, fmt.Errorf("emojiver: invalid health %d", health)
	}
	r.Nature, r.Health, r.Phase = nature, health, phase(date)
	r.Sum = Checksum(r.Semver(), commit)
	return r, nil
}

// Verify checks the checksum against the commit the release was built from.
func (r Release) Verify(commit string) error {
	if Checksum(r.Semver(), commit) != r.Sum {
		return errors.New("emojiver: checksum mismatch (typo, tampered version or wrong commit)")
	}
	return nil
}

func (r Release) glyphs() []glyph {
	var g []glyph
	g = append(g, codenames[r.Major])
	g = append(g, number(r.Minor)...)
	g = append(g, glyph{separator, "dot"})
	g = append(g, number(r.Patch)...)
	g = append(g, natures[r.Nature])
	if r.Phase < 0 {
		g = append(g, unknownMoon)
	} else {
		g = append(g, moons[r.Phase])
	}
	g = append(g, healths[r.Health])
	g = append(g, glyph{thumb + tones[r.Channel].emoji, "thumbs up: " + tones[r.Channel].name})
	if r.Channel != Stable {
		g = append(g, glyph{testTube, "test tube"})
		g = append(g, number(r.Pre)...)
	}
	for shift := 12; shift >= 0; shift -= 4 {
		g = append(g, fruits[(r.Sum>>shift)&0xf])
	}
	return g
}

func number(n int) []glyph {
	var g []glyph
	for _, d := range strconv.Itoa(n) {
		g = append(g, keycaps[d-'0'])
	}
	return g
}

// String renders the EmojiVer form.
func (r Release) String() string {
	var b strings.Builder
	for _, g := range r.glyphs() {
		b.WriteString(g.emoji)
	}
	return b.String()
}

// A11y spells every glyph with its CLDR short name, in ASCII.
func (r Release) A11y() string {
	var names []string
	for _, g := range r.glyphs() {
		names = append(names, g.name)
	}
	return strings.Join(names, ", ")
}

// Tag is the Punycode form, valid as an OCI tag.
func (r Release) Tag() string {
	encoded, err := punyEncode(r.String())
	if err != nil {
		panic("emojiver: " + err.Error()) // String is always valid UTF-8
	}
	return "xn--" + encoded
}

// ParseTag decodes the form produced by Tag.
func ParseTag(tag string) (Release, error) {
	rest, ok := strings.CutPrefix(tag, "xn--")
	if !ok {
		return Release{}, errors.New("emojiver: tag must start with xn--")
	}
	decoded, err := punyDecode(rest)
	if err != nil {
		return Release{}, fmt.Errorf("emojiver: %w", err)
	}
	return Parse(decoded)
}

// HeaderValue encodes s as an RFC 8187 ext-value, for HTTP header fields.
func HeaderValue(s string) string {
	const attrChar = "!#$&+-.^_`|~"
	var b strings.Builder
	b.WriteString("UTF-8''")
	for _, c := range []byte(s) {
		if c < 0x80 && (c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.IndexByte(attrChar, c) >= 0) {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Resolve returns the release of a binary built from semver and commit.
// Release builds embed the EmojiVer string computed by CI (prebuilt); it is
// only trusted when it decodes to the same version and its checksum matches
// the commit. Without it, the release is rendered with the unknown glyphs.
func Resolve(semver, commit, prebuilt string) (Release, error) {
	if prebuilt == "" {
		return Build(semver, commit, time.Time{}, Unknown, HealthUnknown)
	}
	r, err := Parse(prebuilt)
	if err != nil {
		return Release{}, err
	}
	if r.Semver() != semver {
		return Release{}, fmt.Errorf("emojiver: embedded string is for %s, not %s", r.Semver(), semver)
	}
	if err := r.Verify(commit); err != nil {
		return Release{}, err
	}
	return r, nil
}
