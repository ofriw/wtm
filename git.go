package main

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// git.go — the only file that shells out to git.

// Pinned parser sample (git 2.50 `worktree list --porcelain`): records are
// separated by blank lines, fields by a single space. The main worktree is
// listed first (worktree-list docs), linked worktrees follow path-sorted.
//
//	worktree /repo/main
//	HEAD 0123456789012345678901234567890123456789
//	branch refs/heads/main
//
//	worktree /repo/detached
//	HEAD 0123456789012345678901234567890123456789
//	detached
//
//	worktree /repo/locked
//	HEAD 0123456789012345678901234567890123456789
//	branch refs/heads/locked
//	locked user reason
type gworktree struct {
	Path     string
	Branch   string // "" when detached or bare
	Bare     bool
	Locked   bool
	Prunable bool
	Main     bool
}

func applyPorcelainField(cur *gworktree, key, val string) {
	if cur == nil {
		return
	}
	switch key {
	case "branch":
		cur.Branch = strings.TrimPrefix(val, "refs/heads/")
	case "bare":
		cur.Bare = true
	case "locked":
		cur.Locked = true
	case "prunable":
		cur.Prunable = true
	}
}

func parsePorcelainLine(line string, cur **gworktree, out *[]gworktree) {
	line = strings.TrimRight(line, "\r")
	if line == "" {
		if *cur != nil {
			*out = append(*out, **cur)
			*cur = nil
		}
		return
	}
	key, val, _ := strings.Cut(line, " ")
	if key == "worktree" {
		if *cur != nil {
			*out = append(*out, **cur)
		}
		*cur = &gworktree{Path: val}
		return
	}
	applyPorcelainField(*cur, key, val)
}

// parseWorktrees is pure so the porcelain format lives here once.
func parseWorktrees(porcelain string) []gworktree {
	var out []gworktree
	var cur *gworktree
	for _, line := range strings.Split(porcelain, "\n") {
		parsePorcelainLine(line, &cur, &out)
	}
	if cur != nil {
		out = append(out, *cur)
	}
	if len(out) > 0 {
		out[0].Main = true
	}
	return out
}

// git runs a git command in dir and returns trimmed stdout.
func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimSpace(stdout.String()), nil
}

// worktrees lists all worktrees of the repo containing dir, main first.
func worktrees(dir string) ([]gworktree, error) {
	out, err := git(dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	if wts := parseWorktrees(out); len(wts) > 0 {
		return wts, nil
	}
	return nil, fmt.Errorf("no git worktrees found in %s", dir)
}

// repoRoot returns the main worktree of the repo containing dir.
func repoRoot(dir string) (string, error) {
	wts, err := worktrees(dir)
	if err != nil {
		return "", err
	}
	return wts[0].Path, nil
}

// worktreeAdd mirrors `git worktree add -b`: new branch at baseRef, checked
// out at path. --no-track is unconditional so a bare `git push` can never
// target the base branch.
func worktreeAdd(repoDir, path, branch, baseRef string) error {
	_, err := git(repoDir, "worktree", "add", "--no-track", "-b", branch, path, baseRef)
	return err
}

// worktreeDirty reports whether git would refuse `worktree remove` for
// uncommitted changes. It mirrors git's check_clean_worktree predicate
// (builtin/worktree.c): non-empty `status --porcelain --ignore-submodules=none`.
// --no-optional-locks keeps this query read-only: a plain status refreshes
// .git/index, bumping the .git mtime that worktreeLastUsed reads as creation.
// Best-effort: a failed status read reports clean, like isIgnored.
func worktreeDirty(dir string) bool {
	out, err := git(dir, "--no-optional-locks", "status", "--porcelain", "--ignore-submodules=none")
	return err == nil && out != ""
}

// worktreeRemove always forces: the caller already obtained explicit consent
// (an interactive pick, or --all/--path), and git otherwise refuses dirty trees.
func worktreeRemove(repoDir, path string) error {
	_, err := git(repoDir, "worktree", "remove", "--force", path)
	return err
}

// worktreeLinkMissing reports the state git refuses to remove: the worktree
// directory survives but its .git backlink is gone (out-of-band delete residue).
func worktreeLinkMissing(path string) bool {
	return exists(path) && !exists(filepath.Join(path, ".git"))
}

// worktreeRepair re-creates missing/corrupt .git backlinks for the repo's
// linked worktrees — the git-sanctioned way to make a broken worktree removable.
func worktreeRepair(repoDir string) error {
	_, err := git(repoDir, "worktree", "repair")
	return err
}

func worktreePrune(repoDir string) error {
	_, err := git(repoDir, "worktree", "prune")
	return err
}

// refExists reports whether ref resolves in dir.
func refExists(dir, ref string) bool {
	_, err := git(dir, "rev-parse", "-q", "--verify", ref)
	return err == nil
}

// isIgnored reports whether rel is ignored in the dir worktree; a non-zero
// check-ignore exit (not ignored, or not a repo) is simply false.
func isIgnored(dir, rel string) bool {
	cmd := exec.Command("git", "check-ignore", "-q", "--", rel)
	cmd.Dir = dir
	return cmd.Run() == nil
}

// remoteDefaultRef returns the remote HEAD symref target, e.g.
// refs/remotes/origin/main; origin is preferred when several exist. Only local
// refs are inspected — `git remote show` would hit the network.
func remoteDefaultRef(dir string) string {
	out, err := git(dir, "for-each-ref", "--format=%(refname:short) %(symref)", "refs/remotes/*/HEAD")
	if err != nil || out == "" {
		return ""
	}
	best := ""
	for _, line := range strings.Split(out, "\n") {
		short, sym, ok := strings.Cut(line, " ")
		if !ok || sym == "" {
			continue
		}
		if short == "origin/HEAD" {
			return sym
		}
		if best == "" {
			best = sym
		}
	}
	return best
}

func defaultLocalCandidate(dir string) string {
	for _, c := range []string{"main", "master", "develop", "trunk"} {
		if refExists(dir, "refs/heads/"+c) {
			return c
		}
	}
	if cfg, err := git(dir, "config", "--get", "init.defaultBranch"); err == nil && cfg != "" {
		return cfg
	}
	if wts, err := worktrees(dir); err == nil {
		for _, w := range wts {
			if w.Main && w.Branch != "" {
				return w.Branch
			}
		}
	}
	return "main"
}

// defaultBranch resolves the repo default branch name and the remote it came
// from (if any), using local data only:
//  1. refs/remotes/<remote>/HEAD symref
//  2. local main, master, develop, trunk — conservative order
//  3. git config init.defaultBranch
//  4. the main worktree's checked-out branch
//  5. "main"
func defaultBranch(dir string) (name, remote string) {
	if sym := remoteDefaultRef(dir); sym != "" {
		if rest := strings.TrimPrefix(sym, "refs/remotes/"); rest != sym {
			if r, b, ok := strings.Cut(rest, "/"); ok && b != "" {
				return b, r
			}
		}
	}
	return defaultLocalCandidate(dir), ""
}

// baseRefFor resolves the start-point for a new branch: the remote-tracking
// default when it exists (kept fresh for clones), else the local branch.
func baseRefFor(dir string) string {
	name, remote := defaultBranch(dir)
	remotes := []string{"origin"}
	if remote != "" && remote != "origin" {
		remotes = append([]string{remote}, remotes...)
	}
	for _, r := range remotes {
		if ref := "refs/remotes/" + r + "/" + name; refExists(dir, ref) {
			return ref
		}
	}
	if ref := "refs/heads/" + name; refExists(dir, ref) {
		return ref
	}
	return name
}
