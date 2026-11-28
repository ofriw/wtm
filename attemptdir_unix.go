//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

// userTempDirName scopes fallback markers to one user in a shared temp dir.
func userTempDirName() string { return fmt.Sprintf("wtm-%d", os.Getuid()) }

// privateFallbackDir requires owner-only permissions and current-user
// ownership on Unix. A foreign-owned 0700 dir would make every marker write
// fail and its contents unreadable, so the cooldown silently disappears.
func privateFallbackDir(info os.FileInfo) bool {
	if info.Mode().Perm() != 0o700 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == uint32(os.Getuid())
}
