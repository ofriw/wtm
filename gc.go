package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

// gc.go — remove UNUSED worktrees and their Pi session history.

type gcOptions struct {
	all          bool
	keepSessions bool
	keepRemote   bool
	paths        []string
}

type gcFailure struct {
	Path  string `json:"path"`
	Error string `json:"error"`
}

type gcResult struct {
	Removed        []string    `json:"removed"`
	RemoteDeleted  []string    `json:"remoteDeleted"`
	DeletedBranch  string      `json:"deletedBranch,omitempty"` // set only by `delete`
	Failed         []gcFailure `json:"failed"`
	SessionsPurged int         `json:"sessionsPurged"`
	KeptSessions   bool        `json:"keptSessions"`
	KeptRemote     bool        `json:"keptRemote"`
	Pruned         bool        `json:"pruned"`
}

// errAborted is returned when the user declines gc confirmation. The "aborted"
// message is already on stderr (see confirmGC), so dispatch maps it to exit 0
// silent rather than printing a redundant line.
var errAborted = errors.New("aborted")

// emptyGCResult is the zero-removal report shape: nil-length slices (not nil)
// so JSON serializes [] not null (pinned by TestGCReport).
func emptyGCResult(keptSessions, keptRemote bool) gcResult {
	return gcResult{Removed: []string{}, RemoteDeleted: []string{}, Failed: []gcFailure{}, KeptSessions: keptSessions, KeptRemote: keptRemote}
}

// abortGCResult is the report when gc stops before removing anything
// (declined confirm or empty selection): no remote branch was deleted, so a
// passed --keep-remote trivially kept them all. (On the removal path the same
// claim goes through plan.keptRemote, which additionally requires targets.)
func abortGCResult(o gcOptions) gcResult {
	return emptyGCResult(o.keepSessions, o.keepRemote)
}

// remotePlan is the single computation of which remote upstreams gc will
// delete: veto-free targets plus the veto reasons already disclosed. Computed
// once after selection; confirm, purge, and report all read it instead of
// re-threading root+surviving.
type remotePlan struct {
	keep    bool
	targets map[string]upstream // deduped delete candidates
	vetoes  map[string]string   // key -> skip reason, disclosed once each
}

// planRemotes collects the deduped delete candidates for selected.
// remoteDeleteVeto stays the single veto source; the plan only records it.
func planRemotes(selected, surviving []worktree, keepRemote bool, root string) remotePlan {
	plan := remotePlan{keep: keepRemote, targets: map[string]upstream{}, vetoes: map[string]string{}}
	for _, w := range selected {
		addRemoteCandidate(&plan, root, w, surviving)
	}
	return plan
}

// addRemoteCandidate folds one worktree into the plan: vetoed upstreams record
// their reason, the rest dedup by key. Planning stays offline: existence is
// probed once per target in attemptRemoteDelete, after consent, under the
// renderer — a pre-prompt probe would stall the picker on a slow remote.
func addRemoteCandidate(plan *remotePlan, root string, w worktree, surviving []worktree) {
	if !w.Upstream.onRemote() {
		return
	}
	key := w.Upstream.key()
	if plan.seen(key) {
		return
	}
	if reason := remoteDeleteVeto(root, w, surviving); reason != "" {
		plan.vetoes[key] = reason
		return
	}
	plan.targets[key] = w.Upstream
}

// seen reports whether key already has a plan entry (target or veto).
func (p remotePlan) seen(key string) bool {
	if _, ok := p.targets[key]; ok {
		return true
	}
	_, ok := p.vetoes[key]
	return ok
}

// count is the disclosed prompt count: deduped, veto-free targets. Existence
// is not probed here (that would put a network call before consent), so an
// upstream deleted out of band may still be counted — a conservative
// over-disclosure for a destructive operation. 0 keeps the prompt
// byte-identical to the pre-remote behavior, and --keep-remote discloses no
// deletes while still recording the keep below.
func (p remotePlan) count() int {
	if p.keep {
		return 0
	}
	return len(p.targets)
}

// keptRemote reports an actual keep, not the flag echo: --keep-remote with no
// remote upstreams keeps nothing, so it must not claim otherwise.
func (p remotePlan) keptRemote() bool {
	return p.keep && len(p.targets) > 0
}

func confirmGC(selected []worktree, plan remotePlan) (bool, error) {
	if !interactive() {
		return false, usagef("gc: confirmation requires a terminal; pass --yes")
	}

	title := gcPromptTitle(len(selected), plan.count())
	ok, err := confirm(title, false)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted")
		return false, errAborted
	}
	return ok, nil
}

