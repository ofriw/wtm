//go:build !windows

package fs

import "os"

// OpenShared opens path for reading. POSIX rename(2) and unlink preserve open
// handles, so a reader can never block an atomic replace (open_windows.go).
func OpenShared(path string) (*os.File, error) {
	return os.Open(path)
}
