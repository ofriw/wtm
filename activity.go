package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// activity.go — active-time and reclaim predicates: the worktree activity
// methods, the permanent-worktree safety guards and the strict/lenient session
// activity scans shared by status and gc.

// unused reports whether gc may reclaim a worktree: a temp one past its own
// idle window, a permanent one past the global unusedTTL. It is the single
// candidate predicate for status and gc.
func (w worktree) unused(now time.Time, ttl time.Duration) bool {
	if w.Temp {
		return tempExpired(w.TempCreated, w.LastUsed, w.TempTTL, now)
	}
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
}

// tempExpiresAt is the deadline a temp worktree is measured against.
// tempDeadline owns the formula. Zero for a permanent worktree.
func (w worktree) tempExpiresAt() time.Time {
	if !w.Temp {
		return time.Time{}
	}
	return tempDeadline(w.TempCreated, w.LastUsed, w.TempTTL)
}

func (w worktree) status(now time.Time, ttl time.Duration) string {
	if w.unused(now, ttl) {
		return "UNUSED"
	}
	return "ACTIVE"
}

// Permanent cleanup has no fresh idle probe. Missing ownership therefore keeps
// every permanent candidate active; temp candidates use the locked fresh probe.
// When all unverified sessions have known ages, protection scopes to permanent
// worktrees older than the newest unverified session. A single unknown-age session
// preserves the original fail-closed contract.
func guardPermanentActivity(wts []worktree, unknown []unverifiedSession, now time.Time, p *progress) {
	if len(unknown) == 0 {
		return
	}
	for _, u := range unknown {
		warnProgress(p, "unverified session ownership: %s", u.Path)
	}
	if hasUnknownAgeSession(unknown) {
		keepPermanentsActive(wts, now, p)
		return
	}
	keepPermanentsOlderThan(wts, newestKnownSession(unknown), now, p)
}

// keepPermanentsActive is the blanket fail-closed guard: one unknown-age
// session grants every permanent worktree fresh activity, so gc cannot prove
// any of them abandoned.
func keepPermanentsActive(wts []worktree, now time.Time, p *progress) {
	for i := range wts {
		if !wts[i].Temp {
			wts[i].LastUsed = now
			warnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

// keepPermanentsOlderThan applies the same age gate as
// unverifiedSession.blocksRemoval: only a session newer than known activity can
// hide recent use. The equality edge differs by intent — this guard protects on
// == (fail closed for permanents), the temp path removes on strict >.
func keepPermanentsOlderThan(wts []worktree, newest, now time.Time, p *progress) {
	for i := range wts {
		if !wts[i].Temp && !wts[i].LastUsed.After(newest) {
			wts[i].LastUsed = now
			warnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

func hasUnknownAgeSession(unknown []unverifiedSession) bool {
	for _, u := range unknown {
		if u.ModTime.IsZero() {
			return true
		}
	}
	return false
}

func newestKnownSession(unknown []unverifiedSession) time.Time {
	var newest time.Time
	for _, u := range unknown {
		if u.ModTime.After(newest) {
			newest = u.ModTime
		}
	}
	return newest
}

func updateNewest(target *time.Time, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("activity stat %s: %w", path, err)
	}
	if info.ModTime().After(*target) {
		*target = info.ModTime()
	}
	return nil
}

func scanGitModTimes(path string, newest *time.Time, strict bool) error {
	if err := scanIndexTime(path, newest); err != nil && strict {
		return err
	}
	if err := scanCommitTime(path, newest); err != nil && strict {
		return err
	}
	return scanChangedFileTimes(path, newest, strict)
}

func scanCommitTime(path string, newest *time.Time) error {
	out, err := git(path, "log", "-1", "--format=%ct")
	if err != nil {
		return err
	}
	s, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return fmt.Errorf("parse git commit time: %w", err)
	}
	if commit := time.Unix(s, 0); commit.After(*newest) {
		*newest = commit
	}
	return nil
}

// changedActivityFiles includes staged files but not staged deletions: their
// activity survives in the private index, while missing unstaged files fail closed.
func changedActivityFiles(path string) (string, error) {
	out, err := git(path, "--no-optional-locks", "ls-files", "-m", "-o", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	staged, err := git(path, "--no-optional-locks", "diff", "--cached", "--name-only", "--diff-filter=d", "-z")
	return out + "\x00" + staged, err
}

func scanChangedFileTimes(path string, newest *time.Time, strict bool) error {
	out, err := changedActivityFiles(path)
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(out, "\x00") {
		if rel != "" && !strings.HasPrefix(rel, ".chunkhound") {
			if err := updateNewest(newest, filepath.Join(path, rel)); err != nil && strict {
				// A vanished changed file cannot be checked for activity: fail
				// closed and say how the user resolves the resulting deadlock.
				// errors.Is, not os.IsNotExist: updateNewest wraps with %w.
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w; commit or restore the deleted file, or remove the checkout explicitly", err)
				}
				return err
			}
		}
	}
	return nil
}

// freshWorktreeActivity refreshes both sources under a fresh session index
// at now; discovery's cached index cannot authorize removal. Errors are fatal:
// removal must fail closed when activity is unreadable. Unverified sessions
// ride along so the caller can scope them per candidate instead of failing
// globally.
func freshWorktreeActivity(path string, now time.Time) (time.Time, []unverifiedSession, error) {
	idx, unverified, err := indexSessionsStrict(nil)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("refresh agent sessions: %w", err)
	}
	last, err := readWorktreeActivityStrict(path, idx, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	return last, unverified, nil
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first agent session.
// nowFunc() is the WTM_NOW test seam: in the container harness it is pinned to a
// fixed time so goldens stay byte-stable. Future mtimes are clamped to that same
// now so a session newer than the test clock cannot break determinism.
func worktreeLastUsed(path string, idx sessionIndex, p *progress) time.Time {
	newest, _ := readWorktreeActivityLenient(path, idx, nowFunc(), p)
	return newest
}

// readWorktreeActivityLenient collects activity timestamps relative to now,
// ignoring errors. Used for display where failure must not block output.
func readWorktreeActivityLenient(path string, idx sessionIndex, now time.Time, p *progress) (time.Time, error) {
	return collectActivity(path, idx, now, false, p)
}

// readWorktreeActivityStrict collects activity timestamps relative to now,
// failing on any error. Used immediately before idle-based removal where
// unreadable activity must prevent reclamation. Strict never warns, so it
// needs no progress handle.
func readWorktreeActivityStrict(path string, idx sessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, true, nil)
}

// collectActivity merges agent sessions, the checkout backlink and Git
// modification times, clamping any future timestamp to now.
func collectActivity(path string, idx sessionIndex, now time.Time, strict bool, p *progress) (time.Time, error) {
	newest, err := sessionLastUsed(path, idx, strict, p)
	if err != nil {
		return newest, err
	}
	if err := updateNewest(&newest, filepath.Join(path, ".git")); err != nil && strict {
		return newest, err
	}
	if err := scanGitModTimes(path, &newest, strict); err != nil && strict {
		return newest, err
	}
	if newest.After(now) {
		return now, nil
	}
	return newest, nil
}

// The private index records staging, including deletion with no remaining
// file. Read-only Git queries must not refresh it and restart the idle window.
func scanIndexTime(path string, newest *time.Time) error {
	dir, err := gitPrivateDir(path)
	if err != nil {
		return err
	}
	return updateNewest(newest, filepath.Join(dir, "index"))
}
