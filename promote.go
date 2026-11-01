package main

import (
	"errors"
	"fmt"
	"strings"
)

// Promotion removes registry permission and private Git identity metadata.
// The checkout and its branch are left untouched.

type promoteResult struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Promoted bool   `json:"promoted"`
}

func cmdPromote(g *globals, args []string) error {
	fs := subFlags("promote", g)
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("promote: want <branch>")
	}
	return promoteWorktree(g, pos[0])
}

// promoteWorktree resolves the worktree addressed by branch and clears its temp
// record. A worktree that is already permanent is the desired end state, so it
// succeeds with promoted=false rather than erroring (safe for retries).
func promoteWorktree(g *globals, branch string) error {
	ws, err := loadWorkspace(g, true, false)
	if err != nil {
		return err
	}
	choice, def, err := resolveRepoBranch(ws.root, branch)
	if err != nil {
		return err
	}
	w, err := checkedDeleteTarget(exactBranch(ws.wts, choice.name), choice.name, def)
	if err != nil {
		return explainPromoteMiss(ws.root, ws.wts, choice.name, err)
	}
	promoted, err := promoteTempRecord(w.Path)
	if err != nil {
		return fmt.Errorf("promote %s: %w", w.Path, err)
	}
	return printPromoteResult(g, promoteResult{Path: w.Path, Branch: w.Branch, Promoted: promoted})
}

// explainPromoteMiss adds the live tmp alias to a missing-checkout refusal. A
// ref with no checkout wins resolution, so promote cannot silently retarget a
// different checkout; the error names the command that reaches the alias.
// The alias resolves through resolveRepoBranch so the hint uses Git's stored
// spelling, which may differ by case from the requested tmp/ name.
func explainPromoteMiss(root string, wts []worktree, branch string, cause error) error {
	alias, err := tempBranch(branch)
	if !errors.Is(cause, errNoWorktreeForBranch) || err != nil || alias == branch {
		return cause
	}
	live, _, err := resolveRepoBranch(root, alias)
	if err != nil {
		return err
	}
	if !live.exists || live.name == branch {
		return cause
	}
	if w := exactBranch(wts, live.name); w != nil {
		return fmt.Errorf("%w; %q is checked out at %s; run `wtm promote %s`",
			cause, live.name, displayPath(w.Path), live.name)
	}
	return cause
}

func printPromoteResult(g *globals, res promoteResult) error {
	if g.json {
		return printJSON(res)
	}
	verb := "promoted"
	if !res.Promoted {
		verb = "already permanent"
	}
	fmt.Printf("%s %s\n", verb, res.Path)
	if res.Promoted && strings.HasPrefix(res.Branch, tempBranchPrefix) {
		fmt.Printf("branch remains %s (temporary expiry disabled; normal gc rules apply)\n", res.Branch)
	}
	return nil
}
