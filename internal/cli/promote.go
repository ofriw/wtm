package cli

import (
	"errors"
	"fmt"
	"strings"

	"wtm/internal/core"
	"wtm/internal/harness"
	"wtm/internal/temp"
	"wtm/internal/ui"
)

// Promotion removes registry permission and private Git identity metadata.
// The checkout and its branch are left untouched.

type promoteResult struct {
	Path     string `json:"path"`
	Branch   string `json:"branch"`
	Promoted bool   `json:"promoted"`
}

func cmdPromote(g *globals, args []string) error {
	flags := subFlags("promote", g)
	pos, err := parseSub(flags, g, args, usage)
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
	ws, err := loadWorkspace(g, true, harness.DiscoverySessions(false))
	if err != nil {
		return err
	}
	choice, def, err := resolveRepoBranch(ws.Root, branch)
	if err != nil {
		return err
	}
	w, err := checkedDeleteTarget(exactBranch(ws.Wts, choice.Name), choice.Name, def)
	if err != nil {
		return explainPromoteMiss(ws.Root, ws.Wts, choice.Name, err)
	}
	promoted, err := temp.PromoteTempRecord(w.Path)
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
func explainPromoteMiss(root string, wts []core.Worktree, branch string, cause error) error {
	alias, err := core.TempBranch(branch)
	if !errors.Is(cause, errNoWorktreeForBranch) || err != nil || alias == branch {
		return cause
	}
	live, _, err := resolveRepoBranch(root, alias)
	if err != nil {
		return err
	}
	if !live.Exists || live.Name == branch {
		return cause
	}
	if w := exactBranch(wts, live.Name); w != nil {
		return fmt.Errorf("%w; %q is checked out at %s; run `wtm promote %s`",
			cause, live.Name, core.DisplayPath(w.Path), live.Name)
	}
	return cause
}

func printPromoteResult(g *globals, res promoteResult) error {
	if g.JSON {
		return ui.PrintJSON(res)
	}
	verb := "promoted"
	if !res.Promoted {
		verb = "already permanent"
	}
	fmt.Printf("%s %s\n", verb, res.Path)
	if res.Promoted && strings.HasPrefix(res.Branch, core.TempBranchPrefix) {
		fmt.Printf("branch remains %s (temporary expiry disabled; normal gc rules apply)\n", res.Branch)
	}
	return nil
}
