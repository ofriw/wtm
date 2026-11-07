//go:build windows

package main

import "os"

// userTempDirName returns the per-user temp dir component. Windows resolves
// os.TempDir() inside the user's profile, so no UID suffix is needed.
func userTempDirName() string { return "wtm" }

// privateFallbackDir leaves ownership to the per-user temp directory ACL.
func privateFallbackDir(os.FileInfo) bool { return true }
