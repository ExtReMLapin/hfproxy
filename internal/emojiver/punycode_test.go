package emojiver

import "testing"

// Sample strings from RFC 3492 section 7.1.
func TestPunycodeRFC3492Vectors(t *testing.T) {
	cases := []struct{ unicode, ascii string }{
		{"3年B組金八先生", "3B-ww4c5e180e575a65lsy2b"},
		{"почемужеонинеговорятпорусски", "b1abfaaepdrnnbgefbadotcwatmq2g4l"},
		{"ひとつ屋根の下2", "2-u9tlzr9756bt3uc0v"},
	}
	for _, c := range cases {
		got, err := punyEncode(c.unicode)
		if err != nil || got != c.ascii {
			t.Errorf("punyEncode(%q) = %q, %v; want %q", c.unicode, got, err, c.ascii)
		}
		back, err := punyDecode(c.ascii)
		if err != nil || back != c.unicode {
			t.Errorf("punyDecode(%q) = %q, %v; want %q", c.ascii, back, err, c.unicode)
		}
	}
}

func TestPunycodeDecodeRejectsGarbage(t *testing.T) {
	for _, in := range []string{"-", "abc-!!", "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"} {
		if _, err := punyDecode(in); err == nil {
			t.Errorf("punyDecode(%q) accepted invalid input", in)
		}
	}
}
