package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func strictActivityFixture(t *testing.T) (string, tempStore) {
	t.Helper()
	sandbox(t)
	repo := initRepo(t, "main")
	writeFile(t, filepath.Join(repo, "tracked"), "initial", 0o600)
	gitCommit(t, repo, "initial", "tracked")
	return repo, tempStore{repo: {CreatedAt: time.Unix(1, 0), TTL: "1h"}}
}

// idleActivityFixture backdates the checkout behind the fixture's 2020 commit
// so the candidate is genuinely idle-expired: only then can a test tell an
// unverified-session block apart from errTempActive.
func idleActivityFixture(t *testing.T) (string, tempStore) {
	t.Helper()
	repo, store := strictActivityFixture(t)
	old := time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC)
	for _, p := range []string{repo, filepath.Join(repo, ".git"), filepath.Join(repo, "tracked")} {
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	backdateActivityIndex(t, repo, old)
	return repo, store
}

// corruptSession plants an unattributable session file and pins its mtime, so
// freshness relative to the candidate's known activity is deterministic.
func corruptSession(t *testing.T, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	p := mkSession(t, wtPath, dirName, mtime)
	writeFile(t, p, "{broken json}\n", 0o600)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// requireActivityError runs the idle probe and pins fail-closed behavior: a
// failing probe keeps the checkout. The returned error lets callers classify
// the failure without matching internal wording.
func requireActivityError(t *testing.T, repo string, store tempStore, now time.Time) error {
	t.Helper()
	_, err := verifyTempIdle(store, worktree{Path: repo}, now)
	if err == nil {
		t.Fatal("activity verification accepted a failing probe")
	}
	if _, statErr := os.Stat(repo); statErr != nil {
		t.Fatalf("activity verification removed checkout: %v", statErr)
	}
	return err
}

// requireProbeError pins an ordinary activity failure: neither outcome gc
// classifies specially, so gc reports it as a tooling error.
func requireProbeError(t *testing.T, repo string, store tempStore, now time.Time) {
	t.Helper()
	if err := requireActivityError(t, repo, store, now); errors.Is(err, errTempActive) || errors.Is(err, errTempUnverified) {
		t.Fatalf("activity error = %v; want an ordinary probe failure", err)
	}
}

// requireUnverifiedSessionError pins the sentinel that makes gc disclose an
// unattributable session instead of treating it as a tooling failure.
func requireUnverifiedSessionError(t *testing.T, repo string, store tempStore, now time.Time) {
	t.Helper()
	if err := requireActivityError(t, repo, store, now); !errors.Is(err, errTempUnverified) {
		t.Fatalf("activity error = %v; want unverified-session block", err)
	}
}

func TestStrictActivityAcceptsDayTTL(t *testing.T) {
	repo, store := strictActivityFixture(t)
	rec := store[repo]
	rec.TTL = "7d"
	store[repo] = rec
	if _, err := verifyTempIdle(store, worktree{Path: repo}, tempNow); !errors.Is(err, errTempActive) {
		t.Fatalf("day TTL activity = %v; want ACTIVE", err)
	}
}

func TestStrictActivityRejectsGitLogFailure(t *testing.T) {
	repo, store := strictActivityFixture(t)
	mustGit(t, repo, "config", "log.showSignature", "invalid-boolean")
	requireProbeError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsGitFileListingFailure(t *testing.T) {
	repo, store := strictActivityFixture(t)
	mustGit(t, repo, "config", "core.excludesFile", filepath.Dir(repo))
	requireProbeError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsMissingChangedFile(t *testing.T) {
	repo, store := strictActivityFixture(t)
	if err := os.Remove(filepath.Join(repo, "tracked")); err != nil {
		t.Fatal(err)
	}
	requireProbeError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsInvalidSessionHeader(t *testing.T) {
	repo, store := idleActivityFixture(t)
	corruptSession(t, repo, "bad-header", tempNow)
	requireUnverifiedSessionError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsMissingSessionCWD(t *testing.T) {
	repo, store := idleActivityFixture(t)
	p := mkSession(t, repo, "no-cwd", tempNow)
	writeFile(t, p, "{}\n", 0o600)
	if err := os.Chtimes(p, tempNow, tempNow); err != nil {
		t.Fatal(err)
	}
	requireUnverifiedSessionError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsInvalidSessionsDirectory(t *testing.T) {
	repo, store := strictActivityFixture(t)
	base := filepath.Join(piAgentDir(), "sessions")
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	writeFile(t, base, "not a directory", 0o600)
	requireProbeError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsSessionWalkFailure(t *testing.T) {
	repo, store := strictActivityFixture(t)
	p := filepath.Dir(mkSession(t, repo, "unreadable", time.Unix(1, 0)))
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o700) })
	if _, err := os.ReadDir(p); err == nil {
		t.Skip("filesystem permits reading mode-000 directories")
	}
	requireProbeError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsSessionHeaderReadFailure(t *testing.T) {
	repo, store := idleActivityFixture(t)
	p := mkSession(t, repo, "unreadable-header", tempNow)
	if err := os.Chmod(p, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
	if f, err := os.Open(p); err == nil {
		_ = f.Close()
		t.Skip("filesystem permits reading mode-000 files")
	}
	requireUnverifiedSessionError(t, repo, store, tempNow)
}

func TestStrictActivityRejectsSessionStatFailure(t *testing.T) {
	repo, _ := strictActivityFixture(t)
	p := mkSession(t, repo, "removed", time.Unix(1, 0))
	idx, _, err := indexPiSessionsStrict()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := readWorktreeActivityStrict(repo, idx, tempNow); err == nil {
		t.Fatal("missing indexed session accepted")
	}
}

func TestStrictActivityAllowsAbsentSessionsDirectory(t *testing.T) {
	repo, _ := strictActivityFixture(t)
	if err := os.Remove(filepath.Join(piAgentDir(), "sessions")); err != nil {
		t.Fatal(err)
	}
	idx, unverified, err := indexPiSessionsStrict()
	if err != nil || len(idx) != 0 || len(unverified) != 0 {
		t.Fatalf("missing sessions = %v, %v, %v", idx, unverified, err)
	}
	if _, err := readWorktreeActivityStrict(repo, idx, tempNow); err != nil {
		t.Fatal(err)
	}
}

// A corrupt session older than the candidate's known activity cannot be its
// hidden recent use: verification returns it as stale for the gc caller to
// disclose and reclaims.
func TestStrictActivityIgnoresStaleUnrelatedCorruptSession(t *testing.T) {
	repo, store := idleActivityFixture(t)
	corruptSession(t, filepath.Join(t.TempDir(), "other"), "other", time.Unix(1, 0))
	stale, err := verifyTempIdle(store, worktree{Path: repo}, tempNow)
	if err != nil {
		t.Fatalf("stale corrupt session blocked reclaim: %v", err)
	}
	if len(stale) != 1 || !strings.Contains(stale[0], "other") {
		t.Fatalf("stale corrupt session passed silently: %v", stale)
	}
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("idle verification removed checkout: %v", err)
	}
}

// A fresh unattributable session in an unrelated project could still be the
// candidate's hidden activity: GC fails closed.
func TestStrictActivityRejectsFreshUnrelatedCorruptSession(t *testing.T) {
	repo, store := idleActivityFixture(t)
	corruptSession(t, filepath.Join(t.TempDir(), "other"), "other", tempNow)
	requireUnverifiedSessionError(t, repo, store, tempNow)
}
