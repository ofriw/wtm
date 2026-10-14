//go:build windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Match the production lock's byte range, but do not wait for the holder.
func nonblockingTempFileLock(file *os.File) error {
	return windows.LockFileEx(windows.Handle(file.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0, 1, 0, &windows.Overlapped{})
}

func isTempLockContention(err error) bool {
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION)
}