func gcPromptTitle(worktrees, remotes int) string {
	if remotes == 0 {
		return fmt.Sprintf("Remove %d worktree(s)?", worktrees)
	}
	return fmt.Sprintf("Remove %d worktree(s) and delete %d remote branch(es)?", worktrees, remotes)
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
	fs.BoolVar(&o.keepRemote, "keep-remote", false, "keep remote upstream branches")
	fs.Func("path", "worktree to remove (repeatable)", func(v string) error {
		o.paths = append(o.paths, v)
		return nil
	})
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return o, err
	}
	return o, validateGCArgs(o, pos)
}

func promptGC(g *globals, selected []worktree, plan remotePlan) (bool, error) {
	if g.yes {
		return true, nil
	}
	return confirmGC(selected, plan)
}

// selectAndConfirmGC picks the worktrees to remove and gets consent. The
// candidate scan runs under the progress renderer; the huh picker and confirm
// run after it exits, because two tea programs must never share the tty. The
// remote plan is computed once here so confirm and purge share one source.
func selectAndConfirmGC(g *globals, wts []worktree, o gcOptions, ttl time.Duration, root string) ([]worktree, remotePlan, error) {
	var none remotePlan
	candidates, err := scanSelection(g, wts, o, ttl)
	if err != nil || len(candidates) == 0 {
		return candidates, none, err
	}
	selected, err := chooseCandidates(candidates, o, ttl)
	if err != nil || len(selected) == 0 {
		return selected, none, err
	}
	plan := planRemotes(selected, survivingWorktrees(wts, selected), o.keepRemote, root)
	ok, err := promptGC(g, selected, plan)
	if err != nil || !ok {
		return nil, none, err
	}
	return selected, plan, nil
}

// scanSelection runs the candidate scan under the renderer. An explicit --path
// is already fast and may warn on stderr, so it bypasses the renderer entirely.
func scanSelection(g *globals, wts []worktree, o gcOptions, ttl time.Duration) ([]worktree, error) {
	if len(o.paths) > 0 {
		return gcSelection(wts, o, ttl, nil)
	}
	var candidates []worktree
	err := withProgress(g, true, func(p *progress) error {
		var serr error
		candidates, serr = gcSelection(wts, o, ttl, p)
		return serr
	})
	return candidates, err
}

// chooseCandidates resolves the interactive picker for scanned candidates;
// explicit --path and --all selections are already final.
func chooseCandidates(candidates []worktree, o gcOptions, ttl time.Duration) ([]worktree, error) {
	if o.all || len(o.paths) > 0 {
		return candidates, nil
	}
	return promptCandidateSelection(candidates, time.Now(), ttl, o.keepRemote)
}

func handleGCAbort(g *globals, o gcOptions, err error) error {
	if errors.Is(err, errAborted) && g.json {
		return gcReport(g, abortGCResult(o))
	}
	return err
}

func cmdGC(g *globals, args []string) error {
	o, err := parseGCFlags(g, args)
	if err != nil {
		return err
	}
	root, ttl, wts, err := loadWorkspace(g, true)
	if err != nil {
		return err
	}
	selected, plan, err := selectAndConfirmGC(g, wts, o, ttl, root)
	if err != nil {
		return handleGCAbort(g, o, err)
	}
	if len(selected) == 0 {
		return gcReport(g, abortGCResult(o))
	}
	return runGCRemove(g, root, selected, plan, o)
}

// reportResult dispatches the report if removals, remote deletes, branch
// deletes or failures occurred, joining any report failure with the run
// error. The branch/remote arms exist for branch-only delete retries.
func reportResult(g *globals, res gcResult, runErr error) error {
	if len(res.Removed) > 0 || len(res.Failed) > 0 || len(res.RemoteDeleted) > 0 || res.DeletedBranch != "" {
		if rerr := gcReport(g, res); rerr != nil {
			if runErr != nil {
				return fmt.Errorf("%w; report: %w", runErr, rerr)
			}
			return rerr
		}
	}
	return runErr
}

// runGCRemove removes the selected worktrees under the renderer, then prints the
// report only after the renderer exits: a raw stdout write would land mid-frame
// and corrupt the inline display.
func runGCRemove(g *globals, root string, selected []worktree, plan remotePlan, o gcOptions) error {
	var res gcResult
	runErr := withProgress(g, true, func(p *progress) error {
		var rerr error
		res, rerr = gcRemove(root, selected, plan, o.keepSessions, p)
		return rerr
	})
	return reportResult(g, res, runErr)
}

