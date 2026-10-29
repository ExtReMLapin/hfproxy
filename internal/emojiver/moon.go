package emojiver

import (
	"math"
	"time"
)

// SynodicMonth is the mean length of a lunar cycle, in days.
const SynodicMonth = 29.530588853

// referenceNewMoon is the new moon of 6 January 2000, 18:14 UTC.
var referenceNewMoon = time.Date(2000, 1, 6, 18, 14, 0, 0, time.UTC)

// phase returns the nearest of the 8 principal moon phases, or -1 for a zero time.
func phase(t time.Time) int {
	if t.IsZero() {
		return -1
	}
	age := math.Mod(t.Sub(referenceNewMoon).Hours()/24, SynodicMonth)
	if age < 0 {
		age += SynodicMonth
	}
	return int(math.Floor(age/SynodicMonth*8+0.5)) % 8
}

// Moon returns the moon phase emoji for t (🌚 when t is the zero time).
func Moon(t time.Time) string {
	if p := phase(t); p >= 0 {
		return moons[p].emoji
	}
	return unknownMoon.emoji
}
