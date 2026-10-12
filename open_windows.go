//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// openShared opens path for reading with FILE_SHARE_DELETE. Go's os.Open grants
// only FILE_SHARE_READ|FILE_SHARE_WRITE, so an ordinary reader handle blocks
// MoveFileEx (os.Rename) with ERROR_ACCESS_DENIED. Store readers must not block
// the atomic replace that keeps settings.json and temp.json consistent.
func openShared(path string) (*os.File, error) {
	name, err := windowsPath(path)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	h, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(h), path), nil
}

// maxPathWithoutPrefix is MAX_PATH (260) minus the 8.3 name reserve, matching
// os.fixLongPath: shorter paths need no extended prefix.
const maxPathWithoutPrefix = 248

// windowsPath returns the UTF-16 name for CreateFile, extended-length prefixed
// (\\?\) only when the absolute path may exceed MAX_PATH. Go's os package does
// the same internally; a custom CreateFile must repeat it because the prefix is
// what lifts the limit.
func windowsPath(path string) (*uint16, error) {
	abs := path
	if !filepath.IsAbs(abs) {
		var err error
		if abs, err = filepath.Abs(path); err != nil {
			return nil, err
		}
	}
	if len(abs) >= maxPathWithoutPrefix {
		abs = extendedPath(abs)
	}
	return windows.UTF16PtrFromString(abs)
}

// extendedPath adds the \\?\ prefix (or \\?\UNC\ for shares) so CreateFile
// accepts paths beyond MAX_PATH. FullPath normalizes separators and resolves
// relative components, which the prefix requires: \\?\ disables both. A
// FullPath failure falls back to the raw path and lets CreateFile report the
// error: the fallback never hides a failure, it only preserves a chance to
// succeed where the process is long-path aware.
func extendedPath(path string) string {
	abs, err := windows.FullPath(path)
	if err != nil {
		return path
	}
	if strings.HasPrefix(abs, `\\?\`) || strings.HasPrefix(abs, `\\.\`) {
		return abs
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + abs[2:]
	}
	return `\\?\` + abs
}