func promptCandidateSelection(candidates []worktree, now time.Time, ttl time.Duration, keepRemote bool) ([]worktree, error) {
	if !interactive() {
		return nil, usagef("gc: nothing selected; use --all or --path")
	}
	header, body, _ := pickerGrid(candidates, columnContext{now: now, ttl: ttl}, pickerBudget())
	sel, err := multiSelect(gcPickerTitle(len(candidates)), gcPickerDescription(keepRemote, header), body)
	if err != nil {
		return nil, err
	}
	out := make([]worktree, 0, len(sel))
	for _, i := range sel {
		out = append(out, candidates[i])
	}
	return out, nil
}

// pickerGrid renders the candidate grid at a width budget. Budgeting the
// terminal minus huh's chrome and option prefix is mandatory: with either
// omitted, lipgloss wraps the row and the columns visibly misalign. The budget
// is a parameter so tests pin deterministic widths without a terminal.
func pickerGrid(candidates []worktree, ctx columnContext, budget int) (string, []string, []int) {
	headers := columnHeaders(gcPickerColumns)
	rows := gridRows(gcPickerColumns, candidates, ctx)
	headers, rows, widths, aligns := sizedGrid(headers, rows, budget, columnRightAligns(gcPickerColumns))
	header, body := renderGrid(headers, rows, widths, aligns)
	return header, body, widths
}

// pickerBudget is the label width huh can render without wrapping.
func pickerBudget() int {
	return max(0, widthBudget()-pickerChromeWidth-pickerOptionPrefixWidth)
}

// gcPickerDescription is the picker's description: the policy line, then the
// grid header indented to sit exactly above the option keys (huh indents only
// by its frame, not by the option prefix).
func gcPickerDescription(keepRemote bool, header string) string {
	return gcPickerPolicy(keepRemote) + "\n" + strings.Repeat(" ", pickerOptionPrefixWidth) + header
}

// gcPickerColumns is the picker's projection of the shared worktree grid.
// STATUS is dropped because every candidate is UNUSED (said once in the title),
// and UNCOMMITTED replaces the inventory columns: it is the only field whose
// value changes the consequence of the choice.
var gcPickerColumns = []worktreeColumn{colPath, colBranch, colLastUsed, colUncommitted, colUpstream}

// gcPickerTitle keeps the "Select worktrees to remove" contract (pinned by the
// TUI test) and states "unused" once, since every candidate is UNUSED.
func gcPickerTitle(n int) string {
	return fmt.Sprintf("Select worktrees to remove (%d unused, space toggle, ctrl+a all)", n)
}

// gcPickerPolicy discloses the destructive remote effect once, before consent,
// instead of repeating it on every row.
func gcPickerPolicy(keepRemote bool) string {
	if keepRemote {
		return "Remote branches are kept (--keep-remote)."
	}
	return "Remote upstream branches will be deleted; pass --keep-remote to keep them."
}

// gcSelection scans UNUSED, non-main worktrees: explicit --path wins, --all
// takes every scanned candidate. The interactive choice is deferred to
// chooseCandidates so it runs outside any progress renderer.
func gcSelection(wts []worktree, o gcOptions, ttl time.Duration, p *progress) ([]worktree, error) {
	now := time.Now()
	if len(o.paths) > 0 {
		return gcByPath(wts, o.paths, ttl, now)
	}
	candidates := unusedCandidates(wts, now, ttl)
	markDirty(candidates, p)
	return candidates, nil
}

// unusedCandidates filters to non-main UNUSED worktrees; the main worktree is
// never a candidate — losing it would destroy the repo itself.
func unusedCandidates(wts []worktree, now time.Time, ttl time.Duration) []worktree {
	var candidates []worktree
	for _, w := range wts {
		if !w.Main && w.unused(now, ttl) {
			candidates = append(candidates, w)
		}
	}
	return candidates
}

// markDirty computes UNCOMMITTED for each candidate under the phase; the picker
// column and the forced-removal warning are its only consumers, so it is never
// computed for every worktree in discover.
func markDirty(candidates []worktree, p *progress) {
	p.phase("checking candidates", len(candidates))
	for i := range candidates {
		candidates[i].Dirty = worktreeDirty(candidates[i].Path)
		p.advance(1)
		p.detail(displayPath(candidates[i].Path))
	}
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
			warnf("%s is ACTIVE (%s)", w.Path, w.status(now, ttl))
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

func removeOneWorktree(root string, w worktree, plan remotePlan, keepSessions bool, res *gcResult, idx sessionIndex, p *progress, outcomes map[string]error) {
	// worktreeRemove is always --force, so uncommitted changes must be
	// disclosed before they are destroyed (stdout stays machine-clean).
	if w.Dirty {
		warnProgress(p, "%s has uncommitted changes; forcing removal", w.Path)
	}
	if err := worktreeRemoveRecovering(root, w.Path); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: w.Path, Error: err.Error()})
		return
	}
	res.Removed = append(res.Removed, w.Path)
	p.detail(displayPath(w.Path))
	purgeWorktree(root, w, plan, keepSessions, res, idx, p, outcomes)
}

