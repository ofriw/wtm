//go:build !windows

package fs

import "os"

// RenameReplace atomically replaces newname with oldname. POSIX rename(2)
// preserves open handles, so the reader/writer race cannot occur and no
// fallback is needed (rename_windows.go).
func RenameReplace(oldname, newname string) error {
	return os.Rename(oldname, newname)
}
