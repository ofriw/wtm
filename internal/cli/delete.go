package cli

import (
	"errors"
	"fmt"
	"os"
	"time"

	"wtm/internal/core"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/ui"
)

// delete.go — branch-addressed reclamation. `delete <branch>` runs the same
// destructive path as gc (worktree, remote upstream, agent sessions, prune), but
// for one explicit worktree and regardless of the unused TTL: naming the
// branch is the consent, so the UNUSED gate does not apply.

// errNoWorktreeForBranch lets delete detect the branch-only case by identity
// instead of matching the message text.
var errNoWorktreeForBranch = errors.New("no worktree has branch")

type deleteOptions struct {
	branch       string
	keepSessions bool
	keepRemote   bool
}

func cmdDelete(g *globals, args []string) error {
	flags := subFlags("delete", g)
	var o deleteOptions
	flags.BoolVar(&o.keepSessions, "keep-sessions", false, "keep agent session history")
	flags.BoolVar(&o.keepRemote, "keep-remote", false, "keep the remote upstream branch")
	pos, err := parseSub(flags, g, args, usage)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("delete: want <branch>")
	}
	o.branch = pos[0]
	return deleteWorktree(g, o)
}

func prepareDelete(g *globals, o deleteOptions) (core.Workspace, core.Worktree, remotePlan, error) {
	ws, err := loadWorkspace(g, true, harness.DiscoverySessions(true))
	if err != nil {
		return core.Workspace{}, core.Worktree{}, remotePlan{}, err
	}
	branch, def, err := resolveRepoBranch(ws.Root, o.branch)
	if err != nil {
		return core.Workspace{}, core.Worktree{}, remotePlan{}, err
	}
	w, err := checkedDeleteTarget(exactBranch(ws.Wts, branch.Name), branch.Name, def)
	if err != nil {
		return core.Workspace{}, core.Worktree{}, remotePlan{}, err
	}
	now := time.Now()
	// Same ACTIVE disclosure as gc --path (findWorktreeByPath).
	if !w.Unused(now, ws.TTL) {
		ui.Warnf("%s is ACTIVE (%s)", w.Path, w.Status(now, ws.TTL))
	}
	// Dirty is computed here, not inside the shared resolver.
	w.Dirty = git.WorktreeDirty(w.Path)
	selected := []core.Worktree{*w}
	plan := planRemotes(selected, survivingWorktrees(ws.Wts, selected), o.keepRemote, ws.Root)
	return ws, *w, plan, nil
}

func deleteWorktree(g *globals, o deleteOptions) error {
	ws, w, plan, err := prepareDelete(g, o)
	if err != nil {
		// The worktree may already be gone while the branch
		// remains (remote failure keeps it); retry branch-only.
		return deleteOrphanFallback(g, o, err)
	}
	if err := confirmDelete(g, w, plan); err != nil {
		if errors.Is(err, errAborted) && g.JSON {
			return gcReport(g, abortGCResult(gcOptions{keepSessions: o.keepSessions, keepRemote: o.keepRemote}))
		}
		return err
	}
	res, runErr := runDelete(g, ws.Root, w, plan, o.keepSessions, ws.Idx)
	return reportResult(g, res, runErr)
}

// resolveRepoBranch resolves raw against local refs and returns the stored
// branch plus the stored default branch. delete and promote share it so target
// identity never depends on the caller's spelling.
func resolveRepoBranch(root, raw string) (core.BranchChoice, string, error) {
	refs, err := git.LocalBranches(root)
	if err != nil {
		return core.BranchChoice{}, "", err
	}
	choice, err := core.ResolveLocalBranch(refs, raw, core.BranchForAddress)
	if err != nil {
		return core.BranchChoice{}, "", err
	}
	def, _ := git.DefaultBranch(root)
	return choice, core.StoredBranchName(refs, def), nil
}

func exactBranch(wts []core.Worktree, branch string) *core.Worktree {
	for i := range wts {
		if branch != "" && wts[i].Branch == branch {
			return &wts[i]
		}
	}
	return nil
}

func checkedDeleteTarget(w *core.Worktree, branch, defBranch string) (*core.Worktree, error) {
	if w == nil {
		return nil, fmt.Errorf("%w %q", errNoWorktreeForBranch, branch)
	}
	if w.Main {
		return nil, fmt.Errorf("refusing to remove the main worktree %s", w.Path)
	}
	if w.Branch == defBranch {
		return nil, fmt.Errorf("refusing to remove default branch %q", w.Branch)
	}
	return w, nil
}

// confirmDelete obtains consent unless --yes. The branch names the target, but
// the removal still destroys uncommitted work, remote history and sessions.
func confirmDelete(g *globals, w core.Worktree, plan remotePlan) error {
	if w.Dirty {
		ui.Warnf("%s has uncommitted changes", core.DisplayPath(w.Path))
	}
	if g.Yes {
		return nil
	}
	if !ui.Interactive() {
		return usagef("delete: confirmation requires a terminal; pass --yes")
	}
	ok, err := ui.Confirm(deletePromptTitle(w, plan.count()), false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted")
		return errAborted
	}
	return nil
}

