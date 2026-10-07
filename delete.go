package main

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// delete.go — branch-addressed reclamation. `delete <branch>` runs the same
// destructive path as gc (worktree, remote upstream, Pi sessions, prune), but
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
	fs := subFlags("delete", g)
	var o deleteOptions
	fs.BoolVar(&o.keepSessions, "keep-sessions", false, "keep Pi session history")
	fs.BoolVar(&o.keepRemote, "keep-remote", false, "keep the remote upstream branch")
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return usagef("delete: want <branch>")
	}
	o.branch = pos[0]
	return deleteWorktree(g, o)
}

func prepareDelete(g *globals, o deleteOptions) (string, worktree, remotePlan, error) {
	root, ttl, wts, err := loadWorkspace(g, true)
	if err != nil {
		return "", worktree{}, remotePlan{}, err
	}
	def, _ := defaultBranch(root)
	w, err := findWorktreeByBranch(wts, o.branch, def)
	if err != nil {
		return "", worktree{}, remotePlan{}, err
	}
	now := time.Now()
	// Same ACTIVE disclosure as gc --path (findWorktreeByPath).
	if !w.unused(now, ttl) {
		warnf("%s is ACTIVE (%s)", w.Path, w.status(now, ttl))
	}
	// Dirty is computed here, not inside the pure matchBranch.
	w.Dirty = worktreeDirty(w.Path)
	selected := []worktree{*w}
	plan := planRemotes(selected, survivingWorktrees(wts, selected), o.keepRemote, root)
	return root, *w, plan, nil
}

func deleteWorktree(g *globals, o deleteOptions) error {
	root, w, plan, err := prepareDelete(g, o)
	if err != nil {
		// The worktree may already be gone while the branch
		// remains (remote failure keeps it); retry branch-only.
		return deleteOrphanFallback(g, o, err)
	}
	if err := confirmDelete(g, w, plan); err != nil {
		if errors.Is(err, errAborted) && g.json {
			return gcReport(g, abortGCResult(gcOptions{keepSessions: o.keepSessions, keepRemote: o.keepRemote}))
		}
		return err
	}
	res, runErr := runDelete(g, root, w, plan, o.keepSessions)
	return reportResult(g, res, runErr)
}

// matchBranch is pure: name resolution only, no Dirty probe. The caller
// computes Dirty so matching stays side-effect free and testable.
func matchBranch(wts []worktree, branch string) *worktree {
	for i := range wts {
		if wts[i].Branch == branch {
			return &wts[i]
		}
	}
	if norm, _ := normalizeBranch(branch); norm != "" && norm != branch {
		for i := range wts {
			if wts[i].Branch == norm {
				return &wts[i]
			}
		}
	}
	return nil
}

// findWorktreeByBranch resolves the branch-addressed target and refuses the
// main worktree or default branch, which must never be removed.
func findWorktreeByBranch(wts []worktree, branch, defBranch string) (*worktree, error) {
	// Empty must never match a detached worktree (Branch == "").
	if branch == "" {
		return nil, fmt.Errorf("%w %q", errNoWorktreeForBranch, branch)
	}
	w := matchBranch(wts, branch)
	if w == nil {
		return nil, fmt.Errorf("%w %q", errNoWorktreeForBranch, branch)
	}
	if w.Main {
		return nil, fmt.Errorf("refusing to delete the main worktree %s", w.Path)
	}
	if w.Branch == defBranch {
		return nil, fmt.Errorf("refusing to delete default branch %q", w.Branch)
	}
	return w, nil
}

// confirmDelete obtains consent unless --yes. The branch names the target, but
// the removal still destroys uncommitted work, remote history and sessions.
func confirmDelete(g *globals, w worktree, plan remotePlan) error {
	if w.Dirty {
		warnf("%s has uncommitted changes", displayPath(w.Path))
	}
	if g.yes {
		return nil
	}
	if !interactive() {
		return usagef("delete: confirmation requires a terminal; pass --yes")
	}
	ok, err := confirm(deletePromptTitle(w, plan.count()), false)
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted")
		return errAborted
	}
	return nil
}