// purgeWorktree cleans up after a successful removal: the remote upstream
// (unless kept), then the Pi session history (unless kept).
func purgeWorktree(root string, w worktree, plan remotePlan, keepSessions bool, res *gcResult, idx sessionIndex, p *progress, outcomes map[string]error) {
	// Remote delete runs only after the local worktree is gone, so a failed
	// local removal never orphans a still-needed remote branch.
	if !plan.keep && w.Upstream.onRemote() {
		purgeRemoteWorktree(root, w, plan, res, p, outcomes)
	}
	if !keepSessions {
		purgeWorktreeSessions(w.Path, res, idx)
	}
}

// remoteDeleteVeto is the single source of truth for "gc must not delete this
// upstream": "" means delete, anything else is the human-readable skip reason.
// The plan records it once; purgeRemoteWorktree reads the plan.
func remoteDeleteVeto(root string, w worktree, surviving []worktree) string {
	if isRemoteDefaultBranch(root, w.Upstream) {
		return fmt.Sprintf("it is the default branch of %s", w.Upstream.Remote)
	}
	if survivingPath, tracked := survivingUpstreamTracked(surviving, w.Upstream); tracked {
		return fmt.Sprintf("still tracked by %s", survivingPath)
	}
	return ""
}

func purgeRemoteWorktree(root string, w worktree, plan remotePlan, res *gcResult, p *progress, outcomes map[string]error) {
	// A vetoed upstream is skipped with the reason disclosed once per upstream,
	// since sharers would otherwise repeat the same warning.
	key := w.Upstream.key()
	if reason, ok := plan.vetoes[key]; ok {
		if _, seen := outcomes[key]; !seen {
			outcomes[key] = nil
			warnProgress(p, "skipping remote delete %s: %s", w.Upstream.Short, reason)
		}
		return
	}
	// Guard against plan/purge desync: every onRemote non-vetoed upstream of a
	// non-kept run is a planned target, so a missing key must never delete.
	if _, ok := plan.targets[key]; !ok {
		return
	}
	deleteRemote(root, w, res, p, outcomes)
}

func purgeWorktreeSessions(path string, res *gcResult, idx sessionIndex) {
	n, err := purgePiSessions(path, idx)
	res.SessionsPurged += n
	if err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: path, Error: "purge sessions: " + err.Error()})
	}
}

func failRemote(res *gcResult, path, short string, err error) {
	res.Failed = append(res.Failed, gcFailure{Path: path, Error: "delete remote " + short + ": " + err.Error()})
}

// isRemoteDefaultBranch reports whether u tracks the remote's default branch,
// guarded first by the remote's recorded HEAD symref and falling back to the
// repository default branch so cloned repos without symrefs stay protected.
func isRemoteDefaultBranch(root string, u upstream) bool {
	if root == "" {
		return false
	}
	if defRef := remoteDefaultBranchRef(root, u.Remote); defRef != "" {
		return u.Ref == defRef
	}
	// A remote without a locally recorded HEAD cannot be resolved offline, so
	// protect the repo-wide default name on ANY remote. This over-protects a
	// coincidentally-named branch but can never delete a shared default branch.
	name, _ := defaultBranch(root)
	if name != "" {
		return u.Ref == "refs/heads/"+name
	}
	return false
}

// survivingUpstreamTracked reports whether u is still tracked by a surviving
// worktree (one not selected for removal in this gc run).
func survivingUpstreamTracked(surviving []worktree, u upstream) (string, bool) {
	for _, s := range surviving {
		if s.Upstream.onRemote() && s.Upstream.Remote == u.Remote && s.Upstream.Ref == u.Ref {
			return s.Path, true
		}
	}
	return "", false
}

// survivingWorktrees returns all worktrees from all that are not in selected.
func survivingWorktrees(all, selected []worktree) []worktree {
	sel := make(map[string]bool, len(selected))
	for _, w := range selected {
		sel[w.Path] = true
	}
	var surviving []worktree
	for _, w := range all {
		if !sel[w.Path] {
			surviving = append(surviving, w)
		}
	}
	return surviving
}

