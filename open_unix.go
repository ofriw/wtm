//go:build !windows

package main

import "os"

// openShared opens path for reading. POSIX rename(2) and unlink preserve open
// handles, so a reader can never block an atomic replace (open_windows.go).
func openShared(path string) (*os.File, error) {
	return os.Open(path)
}