func deletePromptTitle(w worktree, remotes int) string {
	// An orphan retry has no worktree path; name the branch only.
	if w.Path == "" {
		title := fmt.Sprintf("Delete local branch %s", w.Branch)
		if remotes > 0 {
			title += fmt.Sprintf(", and %d remote branch(es)", remotes)
		}
		return title + "?"
	}
	title := fmt.Sprintf("Delete worktree %s, local branch %s", displayPath(w.Path), w.Branch)
	if remotes > 0 {
		title += fmt.Sprintf(", and %d remote branch(es)", remotes)
	}
	return title + "?"
}

func finishLocalDelete(root, branch string, res *gcResult, runErr error) error {
	if runErr != nil {
		// Actionable retry hint; the "kept local branch X" prefix is pinned.
		warnf("kept local branch %s due to deletion failure; fix the cause and retry `delete %s --yes`", branch, branch)
		return runErr
	}
	if derr := branchDelete(root, branch); derr != nil {
		res.Failed = append(res.Failed, gcFailure{Path: root, Error: "delete local branch: " + derr.Error()})
		return fmt.Errorf("delete local branch %s: %w", branch, derr)
	}
	res.DeletedBranch = branch
	return nil
}

// runDelete performs the gc removal, then force-deletes the local branch the
// command is addressed by. gc deliberately keeps local branches; delete names
// one, so it removes it — after the worktree is gone, so the branch is free.
func runDelete(g *globals, root string, w worktree, plan remotePlan, keepSessions bool) (gcResult, error) {
	// confirmDelete already disclosed uncommitted changes before consent, so
	// clear Dirty to keep removeOneWorktree from warning a second time.
	w.Dirty = false
	var res gcResult
	runErr := withProgress(g, true, func(p *progress) error {
		var rerr error
		res, rerr = gcRemove(root, []worktree{w}, plan, keepSessions, p)
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
	root, _, wts, err := loadWorkspace(g, true)
	if err != nil {
		return cause
	}
	branch, ok := orphanBranchName(root, o.branch)
	if !ok {
		return cause
	}
	if def, _ := defaultBranch(root); branch == def {
		return fmt.Errorf("refusing to delete default branch %q", branch)
	}
	return runBranchOnlyDelete(g, o, root, wts, branch)
}

// runBranchOnlyDelete confirms and runs the worktree-less retry: the remote
// upstream (unless kept) via the shared veto-aware purge, then the branch.
func runBranchOnlyDelete(g *globals, o deleteOptions, root string, wts []worktree, branch string) error {
	warnf("no worktree for branch %q; continuing with branch-only cleanup", branch)
	ups, _ := branchUpstreams(root)
	w := worktree{Branch: branch, Upstream: ups[branch]}
	plan := planRemotes([]worktree{w}, wts, o.keepRemote, root)
	if cerr := confirmDelete(g, w, plan); cerr != nil {
		if errors.Is(cerr, errAborted) && g.json {
			return gcReport(g, abortGCResult(gcOptions{keepSessions: o.keepSessions, keepRemote: o.keepRemote}))
		}
		return cerr
	}
	res, runErr := runOrphanDelete(g, root, w, plan, o.keepSessions)
	return reportResult(g, res, runErr)
}

// orphanBranchName maps the delete argument onto the surviving local branch,
// trying the raw name first so an already-valid ref is never renormalized.
func orphanBranchName(root, raw string) (string, bool) {
	if raw == "" {
		return "", false
	}
	// A refname cannot start with '-', so a leading-hyphen argument is never a
	// raw local branch; skipping it also keeps git from reading it as an option.
	if raw[0] != '-' && refExists(root, "refs/heads/"+raw) {
		return raw, true
	}
	if norm, err := normalizeBranch(raw); err == nil && norm != raw && refExists(root, "refs/heads/"+norm) {
		return norm, true
	}
	return "", false
}

// runOrphanDelete deletes the orphan's remote upstream (unless kept) and the
// local branch, reusing the shared purge and branch finishers.
func runOrphanDelete(g *globals, root string, w worktree, plan remotePlan, keepSessions bool) (gcResult, error) {
	res := emptyGCResult(keepSessions, plan.keptRemote())
	runErr := withProgress(g, true, func(p *progress) error {
		if !plan.keep && w.Upstream.onRemote() {
			purgeRemoteWorktree(root, w, plan, &res, p, map[string]error{})
		}
		return nil
	})
	if runErr == nil && len(res.Failed) > 0 {
		runErr = fmt.Errorf("delete remote %s failed", w.Upstream.Short)
	}
	return res, finishLocalDelete(root, w.Branch, &res, runErr)
}
