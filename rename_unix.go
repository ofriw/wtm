//go:build !windows

package main

import "os"

// renameReplace atomically replaces newname with oldname. POSIX rename(2)
// preserves open handles, so the reader/writer race cannot occur and no
// fallback is needed (rename_windows.go).
func renameReplace(oldname, newname string) error {
	return os.Rename(oldname, newname)
}
