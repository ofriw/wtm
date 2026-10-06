package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// add.go — create a worktree, then seed it with the source's agent harness and
// ChunkHound workspace.

type addOptions struct {
	path    string // explicit directory override; empty derives a sibling path
	start   string
	from    string
	noIndex bool
}

type addResult struct {
	Path    string `json:"path"`
	Branch  string `json:"branch"`
	Base    string `json:"base"`
	Source  string `json:"source"`
	Indexed bool   `json:"indexed"`
}

func cmdAdd(g *globals, args []string) error {
	fs := subFlags("add", g)
	var o addOptions
	fs.StringVar(&o.path, "p", "", "worktree directory (default: sibling of the current worktree)")
	fs.StringVar(&o.path, "path", "", "alias of -p")
	fs.StringVar(&o.from, "from", "", "source worktree to seed from (default: the worktree at the start-point)")
	fs.BoolVar(&o.noIndex, "no-index", false, "skip `chunkhound index` after seeding (also skipped when chunkhound is absent)")
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("add: want <branch> [<start-point>]")
	}
	if len(pos) == 2 {
		o.start = pos[1]
	}
	return addWorktree(g, pos[0], o)
}

func seedWorktree(src, path string, noIndex bool) (bool, error) {
	if err := copyHarness(src, path); err != nil {
		return false, fmt.Errorf("copy agent harness: %w", err)
	}
	if err := copyChunkHound(src, path); err != nil {
		return false, fmt.Errorf("copy ChunkHound workspace: %w", err)
	}
	if err := rewriteRootGuard(path); err != nil {
		return false, err
	}
	if err := patchDatabasePath(src, path); err != nil {
		return false, err
	}
	return indexSeed(path, noIndex)
}

// indexSeed refreshes the seeded db unless indexing is opted out or chunkhound
// is absent. A missing binary is not an error — ChunkHound is optional — but a
// seeded workspace means the project does use it, so the skip is disclosed.
func indexSeed(path string, noIndex bool) (bool, error) {
	if noIndex {
		return false, nil
	}
	switch err := runIndex(path); {
	case errors.Is(err, errNoChunkHound):
		warnIfChunkHoundExpected(path)
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// warnIfChunkHoundExpected warns that indexing was skipped for a worktree that
// carries a ChunkHound workspace, whose copied db is therefore not refreshed.
func warnIfChunkHoundExpected(path string) {
	if hasChunkHoundWorkspace(path) {
		warnf("chunkhound not on PATH; skipped indexing %s; search results for this worktree may be stale", path)
	}
}

// resolveAddSource picks the worktree to seed from. An explicit --from wins.
// Otherwise the source is the worktree checked out at the start-point, so the
// seeded harness — gitignored db/agent/mcp state git cannot carry — matches the
// tree the new worktree is created from. When no worktree sits at the
// start-point, the ref is checked out nowhere and no faithful index exists, so
// the main worktree is the fallback and the mismatch is disclosed: the copied
// index cannot match, so indexing must re-embed instead of being a no-op.
func resolveAddSource(root string, wts []gworktree, base, from, target string) (string, error) {
	if from != "" {
		src, err := canonicalAbs(from)
		if err != nil {
			return "", err
		}
		if !exists(src) {
			return "", fmt.Errorf("--from %s: not found", src)
		}
		return src, nil
	}
	if src, ok := worktreeAt(root, wts, base, target); ok {
		return src, nil
	}
	warnf("no worktree checked out at %s; seeding from %s; the copied ChunkHound index cannot match and indexing will re-embed", base, canonical(root))
	return canonical(root), nil
}

// worktreeAt returns the existing worktree checked out at base. It prefers the
// worktree whose branch is base's branch, then any worktree at the same commit;
// both require a commit match, so a diverged branch of the same name never
// masquerades as the start-point. target (the checkout being created) is excluded.
func worktreeAt(root string, wts []gworktree, base, target string) (string, bool) {
	commit, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil {
		return "", false
	}
	branch := baseBranchName(root, base)
	exact, same := "", ""
	for _, w := range wts {
		p := canonical(w.Path)
		if p == target || w.HEAD != commit {
			continue
		}
		if branch != "" && w.Branch == branch {
			exact = p
			break
		}
		if same == "" {
			same = p
		}
	}
	if exact != "" {
		return exact, true
	}
	return same, same != ""
}

// baseBranchName maps a start-point ref to its short branch name: refs/heads/x
// and refs/remotes/<remote>/x both yield x; a tag or raw commit yields "".
func baseBranchName(root, base string) string {
	ref, err := git(root, "rev-parse", "--symbolic-full-name", base)
	if err != nil {
		return ""
	}
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b
	}
	if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		if _, b, ok := strings.Cut(rest, "/"); ok {
			return b
		}
	}
	return ""
}

