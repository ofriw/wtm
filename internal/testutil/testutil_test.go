package testutil

import (
	"os"
	"testing"
)

// testutil_test.go — hermetic fixtures shared by every Tier-1/Tier-3 test.
// No mocks: helpers drive the real git binary, the real filesystem and the
// real wtm binary. Tests must not call t.Parallel when they mutate env
// (t.Setenv/t.Chdir forbid it).

// TestMain builds the wtm binary ONCE before any test can sandbox HOME (a
// sandboxed HOME would relocate GOMODCACHE into a read-only temp module cache
// and break TempDir cleanup). Tier-1 tests simply ignore the binary.
func TestMain(m *testing.M) {
	buildWTMOnce()
	code := m.Run()
	if wtmBuildDir != "" {
		_ = os.RemoveAll(wtmBuildDir)
	}
	os.Exit(code)
}