// deleteRemote removes one worktree branch's upstream branch. An upstream ref
// that is already gone is a converged no-op, not a failure: a branch deleted
// out of band (or by an interrupted run) must leave a retried gc exiting 0.
// Every other error is fatal and lands in res.Failed once per upstream key —
// the first worktree records it, the rest skip it — so one rejected delete
// yields one entry and the failed count never multiplies by sharer.
func deleteRemote(root string, w worktree, res *gcResult, p *progress, outcomes map[string]error) {
	key := w.Upstream.key()
	if _, seen := outcomes[key]; seen {
		return
	}
	outcomes[key] = attemptRemoteDelete(root, w, res, p)
}

// attemptRemoteDelete performs one upstream delete; the result is the shared
// outcome deleteRemote records. Nil means deleted or already gone.
func attemptRemoteDelete(root string, w worktree, res *gcResult, p *progress) error {
	exists, err := remoteRefExists(root, w.Upstream.Remote, w.Upstream.Ref)
	if err != nil {
		failRemote(res, w.Path, w.Upstream.Short, err)
		return err
	}
	if !exists {
		return nil
	}
	// A notice is disclosed before the destructive delete, which --path/--yes
	// performs without a prompt.
	noticeProgress(p, "deleting remote %s", w.Upstream.Short)
	if err := deleteRemoteBranch(root, w.Upstream.Remote, w.Upstream.Ref); err != nil {
		failRemote(res, w.Path, w.Upstream.Short, err)
		return err
	}
	res.RemoteDeleted = append(res.RemoteDeleted, w.Upstream.Short)
	return nil
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

func gcRemove(root string, selected []worktree, plan remotePlan, keepSessions bool, p *progress) (gcResult, error) {
	res := emptyGCResult(keepSessions, plan.keptRemote())
	// One session walk for all removals instead of one per worktree.
	p.phase("indexing sessions", 0)
	idx, err := indexPiSessions()
	if err != nil {
		return res, fmt.Errorf("index pi sessions: %w", err)
	}
	removeSelected(root, selected, plan, keepSessions, &res, idx, p)
	pruneWorktrees(&res, root, p)
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%d worktree(s) failed", len(res.Failed))
	}
	return res, nil
}

// removeSelected removes each selected worktree, sharing one session index
// and one per-upstream remote outcome across the run.
func removeSelected(root string, selected []worktree, plan remotePlan, keepSessions bool, res *gcResult, idx sessionIndex, p *progress) {
	p.phase("removing worktrees", len(selected))
	outcomes := map[string]error{}
	for _, w := range selected {
		removeOneWorktree(root, w, plan, keepSessions, res, idx, p, outcomes)
		p.advance(1)
	}
}

// pruneWorktrees runs prune unconditionally: the old `len(res.Removed) > 0`
// gate skipped it exactly when every removal failed, leaving stale admin
// entries that wedge later runs.
func pruneWorktrees(res *gcResult, root string, p *progress) {
	p.phase("pruning", 0)
	if err := worktreePrune(root); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: root, Error: "prune: " + err.Error()})
		return
	}
	res.Pruned = true
}

func printGCOutcomes(res gcResult) {
	for _, p := range res.Removed {
		fmt.Printf("removed %s\n", p)
	}
	for _, u := range res.RemoteDeleted {
		fmt.Printf("deleted remote %s\n", u)
	}
	if res.DeletedBranch != "" {
		fmt.Printf("deleted branch %s\n", res.DeletedBranch)
	}
	if res.Pruned {
		fmt.Println("pruned")
	}
}

func printGCSummary(res gcResult) {
	printGCOutcomes(res)
	if res.KeptSessions {
		fmt.Println("sessions kept")
	} else {
		fmt.Printf("sessions purged: %d\n", res.SessionsPurged)
	}
	if res.KeptRemote {
		fmt.Println("remote branches kept")
	}
}

func printGCHuman(res gcResult) {
	printGCSummary(res)
	for _, f := range res.Failed {
		fmt.Fprintf(os.Stderr, "failed %s: %s\n", f.Path, f.Error)
	}
}

func gcReport(g *globals, res gcResult) error {
	if g.json {
		return printJSON(res)
	}
	if len(res.Removed) == 0 && len(res.Failed) == 0 && len(res.RemoteDeleted) == 0 && res.DeletedBranch == "" {
		fmt.Println("nothing to do")
		return nil
	}
	printGCHuman(res)
	return nil
}
