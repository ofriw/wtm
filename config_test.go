package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// config_test.go — Tier-1 contracts for config.go. Settings are exercised
// through the real filesystem under a sandboxed HOME; no mocks.

func TestTTLDays(t *testing.T) {
	cases := []struct {
		in   string
		want int
		ok   bool
	}{
		{"1d", 1, true},
		{"30d", 30, true},
		{"90d", 90, true},
		{"3650d", 3650, true},
		{"", 0, false},
		{"d", 0, false},
		{"0d", 0, false},
		{"00d", 0, false},
		{"-1d", 0, false},
		{"1h", 0, false},
		{"1.5d", 0, false},
		{"90D", 0, false},
		{" 90d", 0, false},
		{"90d ", 0, false},
		// (1<<63-1)/(24h) == 106751, so 106752 overflows the int64 bound.
		{"106752d", 0, false},
		{"99999999999999999999d", 0, false},
	}
	for _, tc := range cases {
		got, err := ttlDays(tc.in)
		switch {
		case tc.ok && (err != nil || got != tc.want):
			t.Fatalf("ttlDays(%q) = (%d,%v), want (%d,nil)", tc.in, got, err, tc.want)
		case !tc.ok && err == nil:
			t.Fatalf("ttlDays(%q) = (%d,nil), want error", tc.in, got)
		}
	}
}

func TestLoadSettingsDefaults(t *testing.T) {
	sandbox(t)
	s, p, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.UnusedTTL != "90d" {
		t.Fatalf("default unusedTTL = %q, want 90d", s.UnusedTTL)
	}
	if p != settingsFile(t) {
		t.Fatalf("settings path = %q, want %q", p, settingsFile(t))
	}
}

func TestLoadSettingsRoundTrip(t *testing.T) {
	sandbox(t)
	setTTL(t, "30d")
	s, p, err := loadSettings()
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.UnusedTTL != "30d" {
		t.Fatalf("unusedTTL = %q, want 30d", s.UnusedTTL)
	}
	if p != settingsFile(t) {
		t.Fatalf("settings path = %q, want %q", p, settingsFile(t))
	}
}

// TestLoadSettingsToleratesUnknownField pins the forward-compat contract:
// loadSettings is the SSOT and deliberately tolerates unknown keys, so an
// unrecognized field must NOT fail the load while known keys still decode.
// (The approved plan named this RejectsUnknownField; the code is the SSOT.)
func TestLoadSettingsToleratesUnknownField(t *testing.T) {
	sandbox(t)
	writeFile(t, settingsFile(t), "{\"unusedTTL\":\"45d\",\"futureKey\":true}\n", 0o600)
	s, _, err := loadSettings()
	if err != nil {
		t.Fatalf("unknown key must be tolerated, got %v", err)
	}
	if s.UnusedTTL != "45d" {
		t.Fatalf("unusedTTL = %q, want 45d", s.UnusedTTL)
	}
}

func TestLoadSettingsRejectsTrailingJSON(t *testing.T) {
	sandbox(t)
	writeFile(t, settingsFile(t), "{\"unusedTTL\":\"30d\"}\n{\"extra\":1}\n", 0o600)
	_, _, err := loadSettings()
	if err == nil || !strings.Contains(err.Error(), "trailing JSON data") {
		t.Fatalf("want trailing JSON error, got %v", err)
	}
}

func TestLoadSettingsRejectsBadTTL(t *testing.T) {
	sandbox(t)
	for _, ttl := range []string{"0d", "nope", ""} {
		setTTL(t, ttl)
		if _, _, err := loadSettings(); err == nil {
			t.Fatalf("ttl %q must be rejected", ttl)
		}
	}
}

func configTestMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %o, want %o", path, got, want)
	}
}

func TestCmdConfigSetCreatesRestrictedDirs(t *testing.T) {
	sandbox(t)
	if err := cmdConfig(&globals{}, []string{"set", "unusedTTL", "7d"}); err != nil {
		t.Fatalf("cmdConfig set: %v", err)
	}
	s, _, err := loadSettings()
	if err != nil || s.UnusedTTL != "7d" {
		t.Fatalf("loadSettings = (%+v,%v), want 7d", s, err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	p := settingsFile(t)
	configTestMode(t, filepath.Dir(p), 0o700)
	configTestMode(t, p, 0o600)
}

func TestCmdConfigJSONShape(t *testing.T) {
	sandbox(t)
	setTTL(t, "12d")
	out := captureStdout(t, func() {
		if err := cmdConfig(&globals{json: true}, nil); err != nil {
			t.Errorf("cmdConfig: %v", err)
		}
	})
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(m) != 2 {
		t.Fatalf("JSON keys = %v, want exactly path and unusedTTL", m)
	}
	if m["path"] != settingsFile(t) {
		t.Fatalf("path = %v, want %q", m["path"], settingsFile(t))
	}
	if m["unusedTTL"] != "12d" {
		t.Fatalf("unusedTTL = %v, want 12d", m["unusedTTL"])
	}
}

func TestCmdConfigUsageError(t *testing.T) {
	sandbox(t)
	for _, args := range [][]string{{"bogus"}, {"set", "unusedTTL"}, {"set", "foo", "1d"}, {"a", "b", "c", "d"}} {
		var ue usageError
		if err := cmdConfig(&globals{}, args); !errors.As(err, &ue) {
			t.Fatalf("cmdConfig(%v) = %v, want usageError", args, err)
		}
	}
	// Invalid TTL is a usage error (exit 2): rejected before any write.
	if err := cmdConfig(&globals{}, []string{"set", "unusedTTL", "0d"}); !isUsage(err) {
		t.Fatalf("invalid TTL = %v, want usageError", err)
	}
	if exists(settingsFile(t)) {
		t.Fatal("invalid TTL must not create a settings file")
	}
}

// TestCmdConfigSetPreservesUnknownKey pins Fix G: a `config set` round-trip
// must not drop forward-compat keys already present in settings.json.
func TestCmdConfigSetPreservesUnknownKey(t *testing.T) {
	sandbox(t)
	writeFile(t, settingsFile(t), "{\"unusedTTL\":\"30d\",\"futureKey\":true}\n", 0o600)
	if err := cmdConfig(&globals{}, []string{"set", "unusedTTL", "7d"}); err != nil {
		t.Fatalf("cmdConfig set: %v", err)
	}
	raw, err := os.ReadFile(settingsFile(t))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("settings not JSON: %v\n%s", err, raw)
	}
	if _, ok := m["futureKey"]; !ok {
		t.Fatalf("unknown key dropped:\n%s", raw)
	}
	var ttl string
	if err := json.Unmarshal(m["unusedTTL"], &ttl); err != nil || ttl != "7d" {
		t.Fatalf("unusedTTL = %q, want 7d", ttl)
	}
}

func TestUnusedDuration(t *testing.T) {
	sandbox(t)
	assertDuration := func(want time.Duration) {
		t.Helper()
		got, err := unusedDuration()
		if err != nil {
			t.Fatalf("unusedDuration: %v", err)
		}
		if got != want {
			t.Fatalf("unusedDuration = %v, want %v", got, want)
		}
	}
	assertDuration(90 * 24 * time.Hour) // default when no settings file exists
	setTTL(t, "1d")
	assertDuration(24 * time.Hour)
	setTTL(t, "3650d")
	assertDuration(3650 * 24 * time.Hour)
	setTTL(t, "0d")
	if _, err := unusedDuration(); err == nil {
		t.Fatal("bad TTL must error")
	}
}
