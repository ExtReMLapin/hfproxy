package emojiver

import "testing"

func TestNamesRoundTrip(t *testing.T) {
	for n := Unknown; n <= Breaking; n++ {
		if got, err := ParseNature(n.String()); err != nil || got != n {
			t.Errorf("ParseNature(%q) = %v, %v", n.String(), got, err)
		}
	}
	if _, err := ParseNature("vibes"); err == nil {
		t.Error("ParseNature accepted an unknown name")
	}
	if Health(Yellow).String() != "yellow" || Channel(RC).String() != "rc" || Stable.String() != "stable" {
		t.Error("unexpected Health or Channel names")
	}
}

func TestHealthFromRegressions(t *testing.T) {
	for n, want := range map[int]Health{-1: HealthUnknown, 0: Green, 1: Yellow, 5: Yellow, 6: Red, 100: Red} {
		if got := HealthFromRegressions(n); got != want {
			t.Errorf("HealthFromRegressions(%d) = %v, want %v", n, got, want)
		}
	}
}
