package main

import "time"

// timeparse.go — the single RFC3339 parse rule.

// parseRFC3339 parses a timestamp or reports that it is unusable. One layout
// covers plain and fractional seconds, so every transcript timestamp and clock
// seam shares one rule; each caller keeps its own failure result. A malformed
// string is refused; a well-formed string of any year is accepted, so clock
// seams (WTM_NOW, WTM_CREATED_AT) and subscription dates accept pre-2000
// values without a misleading warning. Transcript boundaries apply their own
// plausibility check (see transcriptTimestamp).
func parseRFC3339(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// transcriptTimestamp parses a transcript record's timestamp. A timestamp whose
// UTC year predates 2000 is corrupt — a Go zero time, an epoch-zero bug, a
// fuzzed record — and cannot describe a real agent session, so it is refused
// like a malformed timestamp instead of feeding absurd spans downstream. The
// check lives only here, at the Pi/Claude transcript boundaries, so the clock
// and subscription seams keep accepting pre-2000 values.
func transcriptTimestamp(s string) (time.Time, bool) {
	t, ok := parseRFC3339(s)
	if !ok || t.UTC().Year() < 2000 {
		return time.Time{}, false
	}
	return t, true
}
