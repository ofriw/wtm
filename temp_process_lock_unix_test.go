//go:build !windows

package main

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// A nonblocking probe proves contention before the pipe releases the holder.
func nonblockingTempFileLock(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
}

func isTempLockContention(err error) bool {
	return errors.Is(err, unix.EWOULDBLOCK)
}
