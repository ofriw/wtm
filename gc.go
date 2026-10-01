package main

import (
	"errors"
	"fmt"
	"os"
	"time"
)

// gc.go — remove UNUSED worktrees and their Pi session history.

type gcOptions struct {
	all          bool
	keepSessions bool
	paths        []string
}

type gcFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type gcResult struct {
	Removed        []string    `json:"removed"`
	Failed         []gcFailure `json:"failed"`
	SessionsPurged int         `json:"sessionsPurged"`
	KeptSessions   bool        `json:"keptSessions"`
	Pruned         bool        `json:"pruned"`
}

// errAborted is returned when the user declines gc confirmation. The "aborted"
// message is already on stderr (see confirmGC), so dispatch maps it to exit 0
// silent rather than printing a redundant line.
var errAborted = errors.New("aborted")

// emptyGCResult is the zero-removal report shape: nil-length slices (not nil)
// so JSON serializes [] not null (pinned by TestGCReport).
func emptyGCResult(kept bool) gcResult {
	return gcResult{Removed: []string{}, Failed: []gcFailure{}, KeptSessions: kept}
}

func confirmGC(selected []worktree) (bool, error) {
	if !interactive() {
		return false, usagef("gc: confirmation requires a terminal; pass --yes")
	}

	ok, err := confirm(fmt.Sprintf("Remove %d worktree(s)?", len(selected)), false)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted")
		return false, errAborted
	}
	return ok, nil
}

func validateGCArgs(o gcOptions, pos []string) error {
	if len(pos) > 0 {
		return usagef("gc: unexpected argument %q", pos[0])
	}
	if o.all && len(o.paths) > 0 {
		return usagef("gc: --all and --path are mutually exclusive")
	}
	return nil
}

func parseGCFlags(g *globals, args []string) (gcOptions, error) {
	fs := subFlags("gc", g)
	var o gcOptions
	fs.BoolVar(&o.all, "all", false, "remove all UNUSED worktrees")
	fs.BoolVar(&o.keepSessions, "keep-sessions", false, "keep Pi session history")
	fs.Func("path", "worktree to remove (repeatable)", func(v string) error {
		o.paths = append(o.paths, v)
		return nil
	})
	pos, err := parseSub(fs, args)
	if err != nil {
		return o, err
	}
	return o, validateGCArgs(o, pos)
}

func promptGC(g *globals, selected []worktree) (bool, error) {
	if g.yes {
		return true, nil
	}
	return confirmGC(selected)
}

func selectAndConfirmGC(g *globals, wts []worktree, o gcOptions, ttl time.Duration) ([]worktree, error) {
	selected, err := gcSelection(wts, o, ttl)
	if err != nil || len(selected) == 0 {
		return selected, err
	}
	ok, err := promptGC(g, selected)
	if err != nil || !ok {
		return nil, err
	}
	return selected, nil
}

func cmdGC(g *globals, args []string) error {
	o, err := parseGCFlags(g, args)
	if err != nil {
		return err
	}
	root, ttl, wts, err := loadWorkspace(g)
	if err != nil {
		return err
	}
	selected, err := selectAndConfirmGC(g, wts, o, ttl)
	if err != nil {
		if errors.Is(err, errAborted) {
			if g.json {
				return gcReport(g, emptyGCResult(o.keepSessions))
			}
			return err
		}
		return err
	}
	if len(selected) == 0 {
		return gcReport(g, emptyGCResult(o.keepSessions))
	}
	return gcRemove(g, root, selected, o.keepSessions)
}

func promptCandidateSelection(candidates []worktree, now time.Time, ttl time.Duration) ([]worktree, error) {
	if !interactive() {
		return nil, usagef("gc: nothing selected; use --all or --path")
	}
	labels := make([]string, len(candidates))
	for i, w := range candidates {
		labels[i] = candidateLabel(w, now, ttl)
	}
	sel, err := multiSelect("Select worktrees to remove (space toggle, ctrl+a all)", labels)
	if err != nil {
		return nil, err
	}
	out := make([]worktree, 0, len(sel))
	for _, i := range sel {
		out = append(out, candidates[i])
	}
	return out, nil
}

// candidateLabel is the picker's informative line. Dirty state is shown so the
// user's keep/remove choice is informed; nothing is auto-filtered.
func candidateLabel(w worktree, now time.Time, ttl time.Duration) string {
	label := fmt.Sprintf("%s  (%s, %s", w.Path, w.branchName(), w.status(now, ttl))
	if w.Dirty {
		label += ", dirty"
	}
	return label + ")"
}

// gcSelection picks UNUSED, non-main worktrees: explicit --path wins, --all
// takes every candidate, otherwise an interactive multi-select. The main
// worktree is never a candidate — losing it would destroy the repo itself.
func gcSelection(wts []worktree, o gcOptions, ttl time.Duration) ([]worktree, error) {
	now := time.Now()
	if len(o.paths) > 0 {
		return gcByPath(wts, o.paths, ttl, now)
	}
	var candidates []worktree
	for _, w := range wts {
		if !w.Main && w.unused(now, ttl) {
			// Dirty is consumed by the picker's candidateLabel and the forced-
			// removal warning, so it is computed here for candidates, never for
			// every worktree in discover.
			w.Dirty = worktreeDirty(w.Path)
			candidates = append(candidates, w)
		}
	}
	if o.all || len(candidates) == 0 {
		return candidates, nil
	}
	return promptCandidateSelection(candidates, now, ttl)
}

