package main

import (
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
	root, _, wts, err := loadWorkspace(g, true)
	if err != nil {
		return err
	}
	def, _ := defaultBranch(root)
	w, err := findWorktreeByBranch(wts, branch, def)
	if err != nil {
		return err
	}
	promoted, err := promoteTempRecord(w.Path)
	if err != nil {
		return fmt.Errorf("promote %s: %w", w.Path, err)
	}
	return printPromoteResult(g, promoteResult{Path: w.Path, Branch: w.Branch, Promoted: promoted})
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
