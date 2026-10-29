package emojiver

import (
	"fmt"
	"strconv"
	"strings"
)

type tokenKind int

const (
	tokCodename tokenKind = iota
	tokDigit
	tokSeparator
	tokNature
	tokMoon
	tokHealth
	tokThumb
	tokTestTube
	tokFruit
)

var tokenKindNames = [...]string{"codename", "keycap digit", separator, "nature", "moon", "health", "👍 with a skin tone", testTube, "checksum fruit"}

type token struct {
	kind  tokenKind
	value int
	text  string
}

// vocabulary is closed: every EmojiVer glyph is one of these strings, so a
// longest-match scan splits input exactly along its grapheme clusters.
var vocabulary = func() map[string]token {
	v := map[string]token{separator: {kind: tokSeparator}, testTube: {kind: tokTestTube}, unknownMoon.emoji: {kind: tokMoon, value: -1}}
	add := func(kind tokenKind, glyphs []glyph) {
		for i, g := range glyphs {
			if _, dup := v[g.emoji]; dup {
				panic("emojiver: glyph used twice: " + g.emoji)
			}
			v[g.emoji] = token{kind: kind, value: i}
		}
	}
	add(tokCodename, codenames)
	add(tokDigit, keycaps[:])
	add(tokNature, natures[:])
	add(tokMoon, moons[:])
	add(tokHealth, healths[:])
	add(tokFruit, fruits[:])
	for i, t := range tones {
		v[thumb+t.emoji] = token{kind: tokThumb, value: i}
	}
	return v
}()

func tokenize(s string) ([]token, error) {
	var out []token
	for pos := 0; pos < len(s); {
		best := ""
		for text := range vocabulary {
			if len(text) > len(best) && strings.HasPrefix(s[pos:], text) {
				best = text
			}
		}
		if best == "" {
			rest := s[pos:]
			if len(rest) >= 4 && rest[0] >= '0' && rest[0] <= '9' && strings.HasPrefix(rest[1:], "⃣") {
				return nil, fmt.Errorf("emojiver: keycap %q at byte %d lacks the variation selector U+FE0F (write %q)", rest[:4], pos, rest[:1]+"️⃣")
			}
			return nil, fmt.Errorf("emojiver: unexpected %q at byte %d", firstRune(rest), pos)
		}
		t := vocabulary[best]
		t.text = best
		out = append(out, t)
		pos += len(best)
	}
	return out, nil
}

func firstRune(s string) string {
	for _, r := range s {
		return string(r)
	}
	return ""
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) next(kind tokenKind) (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, fmt.Errorf("emojiver: input ends where a %s was expected", tokenKindNames[kind])
	}
	t := p.tokens[p.pos]
	if t.kind != kind {
		return token{}, fmt.Errorf("emojiver: found %s where a %s was expected", t.text, tokenKindNames[kind])
	}
	p.pos++
	return t, nil
}

func (p *parser) number() (int, error) {
	var digits strings.Builder
	for p.pos < len(p.tokens) && p.tokens[p.pos].kind == tokDigit {
		digits.WriteByte(byte('0' + p.tokens[p.pos].value))
		p.pos++
	}
	d := digits.String()
	switch {
	case d == "":
		_, err := p.next(tokDigit)
		return 0, err
	case len(d) > 1 && d[0] == '0':
		return 0, fmt.Errorf("emojiver: number %s has a leading zero", d)
	case len(d) > 10:
		return 0, fmt.Errorf("emojiver: number %s is too large", d)
	}
	n, err := strconv.ParseInt(d, 10, 32)
	return int(n), err
}

// Parse decodes an EmojiVer string. It accepts exactly what String produces.
func Parse(s string) (Release, error) {
	tokens, err := tokenize(s)
	if err != nil {
		return Release{}, err
	}
	p := &parser{tokens: tokens}
	var r Release
	steps := []func() error{
		func() error { t, err := p.next(tokCodename); r.Major = t.value; return err },
		func() error { var err error; r.Minor, err = p.number(); return err },
		func() error { _, err := p.next(tokSeparator); return err },
		func() error { var err error; r.Patch, err = p.number(); return err },
		func() error { t, err := p.next(tokNature); r.Nature = Nature(t.value); return err },
		func() error { t, err := p.next(tokMoon); r.Phase = t.value; return err },
		func() error { t, err := p.next(tokHealth); r.Health = Health(t.value); return err },
		func() error { t, err := p.next(tokThumb); r.Channel = Channel(t.value); return err },
		func() error {
			if r.Channel == Stable {
				return nil
			}
			if _, err := p.next(tokTestTube); err != nil {
				return err
			}
			var err error
			if r.Pre, err = p.number(); err == nil && r.Pre == 0 {
				err = fmt.Errorf("emojiver: prerelease number must be at least 1")
			}
			return err
		},
		func() error {
			for range 4 {
				t, err := p.next(tokFruit)
				if err != nil {
					return err
				}
				r.Sum = r.Sum<<4 | uint16(t.value)
			}
			return nil
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return Release{}, err
		}
	}
	if p.pos != len(tokens) {
		return Release{}, fmt.Errorf("emojiver: unexpected %s after the checksum", tokens[p.pos].text)
	}
	if _, err := FromSemver(r.Semver()); err != nil {
		return Release{}, err
	}
	return r, nil
}
