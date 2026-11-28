package main

import (
	"os"
	"time"
)

// seam_env.go — the program-wide env seams: the clock and creation pins the
// container test suite sets. They are consumed across commands, so they live
// here, not under status.go.

// nowFunc is the clock seam. Production reads the wall clock; the container
// harness pins WTM_NOW so every clock read is deterministic in goldens: status
// ages, worktreeLastUsed and the gc TTL decision all draw from this one seam.
// A malformed value is a test-setup error, so warnNowEnv discloses it once
// before any renderer starts: a raw warning inside the frame is erased. As a
// package variable it is also the injection seam that pins the one-clock-per-run
// contract.
var nowFunc = func() time.Time {
	if t, ok := parseNowEnv(); ok {
		return t
	}
	return time.Now()
}

// nowEnv is the WTM_NOW clock seam. The container test suite pins it so
// worktree ages and the gc TTL boundary are byte-stable; production never sets
// it.
const nowEnv = "WTM_NOW"

// parseNowEnv parses the WTM_NOW seam. An absent or malformed value reports
// ok=false, so nowFunc falls back to the wall clock.
func parseNowEnv() (time.Time, bool) {
	return parseEnvRFC3339(nowEnv)
}

// warnNowEnv runs at dispatch, before any subcommand can open a renderer.
func warnNowEnv() {
	warnTimeSeam(nowEnv, "the wall clock")
}

// createdAtEnv is the WTM_CREATED_AT test seam. Fixtures cannot seed a
// filesystem birth time, so the scenario harness pins it; production never
// sets it. A malformed value is a test-setup error, so warnCreatedAtEnv
// discloses it once while worktreeCreated falls through to the FS fact.
const createdAtEnv = "WTM_CREATED_AT"

// parseCreatedAtEnv parses the creation seam. Absent and malformed values both
// report ok=false, so the caller uses the filesystem birth time.
func parseCreatedAtEnv() (time.Time, bool) {
	return parseEnvRFC3339(createdAtEnv)
}

// warnCreatedAtEnv runs at dispatch beside warnNowEnv (see main.go for why).
func warnCreatedAtEnv() {
	warnTimeSeam(createdAtEnv, "the filesystem birth time")
}

// parseEnvRFC3339 parses one env time seam: absent or malformed → ok=false.
func parseEnvRFC3339(name string) (time.Time, bool) {
	raw := os.Getenv(name)
	if raw == "" {
		return time.Time{}, false
	}
	return parseRFC3339(raw)
}

// warnTimeSeam discloses a set-but-malformed env time seam once, at dispatch,
// before any renderer starts.
func warnTimeSeam(name, fallback string) {
	raw := os.Getenv(name)
	if raw == "" {
		return
	}
	if _, ok := parseRFC3339(raw); !ok {
		warnf("%s %q is not RFC3339: using %s", name, raw, fallback)
	}
}
