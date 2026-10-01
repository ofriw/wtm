package main

import (
	"fmt"
	"path/filepath"
	"strings"
)

// add.go — create a worktree, then seed it with the source's Pi harness and
// ChunkHound workspace.

type addOptions struct {
	branch  string
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
	fs.StringVar(&o.branch, "b", "", "new branch name (default: base name of path)")
	fs.StringVar(&o.branch, "branch", "", "alias of -b")
	fs.StringVar(&o.from, "from", "", "source worktree to seed from (default: main worktree)")
	fs.BoolVar(&o.noIndex, "no-index", false, "skip `chunkhound index` after seeding")
	pos, err := parseSub(fs, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("add: want <path> [<start-point>]")
	}
	if len(pos) == 2 {
		o.start = pos[1]
	}
	return addWorktree(g, pos[0], o)
}

func seedWorktree(src, path string, noIndex bool) error {
	if err := copyPiHarness(src, path); err != nil {
		return fmt.Errorf("copy Pi harness: %w", err)
	}
	if err := copyChunkHound(src, path); err != nil {
		return fmt.Errorf("copy ChunkHound workspace: %w", err)
	}
	if err := rewriteRootGuard(path); err != nil {
		return err
	}
	if err := patchDatabasePath(src, path); err != nil {
		return err
	}
	if !noIndex {
		return runIndex(path)
	}
	return nil
}

func resolveAddSource(root, from string) (string, error) {
	if from == "" {
		return root, nil
	}
	src, err := canonicalAbs(from)
	if err != nil {
		return "", err
	}
	if !exists(src) {
		return "", fmt.Errorf("--from %s: not found", src)
	}
	return src, nil
}

func resolveBranchAndBase(root, path, branch, start string) (string, string) {
	if branch == "" {
		branch = filepath.Base(path)
	}
	if start == "" {
		start = baseRefFor(root)
	}
	return branch, start
}

// rollbackAdd undoes a failed seed: pristine checkouts are removed, but any
// seeded content (even gitignored, e.g. a copied .mcp.json) is user-inspectable
// state, so the partial worktree survives and the caller is told to inspect it.
func rollbackAdd(root, path, branch string, seedErr error) error {
	// --ignored keeps the check honest: a copied .mcp.json is ignored, yet is
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

func resolveAddPaths(g *globals, pathArg, from string) (root, src, path string, err error) {
	if root, err = resolveRoot(g); err != nil {
		return
	}
	if src, err = resolveAddSource(root, from); err != nil {
		return
	}
	path, err = canonicalAbs(pathArg)
	return
}

// addWorktree mirrors `git worktree add -b <branch> <path> <start-point>` and
// then replicates the source's harness. Idempotent by construction: an
// existing path or branch fails the git step before anything is written.
func addWorktree(g *globals, pathArg string, o addOptions) error {
	root, src, path, err := resolveAddPaths(g, pathArg, o.from)
	if err != nil {
		return err
	}
	branch, base := resolveBranchAndBase(root, path, o.branch, o.start)
	if err := worktreeAdd(root, path, branch, base); err != nil {
		return err
	}
	if err := seedWorktree(src, path, o.noIndex); err != nil {
		return rollbackAdd(root, path, branch, err)
	}
	return reportAddResult(g, addResult{Path: path, Branch: branch, Base: base, Source: src, Indexed: !o.noIndex})
}