func findWorktreeByPath(wts []worktree, want, raw string, now time.Time, ttl time.Duration) (*worktree, error) {
	for _, w := range wts {
		if w.Path != want {
			continue
		}
		if w.Main {
			return nil, fmt.Errorf("refusing to remove the main worktree %s", w.Path)
		}
		// loadWorkspace errors before --path resolution, so the ttlErr branch
		// that guarded this warning is dead; warn unconditionally instead.
		if !w.unused(now, ttl) {
			fmt.Fprintf(os.Stderr, "warning: %s is ACTIVE (%s)\n", w.Path, w.status(now, ttl))
		}
		// --path selection is consent, but the removal is still forced: carry
		// Dirty so removeOneWorktree can disclose uncommitted changes.
		w.Dirty = worktreeDirty(w.Path)
		return &w, nil
	}
	return nil, fmt.Errorf("--path %s: not a worktree of this repository", raw)
}

func appendPathCandidate(wts []worktree, p string, seen map[string]bool, now time.Time, ttl time.Duration) (*worktree, error) {
	want, err := canonicalAbs(p)
	if err != nil || seen[want] {
		return nil, err
	}
	seen[want] = true
	return findWorktreeByPath(wts, want, p, now, ttl)
}

func gcByPath(wts []worktree, paths []string, ttl time.Duration, now time.Time) ([]worktree, error) {
	seen := map[string]bool{}
	var out []worktree
	for _, p := range paths {
		w, err := appendPathCandidate(wts, p, seen, now, ttl)
		if err != nil {
			return nil, err
		}
		if w != nil {
			out = append(out, *w)
		}
	}
	return out, nil
}

func removeOneWorktree(root string, w worktree, keepSessions bool, res *gcResult, idx sessionIndex) {
	// worktreeRemove is always --force, so uncommitted changes must be
	// disclosed on stderr before they are destroyed (stdout stays machine-clean).
	if w.Dirty {
		fmt.Fprintf(os.Stderr, "warning: %s has uncommitted changes; forcing removal\n", w.Path)
	}
	if err := worktreeRemoveRecovering(root, w.Path); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: w.Path, Error: err.Error()})
		return
	}
	res.Removed = append(res.Removed, w.Path)
	if keepSessions {
		return
	}
	n, err := purgePiSessions(w.Path, idx)
	res.SessionsPurged += n
	if err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: w.Path, Error: "purge sessions: " + err.Error()})
	}
}

// worktreeRemoveRecovering removes a worktree, repairing a missing .git backlink
// first: git refuses `worktree remove` when the dir survives without it.
func worktreeRemoveRecovering(root, path string) error {
	err := worktreeRemove(root, path)
	if err == nil || !worktreeLinkMissing(path) {
		return err
	}
	if rerr := worktreeRepair(root); rerr != nil {
		return err // keep the original removal error
	}
	return worktreeRemove(root, path)
}

func gcRemove(g *globals, root string, selected []worktree, keepSessions bool) error {
	// One session walk for all removals instead of one per worktree.
	idx, err := indexPiSessions()
	if err != nil {
		return fmt.Errorf("index pi sessions: %w", err)
	}
	res := emptyGCResult(keepSessions)
	for _, w := range selected {
		removeOneWorktree(root, w, keepSessions, &res, idx)
	}
	// Unconditional: the old `len(res.Removed) > 0` gate skipped prune exactly when
	// every removal failed, leaving stale admin entries that wedge later runs.
	if err := worktreePrune(root); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: root, Error: "prune: " + err.Error()})
	} else {
		res.Pruned = true
	}
	if err := gcReport(g, res); err != nil {
		return err
	}
	if len(res.Failed) > 0 {
		return fmt.Errorf("%d worktree(s) failed", len(res.Failed))
	}
	return nil
}

func printGCHuman(res gcResult) {
	for _, p := range res.Removed {
		fmt.Printf("removed %s\n", p)
	}
	if res.Pruned {
		fmt.Println("pruned")
	}
	if res.KeptSessions {
		fmt.Println("sessions kept")
	} else {
		fmt.Printf("sessions purged: %d\n", res.SessionsPurged)
	}
	for _, f := range res.Failed {
		fmt.Fprintf(os.Stderr, "failed %s: %s\n", f.Path, f.Error)
	}
}

func gcReport(g *globals, res gcResult) error {
	if g.json {
		return printJSON(res)
	}
	if len(res.Removed) == 0 && len(res.Failed) == 0 {
		fmt.Println("nothing to do")
		return nil
	}
	printGCHuman(res)
	return nil
}
