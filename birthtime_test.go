package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// birthtime_test.go — the AGE fact beyond the worktree wiring: the platform
// helper and the mtime-independence that fixes the reported young-age bug.

// TestWorktreeCreatedIgnoresGitMtime pins the bug this contract fixes: a main
// worktree's .git is a directory that git re-stamps on every index write, so a
// young mtime must never become AGE.
func TestWorktreeCreatedIgnoresGitMtime(t *testing.T) {
	t.Setenv(createdAtEnv, "")
	_ = os.Unsetenv(createdAtEnv)
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.Mkdir(gitDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A future stamp cannot lower a birth time, so this isolates the mtime
	// read: on APFS an mtime set before the birth time rewrites the birth.
	stamp := time.Now().Add(365 * 24 * time.Hour)
	if err := os.Chtimes(gitDir, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	got := worktreeCreated(dir)
	if got.Equal(stamp) {
		t.Fatalf("worktreeCreated read the .git mtime %v", stamp)
	}
	if birth := fileBirthTime(gitDir); !birth.IsZero() && !got.Equal(birth) {
		t.Errorf("worktreeCreated = %v, want the birth time %v", got, birth)
	}
}

// TestFileBirthTime pins the platform helper contract: a missing path has no
// mark, and changing mtime never moves the birth time. Filesystems that record
// no birth time (some Linux mounts, older kernels) skip the positive checks.
func TestFileBirthTime(t *testing.T) {
	dir := t.TempDir()
	if !fileBirthTime(filepath.Join(dir, "absent")).IsZero() {
		t.Error("a missing path must have no birth time")
	}

	path := filepath.Join(dir, "stamped")
	writeFile(t, path, "x", 0o644)
	birth := fileBirthTime(path)
	if birth.IsZero() {
		t.Skip("filesystem records no birth time")
	}
	if d := time.Since(birth); d < -time.Minute || d > 10*time.Minute {
		t.Errorf("birth time %v is not near now (%v)", birth, d)
	}
	// A future stamp cannot lower a birth time; APFS lowers it when mtime is
	// set before birth, which is not a git write pattern.
	stamp := time.Now().Add(365 * 24 * time.Hour)
	if err := os.Chtimes(path, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	if got := fileBirthTime(path); !got.Equal(birth) {
		t.Errorf("birth time moved with mtime: %v -> %v", birth, got)
	}
}

// TestWarnCreatedAtEnv pins the test-seam warning: malformed values are
// disclosed once at dispatch, before any renderer, and stay silent elsewhere.
func TestWarnCreatedAtEnv(t *testing.T) {
	t.Setenv(createdAtEnv, "nope")
	warned := captureStderr(t, func() {
		_ = dispatch("bogus", &globals{color: colorAuto, progress: progressAuto}, nil)
	})
	if n := strings.Count(warned, `WTM_CREATED_AT "nope"`); n != 1 {
		t.Errorf("disclosures = %d, want 1: %q", n, warned)
	}
	t.Setenv(createdAtEnv, "2020-01-01T00:00:00Z")
	if silent := captureStderr(t, warnCreatedAtEnv); silent != "" {
		t.Errorf("a valid seam must stay silent: %q", silent)
	}
}
