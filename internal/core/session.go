package core

import (
	"bufio"
	"fmt"
	"os"
	"time"

	"wtm/internal/fs"
	"wtm/internal/ui"
)

// session.go — the agent-session domain model. It carries the types discovery
// and reclamation share; scanning and purge (which know the harness agents)
// live in internal/harness.

// SessionIndex maps a canonical worktree cwd to its session files. It is built
// once per command via indexing so discovery costs one walk per harness instead
// of one per worktree.
type SessionIndex map[string][]SessionRef

// SessionRef carries an owned file and the boundary for empty-dir cleanup.
type SessionRef struct {
	Path       string
	Group      string
	Root       string
	RootTarget string
	Owner      func(*bufio.Reader) (string, error)
}

// UnverifiedSession is unattributable activity. A zero age can hide any use;
// directory symlinks have zero age because appends do not change directory mtimes.
type UnverifiedSession struct {
	Path    string
	ModTime time.Time
}

// BlockRemoval reports whether this unverified session must block removal: a
// zero age can hide any use, and a session newer than known activity could be
// its hidden recent use.
func (u UnverifiedSession) BlockRemoval(knownNewest time.Time) bool {
	return u.ModTime.IsZero() || u.ModTime.After(knownNewest)
}

// SessionErrPolicy decides what an unreadable session does. Reclamation fails
// closed: an incomplete index can misclassify a worktree UNUSED and GC it.
// Read-only discovery warns and reports what it knows.
type SessionErrPolicy struct {
	FailClosed bool
	P          *ui.Progress
}

// SessionErr reports one unreadable session: fatal when FailClosed, else a
// warning through the progress handle (plain stderr when it is nil).
func (pol SessionErrPolicy) SessionErr(err error) error {
	if pol.FailClosed {
		return err
	}
	ui.WarnProgress(pol.P, "%v", err)
	return nil
}

// ErrTempUnverified blocks removal when an unverified session is newer than
// known activity — only such a file could be the candidate's hidden recent use.
var ErrTempUnverified = fmt.Errorf("unverified agent session is newer than known activity")

// SplitUnverifiedSessions separates unverified sessions: one newer than known
// activity could be the candidate's hidden recent use and blocks removal;
// older ones cannot change the verdict and are returned for the caller to
// disclose. The store owner owns the terminal, so no warnings here.
func SplitUnverifiedSessions(unverified []UnverifiedSession, knownNewest time.Time) ([]string, error) {
	var stale []string
	for _, u := range unverified {
		if u.BlockRemoval(knownNewest) {
			return nil, fmt.Errorf("%w: %s", ErrTempUnverified, u.Path)
		}
		stale = append(stale, u.Path)
	}
	return stale, nil
}

// LastUsed is the newest mtime among the worktree's session files (looked up
// in idx); zero when the worktree has never hosted a session.
func LastUsed(wtPath string, idx SessionIndex) time.Time {
	newest, _ := SessionLastUsed(wtPath, idx, false)
	return newest
}

// SessionLastUsed fails closed for fresh removal checks. Display can retain
// known activity, but must disclose every file it could not inspect.
func SessionLastUsed(wtPath string, idx SessionIndex, strict bool) (time.Time, error) {
	var newest time.Time
	for _, r := range idx[fs.Canonical(wtPath)] {
		mtime, err := SessionMtime(r.Path)
		if err != nil {
			if strict {
				return newest, err
			}
			ui.WarnProgress(nil, "%v", err)
		} else if mtime.After(newest) {
			newest = mtime
		}
	}
	return newest, nil
}

// An indexed transcript replaced with a link or directory is no longer verified.
func SessionMtime(p string) (time.Time, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat session %s: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("session is not a regular file: %s", p)
	}
	return info.ModTime(), nil
}
