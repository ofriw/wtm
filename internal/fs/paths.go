package fs

import (
	"os"
	"path/filepath"
)

// WtmDir returns the per-user wtm state directory (~/.wtm).
func WtmDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wtm"), nil
}

// Settings store basenames — the layout SSOT, so the reader (core) and the
// writer (cli) cannot drift.
const SettingsFileName, SettingsLockFileName = "settings.json", "settings.lock"

// Temp store basenames — part of the same layout SSOT; the temp package's
// readers and writers resolve them through these constants.
const TempFileName, TempLockFileName = "temp.json", "temp.lock"

// WtmFilePath resolves a file directly inside ~/.wtm — the SSOT for the store
// layout shared by settings.json and temp.json and their lock files.
func WtmFilePath(name string) (string, error) {
	dir, err := WtmDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}
