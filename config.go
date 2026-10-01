package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const defaultUnusedDays = 90

type settings struct {
	UnusedTTL string `json:"unusedTTL"`
}

func settingsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wtm", "settings.json"), nil
}

func ttlDays(s string) (int, error) {
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

func decodeSettings(data []byte, p string) (settings, error) {
	var s settings
	dec := json.NewDecoder(bytes.NewReader(data))
	// Forward-compat: ignore unknown keys; trailing-data reject below still applies.
	if err := dec.Decode(&s); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return s, fmt.Errorf("%s: trailing JSON data", p)
	}
	if _, err := ttlDays(s.UnusedTTL); err != nil {
		return s, fmt.Errorf("%s: %w", p, err)
	}
	return s, nil
}

func loadSettings() (settings, string, error) {
	p, err := settingsPath()
	if err != nil {
		return settings{}, "", err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return settings{UnusedTTL: fmt.Sprintf("%dd", defaultUnusedDays)}, p, nil
	}
	if err != nil {
		return settings{}, p, err
	}
	s, err := decodeSettings(data, p)
	return s, p, err
}

func unusedDuration() (time.Duration, error) {
	s, _, err := loadSettings()
	if err != nil {
		return 0, err
	}
	days, err := ttlDays(s.UnusedTTL)
	return time.Duration(days) * 24 * time.Hour, err
}

func cmdConfigSet(val string) error {
	// Invalid TTL is user input, so it is a usage error (exit 2), not a write
	// failure: nothing on disk should change for a rejected value.
	if _, err := ttlDays(val); err != nil {
		return usagef("%v", err)
	}
	p, err := settingsPath()
	if err != nil {
		return err
	}
	// Preserve unknown keys: decode into a raw map so a `config set` round-trip
	// cannot drop forward-compat fields (decodeSettings already tolerates them).
	// A corrupt existing file is an explicit (exit 1) error, not a usage error.
	m := map[string]json.RawMessage{}
	if data, err := os.ReadFile(p); err == nil {
		if err := json.Unmarshal(data, &m); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	m["unusedTTL"] = raw
	// Map keys marshal sorted, so output is byte-stable even with unknown keys.
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, append(data, '\n'), 0600)
}

func cmdConfigGet(g *globals) error {
	s, p, err := loadSettings()
	if err != nil {
		return err
	}
	if g.json {
		return printJSON(struct {
			Path      string `json:"path"`
			UnusedTTL string `json:"unusedTTL"`
		}{p, s.UnusedTTL})
	}
	fmt.Printf("path: %s\nunusedTTL: %s\n", p, s.UnusedTTL)
	return nil
}

func cmdConfig(g *globals, args []string) error {
	fs := subFlags("config", g)
	pos, err := parseSub(fs, args)
	if err != nil {
		return err
	}
	if len(pos) == 3 && pos[0] == "set" && pos[1] == "unusedTTL" {
		return cmdConfigSet(pos[2])
	}
	if len(pos) != 0 {
		return usagef("config: use 'config' or 'config set unusedTTL <Nd>'")
	}
	return cmdConfigGet(g)
}
