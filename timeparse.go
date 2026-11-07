package main

import "time"

// timeparse.go — the single RFC3339 parse rule.

// parseRFC3339 parses a timestamp or reports that it is unusable. One layout
// covers plain and fractional seconds, so every transcript timestamp and clock
// seam shares one rule; each caller keeps its own failure result.
func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	return t, err == nil
}
