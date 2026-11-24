package cli

import (
	"encoding/json"
	"fmt"
	"os"

	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/ui"
)

// settingsPath resolves the SSOT settings file inside ~/.wtm.
func settingsPath() (string, error) { return fs.WtmFilePath(fs.SettingsFileName) }

// settingsLockPath resolves the settings write-lock file inside ~/.wtm.
func settingsLockPath() (string, error) { return fs.WtmFilePath(fs.SettingsLockFileName) }

func cmdConfigSet(val string) error {
	// Invalid TTL is user input, so it is a usage error (exit 2), not a write
	// failure: nothing on disk should change for a rejected value.
	if _, err := core.TTLDays(val); err != nil {
		return core.Usagef("%v", err)
	}
	p, err := settingsPath()
	if err != nil {
		return err
	}
	return writeSettings(p, val)
}

// writeSettings serializes the whole read-merge-write under the settings lock so
// concurrent writers cannot lose an update (read-then-replace). Reader safety is
// separate: reads share the handle for delete (openShared), so they never block
// the rename.
func writeSettings(p, val string) error {
	lock, err := settingsLockPath()
	if err != nil {
		return err
	}
	return fs.WithFileLock(lock, func() error {
		return writeSettingsLocked(p, val)
	})
}

// writeSettingsLocked merges unusedTTL into the settings map, preserving
// unknown keys so a round-trip cannot drop forward-compat fields
// (core.LoadSettings tolerates them). A corrupt existing file is an explicit
// (exit 1) error, not a usage one. Callers must hold the settings lock.
func writeSettingsLocked(p, val string) error {
	m, err := readSettingsMapUnlocked(p)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	m[core.UnusedTTLKey] = raw
	// Map keys marshal sorted, so output is byte-stable even with unknown keys.
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// fs.WriteFileAtomic creates the directory itself, so no MkdirAll here: a
	// truncated settings file would break every command that reads it.
	return fs.WriteFileAtomic(p, append(data, '\n'), 0o600)
}

func readSettingsMapUnlocked(p string) (map[string]json.RawMessage, error) {
	m := map[string]json.RawMessage{}
	if data, err := fs.ReadFileShared(p); err == nil {
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	// A top-level JSON null unmarshals into a nil map without error; assigning
	// to it panics, so a corrupt file must still surface as an explicit error.
	if m == nil {
		return nil, fmt.Errorf("%s: want a settings object", p)
	}
	return m, nil
}

func cmdConfigGet(g *core.Globals) error {
	s, p, err := core.LoadSettings()
	if err != nil {
		return err
	}
	if g.JSON {
		// core.Settings is embedded so the json tag stays single-owner; the
		// promoted field keeps the exact key spelling.
		return ui.PrintJSON(struct {
			Path string `json:"path"`
			core.Settings
		}{p, s})
	}
	fmt.Printf("path: %s\nunusedTTL: %s\n", p, s.UnusedTTL)
	return nil
}

func cmdConfig(g *core.Globals, args []string) error {
	flags := core.SubFlags("config", g)
	pos, err := core.ParseSub(flags, g, args, usage)
	if err != nil {
		return err
	}
	if len(pos) == 3 && pos[0] == "set" && pos[1] == "unusedTTL" {
		return cmdConfigSet(pos[2])
	}
	if len(pos) != 0 {
		return core.Usagef("config: use 'config' or 'config set unusedTTL <Nd>'")
	}
	return cmdConfigGet(g)
}
