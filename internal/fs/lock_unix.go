//go:build !windows

package fs

import (
	"os"

	"golang.org/x/sys/unix"
)

// lock_unix.go — the exclusive OS lock behind ~/.wtm stores on unix.

// LockFile takes an exclusive advisory lock, blocking until it is available.
// flock is per open file description, so unlocking/closing one fd never affects
// another process's lock.
func LockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_EX)
}

func UnlockFile(f *os.File) error {
	return unix.Flock(int(f.Fd()), unix.LOCK_UN)
}
