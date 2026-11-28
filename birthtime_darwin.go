//go:build darwin

package main

import (
	"os"
	"syscall"
	"time"
)

// fileBirthTime reports the filesystem creation time. Darwin records it in
// Stat_t.Birthtimespec; a path that cannot be stat'ed has no mark.
func fileBirthTime(path string) time.Time {
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok || (st.Birthtimespec.Sec == 0 && st.Birthtimespec.Nsec == 0) {
		return time.Time{}
	}
	return time.Unix(st.Birthtimespec.Sec, st.Birthtimespec.Nsec)
}
