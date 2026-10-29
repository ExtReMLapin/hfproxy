// Package bytesize formats sizes for people (the console, the CLI, messages).
package bytesize

import "fmt"

// Format writes n in binary units with one decimal, such as "1.5 GiB".
func Format(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= 1024
		if value < 1024 || unit == "TiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	return "0 B"
}
