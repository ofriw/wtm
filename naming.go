package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// naming.go — the single source of truth for branch and worktree directory
// names. Branches keep their semantic, taxonomy-prefixed identity (feature/x);
// directories are project-prefixed slugs placed next to the current worktree,
// so a worktree is never nested inside another and never inherits the branch's
// slashes.

// maxBranchLen follows the 80-char convention that keeps branch names within
// Windows path limits and readable in CI.
const maxBranchLen = 80

// tempBranchPrefix marks an ephemeral branch that gc may reclaim once idle.
const tempBranchPrefix = "tmp/"

// branchPrefixes is the taxonomy the warn-only convention check recognizes.
var branchPrefixes = []string{
	tempBranchPrefix,
	"feature/", "fix/", "hotfix/", "chore/", "refactor/",
	"docs/", "test/", "perf/", "ci/", "release/",
}

func cleanSegments(segs []string, raw string) ([]string, error) {
	for i, s := range segs {
		s = trimDots(trimHyphens(s))
		if s == "" || strings.HasSuffix(s, ".lock") {
			return nil, fmt.Errorf("branch %q is not a valid git ref", raw)
		}
		segs[i] = s
	}
	return segs, nil
}

// normalizeBranch canonicalizes user input to a valid, conventional git branch:
// lowercase, hyphen-separated segments, slashes preserved as taxonomy
// separators. Invalid ref characters are rejected rather than silently dropped.
// Lowercasing collides names that differ only by case (Feature/X vs feature/x
// resolve to the same branch), so the normalized form is the branch identity.
func normalizeBranch(raw string) (string, error) {
	b := strings.ToLower(strings.TrimSpace(raw))
	if b == "" {
		return "", errors.New("branch name is required")
	}
	b = strings.NewReplacer("_", "-", " ", "-").Replace(b)
	if bad, ok := badBranchRune(b); !ok {
		return "", fmt.Errorf("branch %q contains unsupported character %q", raw, bad)
	}
	// @{ opens revision syntax (HEAD@{yesterday}); git rejects it in refnames.
	if strings.Contains(b, "@{") {
		return "", fmt.Errorf("branch %q is not a valid git ref: @{ is revision syntax", raw)
	}
	// '@' alone is not a branch name to git (it resolves the HEAD shorthand).
	if b == "@" {
		return "", fmt.Errorf("branch %q is not a valid git ref: @ has no name", raw)
	}
	segs, err := cleanSegments(strings.Split(b, "/"), raw)
	if err != nil {
		return "", err
	}
	b = strings.Join(segs, "/")
	if len(b) > maxBranchLen {
		return "", fmt.Errorf("branch %q exceeds %d characters", b, maxBranchLen)
	}
	return b, nil
}

// badBranchRune returns the first character outside the allowed git-ref
// alphabet (a-z, 0-9, '-', '.', '/', '@'), if any. '@' alone is legal
// (feature/@scope/auth); the '@{' revision sequence is rejected separately in
// normalizeBranch.
func badBranchRune(b string) (rune, bool) {
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '/', r == '@':
		default:
			return r, false
		}
	}
	return 0, true
}

// trimHyphens collapses runs of '-' and trims the ends; a leading run cannot
// survive because git refs may not start a path component with '-'.
func trimHyphens(s string) string { return trimRuns(s, '-') }

// trimDots collapses runs of '.' and trims the ends, so "a..b" cannot produce
// the invalid ".." sequence.
func trimDots(s string) string { return trimRuns(s, '.') }

func trimRuns(s string, sep rune) string {
	var out strings.Builder
	prev := false
	for _, r := range s {
		if r == sep {
			if prev || out.Len() == 0 {
				continue
			}
			prev = true
		} else {
			prev = false
		}
		out.WriteRune(r)
	}
	return strings.TrimRight(out.String(), string(sep))
}

// tempBranch prefixes a requested name with tmp/ unless it is already
// prefixed, so `add --temp` is idempotent on an already-temp name. The prefix
// is applied before the length check, since it is part of the branch identity.
func tempBranch(raw string) (string, error) {
	b, err := normalizeBranch(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(b, tempBranchPrefix) {
		b = tempBranchPrefix + b
	}
	if len(b) > maxBranchLen {
		return "", fmt.Errorf("branch %q exceeds %d characters", b, maxBranchLen)
	}
	return b, nil
}

// slugify flattens a branch into a filesystem-safe directory component: every
// separator becomes '-', so feature/login -> feature-login.
func slugify(branch string) string {
	var out strings.Builder
	prev := false
	for _, r := range strings.ToLower(branch) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			prev = false
			continue
		}
		if !prev && out.Len() > 0 {
			out.WriteByte('-')
			prev = true
		}
	}
	return strings.Trim(out.String(), "-")
}

// projectName is the stable prefix shared by every sibling worktree: the main
// worktree's directory name. Deriving it from the main worktree (not the
// current one) keeps names stable when a worktree is created from a linked one.
func projectName(wts []gworktree) string {
	if len(wts) == 0 {
		return "worktree"
	}
	base := filepath.Base(wts[0].Path)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "worktree"
	}
	return base
}

// deriveDirName builds the sibling directory name from the project prefix and
// the branch slug: repo + feature/login -> repo-feature-login.
func deriveDirName(project, branch string) string {
	return project + "-" + slugify(branch)
}

// nestedIn reports the existing worktree that contains target (or equals it).
// Worktrees must be siblings: a nested checkout makes IDE watchers, linters and
// test runners double-scan, and breaks discovery assumptions.
func nestedIn(target string, wts []gworktree) (string, bool) {
	for _, w := range wts {
		p := canonical(w.Path)
		if target == p || strings.HasPrefix(target, p+string(filepath.Separator)) {
			return p, true
		}
	}
	return "", false
}

// warnBranchConvention discloses a branch without a recognized type prefix.
// It is a warning, not a rejection: conventions drive CI routing and search,
// but the user may have a deliberate reason to skip them.
func warnBranchConvention(branch string) {
	for _, p := range branchPrefixes {
		if strings.HasPrefix(branch, p) {
			return
		}
	}
	warnf("branch %q has no type prefix (e.g. feature/…); continuing", branch)
}