// locationBase is where sibling worktrees live: the parent of the worktree the
// user is in when it belongs to the target repo, else the parent of the repo's
// main worktree. Never the cwd itself — a worktree created under the cwd nests
// inside another and breaks IDE watchers and test runners.
func locationBase(root string, wts []gworktree) string {
	fallback := filepath.Dir(canonical(root))
	cwd, err := os.Getwd()
	if err != nil {
		return fallback
	}
	top, err := gitTopLevel(cwd)
	if err != nil {
		return fallback
	}
	top = canonical(top)
	for _, w := range wts {
		if canonical(w.Path) == top {
			return filepath.Dir(top)
		}
	}
	return fallback
}

// resolveAddTarget picks the checkout directory: an explicit --path (guarded
// against nesting), or a sibling named <project>-<branch-slug>.
func resolveAddTarget(root string, wts []gworktree, branch, override string) (string, error) {
	if override != "" {
		target, err := canonicalAbs(override)
		if err != nil {
			return "", err
		}
		if parent, nested := nestedIn(target, wts); nested {
			return "", usagef("add: refusing to create a worktree inside %s; worktrees must be siblings, not nested", parent)
		}
		return target, nil
	}
	// The derived path is guarded like --path: locationBase can return a
	// directory inside another worktree when the current worktree is itself
	// nested, and a sibling must never be nested either.
	target := filepath.Join(locationBase(root, wts), deriveDirName(projectName(wts), branch))
	if parent, nested := nestedIn(target, wts); nested {
		return "", usagef("add: refusing to create a worktree inside %s; worktrees must be siblings, not nested", parent)
	}
	return target, nil
}

// rollbackAdd undoes a failed seed: pristine checkouts are removed, but any
// seeded content (even gitignored, e.g. a copied MCP config) is user-inspectable
// state, so the partial worktree survives and the caller is told to inspect it.
func rollbackAdd(root, path, branch string, seedErr error) error {
	// --ignored keeps the check honest: a copied MCP config is ignored, yet is
	// exactly the seeded state a user must be able to inspect.
	changes, checkErr := git(path, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if checkErr != nil || changes != "" {
		return fmt.Errorf("%w; partial worktree kept at %s (branch %s); inspect it before removing", seedErr, path, branch)
	}
	if cleanup := worktreeRemove(root, path); cleanup != nil {
		return fmt.Errorf("%w; partial worktree kept at %s (branch %s): %v", seedErr, path, branch, cleanup)
	}
	if _, cleanup := git(root, "branch", "-d", branch); cleanup != nil {
		return fmt.Errorf("%w; worktree removed but branch %s remains: %v", seedErr, branch, cleanup)
	}
	return seedErr
}

func reportAddResult(g *globals, res addResult) error {
	if g.json {
		return printJSON(res)
	}
	origin := "local"
	if strings.HasPrefix(res.Base, "refs/remotes/") {
		origin = "remote"
	}
	fmt.Printf("worktree: %s\nbranch:   %s\nbase:     %s (%s)\nsource:   %s\nindexed:  %v\n",
		res.Path, res.Branch, res.Base, origin, res.Source, res.Indexed)
	return nil
}

type addParams struct {
	root, src, path, branch, base string
}

func prepareAdd(g *globals, branchArg string, o addOptions) (addParams, error) {
	root, err := resolveRoot(g)
	if err != nil {
		return addParams{}, err
	}
	branch, err := normalizeBranch(branchArg)
	if err != nil {
		return addParams{}, usagef("add: %v", err)
	}
	warnBranchConvention(branch)
	wts, err := worktrees(root)
	if err != nil {
		return addParams{}, err
	}
	path, err := resolveAddTarget(root, wts, branch, o.path)
	if err != nil {
		return addParams{}, err
	}
	base := o.start
	if base == "" {
		base = baseRefFor(root)
	}
	src, err := resolveAddSource(root, wts, base, o.from, path)
	if err != nil {
		return addParams{}, err
	}
	return addParams{root: root, src: src, path: path, branch: branch, base: base}, nil
}

// addWorktree creates the branch and its sibling checkout, then replicates the
// source's harness. Idempotent by construction: an existing path or branch
// fails the git step before anything is written.
func addWorktree(g *globals, branchArg string, o addOptions) error {
	p, err := prepareAdd(g, branchArg, o)
	if err != nil {
		return err
	}
	if err := worktreeAdd(p.root, p.path, p.branch, p.base); err != nil {
		return err
	}
	indexed, err := seedWorktree(p.src, p.path, o.noIndex)
	if err != nil {
		return rollbackAdd(p.root, p.path, p.branch, err)
	}
	return reportAddResult(g, addResult{Path: p.path, Branch: p.branch, Base: p.base, Source: p.src, Indexed: indexed})
}