func deletePromptTitle(w core.Worktree, remotes int) string {
	// An orphan retry has no worktree path; name the branch only.
	if w.Path == "" {
		title := fmt.Sprintf("Delete local branch %s", w.Branch)
		if remotes > 0 {
			title += fmt.Sprintf(", and %d remote branch(es)", remotes)
		}
		return title + "?"
	}
	title := fmt.Sprintf("Delete worktree %s, local branch %s", core.DisplayPath(w.Path), w.Branch)
	if remotes > 0 {
		title += fmt.Sprintf(", and %d remote branch(es)", remotes)
	}
	return title + "?"
}

func finishLocalDelete(root, branch string, res *gcResult, runErr error) error {
	if runErr != nil {
		// Actionable retry hint; the "kept local branch X" prefix is pinned.
		ui.Warnf("kept local branch %s due to deletion failure; fix the cause and retry `delete %s --yes`", branch, branch)
		return runErr
	}
	if derr := git.BranchDelete(root, branch); derr != nil {
		res.Failed = append(res.Failed, gcFailure{Path: root, Error: "delete local branch: " + derr.Error()})
		return fmt.Errorf("delete local branch %s: %w", branch, derr)
	}
	res.DeletedBranch = branch
	return nil
}

// runDelete performs the gc removal, then force-deletes the local branch the
// command is addressed by. Keep this recovery ref until all cleanup succeeds.
func runDelete(g *globals, root string, w core.Worktree, plan remotePlan, keepSessions bool, idx core.SessionIndex) (gcResult, error) {
	// confirmDelete already disclosed uncommitted changes before consent, so
	// clear Dirty to keep removeOneWorktree from warning a second time.
	w.Dirty = false
	var res gcResult
	runErr := withProgress(g, true, func(p *ui.Progress) error {
		var rerr error
		res, rerr = gcCleanup(root, []core.Worktree{w}, plan, keepSessions, idx, gcMode{}, time.Now(), p)
		return rerr
	})
	if len(res.Removed) > 0 {
		runErr = finishLocalDelete(root, w.Branch, &res, runErr)
	}
	return res, runErr
}

// deleteOrphanFallback handles a branch with no worktree: the local branch
// remains after a first delete whose worktree was removed but whose remote
// delete failed, or the branch never had a worktree at all. Either way, the
// branch name is the consent, so it runs the branch-only cleanup. Without it a
// retry fails with "no worktree has branch" and the branch is orphaned.
func deleteOrphanFallback(g *globals, o deleteOptions, cause error) error {
	if cause == nil || !errors.Is(cause, errNoWorktreeForBranch) {
		return cause
	}
	ws, err := loadWorkspace(g, true, harness.DiscoverySessions(true))
	if err != nil {
		return err
	}
	branch, def, err := resolveRepoBranch(ws.Root, o.branch)
	if err != nil {
		return err
	}
	if !branch.Exists {
		return cause
	}
	if branch.Name == def {
		return fmt.Errorf("refusing to remove default branch %q", branch.Name)
	}
	return runBranchOnlyDelete(g, o, ws.Root, ws.Wts, branch.Name)
}

// runBranchOnlyDelete confirms and runs the worktree-less retry: the remote
// upstream (unless kept) via the shared veto-aware purge, then the branch. It
// refuses a branch a live worktree still holds; the resolved stored name makes
// that comparison independent of the argument spelling.
func runBranchOnlyDelete(g *globals, o deleteOptions, root string, wts []core.Worktree, branch string) error {
	if w := exactBranch(wts, branch); w != nil {
		return fmt.Errorf("refusing branch-only delete of %q: checked out at %s", branch, core.DisplayPath(w.Path))
	}
	ui.Warnf("no worktree for branch %q; continuing with branch-only cleanup", branch)
	ups, _ := git.BranchUpstreams(root)
	w := core.Worktree{Branch: branch, Upstream: ups[branch]}
	plan := planRemotes([]core.Worktree{w}, wts, o.keepRemote, root)
	if cerr := confirmDelete(g, w, plan); cerr != nil {
		if errors.Is(cerr, errAborted) && g.JSON {
			return gcReport(g, abortGCResult(gcOptions{keepSessions: o.keepSessions, keepRemote: o.keepRemote}))
		}
		return cerr
	}
	res, runErr := runOrphanDelete(g, root, w, plan, o.keepSessions)
	return reportResult(g, res, runErr)
}

// runOrphanDelete deletes the orphan's remote upstream (unless kept) and the
// local branch, reusing the shared purge and branch finishers.
func runOrphanDelete(g *globals, root string, w core.Worktree, plan remotePlan, keepSessions bool) (gcResult, error) {
	res := emptyGCResult(keepSessions, plan.keptRemote())
	runErr := withProgress(g, true, func(p *ui.Progress) error {
		if !plan.keep && w.Upstream.OnRemote() {
			purgeRemoteWorktree(root, w, plan, &res, p, map[string]error{})
		}
		return nil
	})
	if runErr == nil && len(res.Failed) > 0 {
		runErr = fmt.Errorf("delete remote %s failed", w.Upstream.Short)
	}
	return res, finishLocalDelete(root, w.Branch, &res, runErr)
}
