package emojiver

import "fmt"

var (
	natureNames  = [...]string{"unknown", "chore", "fix", "feature", "security", "breaking"}
	healthNames  = [...]string{"unknown", "green", "yellow", "red"}
	channelLabel = [...]string{"stable", "rc", "beta", "alpha", "nightly"}
)

func (n Nature) String() string {
	if n < Unknown || n > Breaking {
		return fmt.Sprintf("Nature(%d)", int(n))
	}
	return natureNames[n]
}

func (h Health) String() string {
	if h < HealthUnknown || h > Red {
		return fmt.Sprintf("Health(%d)", int(h))
	}
	return healthNames[h]
}

func (c Channel) String() string {
	if c < Stable || c > Nightly {
		return fmt.Sprintf("Channel(%d)", int(c))
	}
	return channelLabel[c]
}

// ParseNature returns the nature with the given name.
func ParseNature(name string) (Nature, error) {
	for i, n := range natureNames {
		if n == name {
			return Nature(i), nil
		}
	}
	return Unknown, fmt.Errorf("emojiver: unknown nature %q (want one of %v)", name, natureNames)
}

// HealthFromRegressions maps the number of open "Still broken after fix"
// issues to a health: 0 green, 1 to 5 yellow, more red; negative is unknown.
func HealthFromRegressions(open int) Health {
	switch {
	case open < 0:
		return HealthUnknown
	case open == 0:
		return Green
	case open <= 5:
		return Yellow
	}
	return Red
}
