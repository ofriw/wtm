//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lock_windows.go — the exclusive OS lock behind ~/.wtm stores on Windows.

// lockFile takes an exclusive lock on one byte of the file. Without
// LOCKFILE_FAIL_IMMEDIATELY the call blocks until the lock is available, which
// matches flock's blocking behavior on unix.
func lockFile(f *os.File) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &windows.Overlapped{})
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, &windows.Overlapped{})
}
