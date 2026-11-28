package core

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"wtm/internal/fs"
)

// settings.go — the ~/.wtm/settings.json read/parse side. The config command
// handlers (which write) live in internal/cli; discovery needs the parsed TTL,
// so the read side lives here.

// defaultUnusedDays is the TTL when no settings file exists.
const defaultUnusedDays = 90

// UnusedTTLKey is the cli writer's key for the unused TTL. The Settings struct
// tag must spell it identically (a Go tag cannot reference a const); the pin
// test (settings_test.go) guards the two against drift.
const UnusedTTLKey = "unusedTTL"

// Settings is the persisted settings file model.
type Settings struct {
	UnusedTTL string `json:"unusedTTL"`
}

// defaultSettings is the SSOT for settings when no file exists.
func defaultSettings() Settings {
	return Settings{UnusedTTL: fmt.Sprintf("%dd", defaultUnusedDays)}
}

// TTLDays parses a "Nd" TTL string, enforcing the positive-whole-days and
// overflow bounds.
func TTLDays(s string) (int, error) {
	if !strings.HasSuffix(s, "d") || len(s) < 2 {
		return 0, fmt.Errorf("unusedTTL must be positive whole days (e.g. 90d)")
	}
	digits := strings.TrimSuffix(s, "d")
	for _, c := range digits {
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("unusedTTL must be positive whole days (e.g. 90d)")
		}
	}
	n, err := strconv.Atoi(digits)
	// WHY: day bound keeps days*24h inside int64 (overflow-safe).
	if err != nil || n < 1 || n > int((1<<63-1)/int64(24*time.Hour)) {
		return 0, fmt.Errorf("unusedTTL out of range: %q", s)
	}
	return n, nil
}

func decodeSettings(data []byte, p string) (Settings, error) {
	var s Settings
	dec := json.NewDecoder(bytes.NewReader(data))
	// Forward-compat: ignore unknown keys; trailing-data reject below still applies.
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return s, fmt.Errorf("%s: trailing JSON data", p)
	}
	if _, err := TTLDays(s.UnusedTTL); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

// LoadSettings reads settings.json. The shared read handle lets a concurrent
// `config set` atomically replace the file (open_windows.go); POSIX rename
// already preserves open handles.
func LoadSettings() (Settings, string, error) {
	p, err := fs.WtmFilePath(fs.SettingsFileName)
	if err != nil {
		return Settings{}, "", err
	}
	data, err := fs.ReadFileShared(p)
	if os.IsNotExist(err) {
		return defaultSettings(), p, nil
	}
	if err != nil {
		return Settings{}, p, err
	}
	s, err := decodeSettings(data, p)
	return s, p, err
}

// UnusedDuration is the resolved global unused TTL.
func UnusedDuration() (time.Duration, error) {
	s, _, err := LoadSettings()
	if err != nil {
		return 0, err
	}
	days, err := TTLDays(s.UnusedTTL)
	return time.Duration(days) * 24 * time.Hour, err
}
