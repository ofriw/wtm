//go:build linux

package main

import (
	"time"

	"golang.org/x/sys/unix"
)

// fileBirthTime reports the filesystem creation time. statx is the only Linux
// interface that exposes it; kernels below 4.11 and filesystems that do not
// record it return no mark (masked bit clear).
func fileBirthTime(path string) time.Time {
	var st unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, path, 0, unix.STATX_BTIME, &st); err != nil {
		return time.Time{}
	}
	if st.Mask&unix.STATX_BTIME == 0 {
		return time.Time{}
	}
	return time.Unix(st.Btime.Sec, int64(st.Btime.Nsec))
}
