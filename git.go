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
	HEAD     string // resolved commit; keys start-point matching in add.go
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
	case "HEAD":
		cur.HEAD = val
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

// gitPrivateDir resolves the per-worktree private admin dir ($GIT_DIR) via
// --absolute-git-dir: a linked worktree's own metadata dir holding its .git
// backlink target (e.g. <main>/.git/worktrees/<name>), not the shared repo dir.
// Callers needing per-checkout state (not common objects) start here.
func gitPrivateDir(dir string) (string, error) {
	out, err := git(dir, "rev-parse", "--absolute-git-dir")
	return strings.TrimSpace(out), err
}

// gitCurrentBranch returns an empty branch for valid detached HEADs only.
func gitCurrentBranch(dir string) (string, error) {
	cmd := exec.Command("git", "symbolic-ref", "--quiet", "--short", "HEAD")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		return strings.TrimSpace(string(out)), nil
	}
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == 1 {
		// Exit 1 means non-symbolic; verify HEAD to reject broken checkouts.
		_, err = git(dir, "rev-parse", "--verify", "HEAD")
		return "", err
	}
	return "", fmt.Errorf("git symbolic-ref --quiet --short HEAD: %s: %w", strings.TrimSpace(string(out)), err)
}

// gitTopLevel returns the root of the worktree containing dir (main or linked).
func gitTopLevel(dir string) (string, error) {
	return git(dir, "rev-parse", "--show-toplevel")
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
// --no-optional-locks prevents index refresh from becoming false activity;
// scanIndexTime uses the private index mtime to protect staged changes.
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

// upstream is a local branch's configured git upstream (tracking branch).
// Short is the display form (origin/feat); Remote/Ref are the push-delete
// target, so a differently-named upstream is deleted exactly, not guessed.
type upstream struct {
	Short  string // origin/feat
	Remote string // origin
	Ref    string // refs/heads/feat
}

// present reports whether an upstream is configured; the zero value is "none".
// A local-branch upstream (branch.X.remote=".") also counts as present: status
// shows the configured truth, while gc deletes only what onRemote() allows.
func (u upstream) present() bool { return u.Remote != "" && u.Ref != "" }

// onRemote reports whether the upstream branch lives on a git remote, i.e. is
// safe to delete with push --delete. Remote "." is git's marker for an upstream
// that is another LOCAL branch, which a push-delete would destroy as a local
// branch — gc must skip it, not "clean it up".
func (u upstream) onRemote() bool { return u.present() && u.Remote != "." }

// key identifies the push-delete target for dedup; tab cannot appear in a ref.
func (u upstream) key() string { return u.Remote + "\t" + u.Ref }

// branchUpstreams maps each local branch to its configured upstream in one
// offline call. Tab separates fields because a refname can hold neither tab nor
// newline; the %(upstream:*) atoms expand empty for a branch without one.
func branchUpstreams(dir string) (map[string]upstream, error) {
	out, err := git(dir, "for-each-ref",
		"--format=%(refname:short)%09%(upstream:short)%09%(upstream:remotename)%09%(upstream:remoteref)",
		"refs/heads/")
	if err != nil {
		return nil, err
	}
	ups := map[string]upstream{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Split(line, "\t")
		if len(f) != 4 || f[0] == "" || f[2] == "" {
			continue
		}
		ups[f[0]] = upstream{Short: f[1], Remote: f[2], Ref: f[3]}
	}
	return ups, nil
}

// remoteRefExists reports whether ref exists on remote, via ls-remote so no
// fetch is needed. A failed query is an error, never a silent "absent":
// treating an unreachable remote as already-deleted would hide the outage and
// leave the branch behind.
func remoteRefExists(dir, remote, ref string) (bool, error) {
	out, err := git(dir, "ls-remote", "--heads", remote, ref)
	if err != nil {
		return false, err
	}
	return out != "", nil
}

// remoteDefaultBranchRef returns the branch a remote's HEAD points at, as a
// local ref name (refs/heads/main); "" when the remote has no recorded HEAD.
// Local refs only: `git remote show` would hit the network.
func remoteDefaultBranchRef(dir, remote string) string {
	out, err := git(dir, "symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD")
	if err != nil {
		return ""
	}
	branch := strings.TrimPrefix(out, "refs/remotes/"+remote+"/")
	if branch == out || branch == "" {
		return ""
	}
	return "refs/heads/" + branch
}

// deleteRemoteBranch deletes ref on the branch's remote. git drops the
// matching local remote-tracking ref as part of the same push, so no separate
// prune is needed (pinned by test/scenarios/gc-remote/expected.manifest, where
// refs/remotes/origin/wt1 is gone while gc-remote-keep's manifest keeps it).
// Ref stays canonical (refs/heads/...) at the call sites; only the push trims
// to the branch name `git push --delete` expects.
func deleteRemoteBranch(dir, remote, ref string) error {
	_, err := git(dir, "push", remote, "--delete", strings.TrimPrefix(ref, "refs/heads/"))
	return err
}

// branchDelete force-deletes a local branch. `delete <branch>` and gc's temp
// purge both name the branch as their target, so -D is intended: the user (or a
// collected temp) chose to drop it even if it is unmerged. The worktree that
// checked it out must already be removed. -- stops a branch name from being
// read as a git option.
func branchDelete(dir, branch string) error {
	_, err := git(dir, "branch", "-D", "--", branch)
	return err
}
