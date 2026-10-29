package emojiver

import (
	"errors"
	"math"
	"strings"
	"unicode/utf8"
)

// Bootstring parameters for Punycode, RFC 3492 section 5.
const (
	punyBase        = 36
	punyTMin        = 1
	punyTMax        = 26
	punySkew        = 38
	punyDamp        = 700
	punyInitialBias = 72
	punyInitialN    = 128
)

var errPunycode = errors.New("invalid punycode")

func punyAdapt(delta, numPoints int, first bool) int {
	if first {
		delta /= punyDamp
	} else {
		delta /= 2
	}
	delta += delta / numPoints
	k := 0
	for delta > ((punyBase-punyTMin)*punyTMax)/2 {
		delta /= punyBase - punyTMin
		k += punyBase
	}
	return k + (punyBase-punyTMin+1)*delta/(delta+punySkew)
}

func punyDigit(d int) byte {
	if d < 26 {
		return byte('a' + d)
	}
	return byte('0' + d - 26)
}

func punyValue(c byte) (int, bool) {
	switch {
	case c >= '0' && c <= '9':
		return int(c-'0') + 26, true
	case c >= 'a' && c <= 'z':
		return int(c - 'a'), true
	case c >= 'A' && c <= 'Z':
		return int(c - 'A'), true
	}
	return 0, false
}

func punyThreshold(k, bias int) int {
	switch {
	case k <= bias:
		return punyTMin
	case k >= bias+punyTMax:
		return punyTMax
	}
	return k - bias
}

// punyEncode implements the Punycode encoding procedure of RFC 3492 section 6.3.
func punyEncode(input string) (string, error) {
	if !utf8.ValidString(input) {
		return "", errPunycode
	}
	runes := []rune(input)
	var out strings.Builder
	for _, r := range runes {
		if r < 0x80 {
			out.WriteRune(r)
		}
	}
	basic := out.Len()
	handled := basic
	if basic > 0 {
		out.WriteByte('-')
	}
	n, delta, bias := punyInitialN, 0, punyInitialBias
	for handled < len(runes) {
		m := math.MaxInt32
		for _, r := range runes {
			if int(r) >= n && int(r) < m {
				m = int(r)
			}
		}
		if (m - n) > (math.MaxInt32-delta)/(handled+1) {
			return "", errPunycode
		}
		delta += (m - n) * (handled + 1)
		n = m
		for _, r := range runes {
			if int(r) < n {
				delta++
			}
			if int(r) != n {
				continue
			}
			q := delta
			for k := punyBase; ; k += punyBase {
				t := punyThreshold(k, bias)
				if q < t {
					break
				}
				out.WriteByte(punyDigit(t + (q-t)%(punyBase-t)))
				q = (q - t) / (punyBase - t)
			}
			out.WriteByte(punyDigit(q))
			bias = punyAdapt(delta, handled+1, handled == basic)
			delta = 0
			handled++
		}
		delta++
		n++
	}
	return out.String(), nil
}

// punyDecode implements RFC 3492 section 6.2 and only accepts the canonical
// encoding, so every Unicode string has exactly one ASCII spelling.
func punyDecode(input string) (string, error) {
	var output []rune
	pos := 0
	if i := strings.LastIndexByte(input, '-'); i >= 0 {
		for j := 0; j < i; j++ {
			if input[j] >= 0x80 {
				return "", errPunycode
			}
			output = append(output, rune(input[j]))
		}
		pos = i + 1
	}
	n, i, bias := punyInitialN, 0, punyInitialBias
	for pos < len(input) {
		oldi, w := i, 1
		for k := punyBase; ; k += punyBase {
			if pos >= len(input) {
				return "", errPunycode
			}
			digit, ok := punyValue(input[pos])
			pos++
			if !ok || digit > (math.MaxInt32-i)/w {
				return "", errPunycode
			}
			i += digit * w
			t := punyThreshold(k, bias)
			if digit < t {
				break
			}
			if w > math.MaxInt32/(punyBase-t) {
				return "", errPunycode
			}
			w *= punyBase - t
		}
		bias = punyAdapt(i-oldi, len(output)+1, oldi == 0)
		if i/(len(output)+1) > math.MaxInt32-n {
			return "", errPunycode
		}
		n += i / (len(output) + 1)
		i %= len(output) + 1
		if n > utf8.MaxRune || (n >= 0xD800 && n <= 0xDFFF) {
			return "", errPunycode
		}
		output = append(output[:i], append([]rune{rune(n)}, output[i:]...)...)
		i++
	}
	decoded := string(output)
	if again, err := punyEncode(decoded); err != nil || !strings.EqualFold(again, input) {
		return "", errPunycode
	}
	return decoded, nil
}
