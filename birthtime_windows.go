//go:build windows

package main

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime reports the filesystem creation time. Windows stamps every
// entry with one, so it is the worktree creation fact on this platform.
func fileBirthTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || (data.CreationTime.LowDateTime == 0 && data.CreationTime.HighDateTime == 0) {
		return time.Time{}
	}
	return time.Unix(0, data.CreationTime.Nanoseconds())
}
