package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/temp"
	"wtm/internal/ui"
)

// gc.go — remove UNUSED worktrees and their agent session history.

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
	Removed         []string `json:"removed"`
	RemoteDeleted   []string `json:"remoteDeleted"`
	DeletedBranch   string   `json:"deletedBranch,omitempty"` // set only by `delete`
	DeletedBranches []string `json:"deletedBranches,omitempty"`
	// Skipped lists candidates the locked verification refused to remove;
	// reportRemovalError owns the skip reasons. stderr carries the warning,
	// JSON carries the fact.
	Skipped        []string    `json:"skipped,omitempty"`
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

// emptyGCResult is the zero-removal report shape. Removed, RemoteDeleted and
// Failed carry no omitempty, so they must be non-nil to serialize as [] and
// not null. Skipped and DeletedBranches carry omitempty and stay absent until
// something is skipped or a temp branch is deleted (pinned by TestGCReport).
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
// once after selection; ui.Confirm, purge, and report all read it instead of
// re-threading root+surviving.
type remotePlan struct {
	keep    bool
	targets map[string]git.Upstream // deduped delete candidates
	vetoes  map[string]string       // key -> skip reason, disclosed once each
}

// planRemotes collects the deduped delete candidates for selected.
// remoteDeleteVeto stays the single veto source; the plan only records it.
func planRemotes(selected, surviving []core.Worktree, keepRemote bool, root string) remotePlan {
	plan := remotePlan{keep: keepRemote, targets: map[string]git.Upstream{}, vetoes: map[string]string{}}
	for _, w := range selected {
		addRemoteCandidate(&plan, root, w, surviving)
	}
	return plan
}

// addRemoteCandidate folds one worktree into the plan: vetoed upstreams record
// their reason, the rest dedup by key. Planning stays offline: existence is
// probed once per target in attemptRemoteDelete, after consent, under the
// renderer — a pre-prompt probe would stall the picker on a slow remote.
func addRemoteCandidate(plan *remotePlan, root string, w core.Worktree, surviving []core.Worktree) {
	if !w.Upstream.OnRemote() {
		return
	}
	key := w.Upstream.Key()
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

func confirmGC(selected []core.Worktree, plan remotePlan) (bool, error) {
	if !ui.Interactive() {
		return false, usagef("gc: confirmation requires a terminal; pass --yes")
	}

	title := gcConsentTitle(selected, plan)
	ok, err := ui.Confirm(title, false)
	if err != nil {
		return false, err
	}
	if !ok {
		fmt.Fprintln(os.Stderr, "aborted")
		return false, errAborted
	}
	return ok, nil
}

// gcPromptBase returns the consent prompt without the trailing question mark,
// so callers can append clauses before the final punctuation.
func gcPromptBase(wtCount, remotes int) string {
	if remotes == 0 {
		return fmt.Sprintf("Remove %d worktree(s)", wtCount)
	}
	return fmt.Sprintf("Remove %d worktree(s) and delete %d remote branch(es)", wtCount, remotes)
}

func gcConsentTitle(selected []core.Worktree, plan remotePlan) string {
	base := gcPromptBase(len(selected), plan.count())
	branches := tempBranchNames(selected)
	if len(branches) == 0 {
		return base + "?"
	}
	return base + "; force-delete temp LOCAL branches: " + strings.Join(branches, ", ") + "?"
}

func tempBranchNames(selected []core.Worktree) []string {
	var branches []string
	for _, w := range selected {
		if w.Temp {
			branches = append(branches, w.Branch)
		}
	}
	return branches
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
	flags := subFlags("gc", g)
	var o gcOptions
	flags.BoolVar(&o.all, "all", false, "remove all UNUSED worktrees")
	flags.BoolVar(&o.keepSessions, "keep-sessions", false, "keep agent session history")
	flags.BoolVar(&o.keepRemote, "keep-remote", false, "keep remote upstream branches")
	flags.Func("path", "worktree to remove (repeatable)", func(v string) error {
		o.paths = append(o.paths, v)
		return nil
	})
	pos, err := parseSub(flags, g, args, usage)
	if err != nil {
		return o, err
	}
	return o, validateGCArgs(o, pos)
}

func promptGC(g *globals, selected []core.Worktree, plan remotePlan) (bool, error) {
	if g.Yes {
		return true, nil
	}
	return confirmGC(selected, plan)
}

// selectAndConfirmGC picks the worktrees to remove and gets consent. The
// candidate scan runs under the progress renderer; the huh picker and ui.Confirm
// run after it exits, because two tea programs must never share the tty. The
// remote plan is computed once here so ui.Confirm and purge share one source.
func selectAndConfirmGC(g *globals, wts []core.Worktree, o gcOptions, ttl time.Duration, root string, now time.Time) ([]core.Worktree, remotePlan, error) {
	var none remotePlan
	candidates, err := scanSelection(g, wts, o, ttl, now)
	if err != nil || len(candidates) == 0 {
		return candidates, none, err
	}
	selected, err := chooseCandidates(candidates, o, ttl, now)
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
func scanSelection(g *globals, wts []core.Worktree, o gcOptions, ttl time.Duration, now time.Time) ([]core.Worktree, error) {
	if len(o.paths) > 0 {
		return gcSelection(wts, o, ttl, now, nil)
	}
	var candidates []core.Worktree
	err := withProgress(g, true, func(p *ui.Progress) error {
		var serr error
		candidates, serr = gcSelection(wts, o, ttl, now, p)
		return serr
	})
	return candidates, err
}

// chooseCandidates resolves the ui.Interactive picker for scanned candidates;
// explicit --path and --all selections are already final.
func chooseCandidates(candidates []core.Worktree, o gcOptions, ttl time.Duration, now time.Time) ([]core.Worktree, error) {
	if o.all || len(o.paths) > 0 {
		return candidates, nil
	}
	return promptCandidateSelection(candidates, now, ttl, o.keepRemote)
}

func handleGCAbort(g *globals, o gcOptions, err error) error {
	if errors.Is(err, errAborted) && g.JSON {
		return gcReport(g, abortGCResult(o))
	}
	return err
}

func cmdGC(g *globals, args []string) error {
	o, err := parseGCFlags(g, args)
	if err != nil {
		return err
	}
	// Reconcile before discovery so missing checkouts cannot remain candidates.
	reconcileGCTempStore()
	ws, err := loadGCWorkspace(g, o.paths)
	if err != nil {
		return err
	}
	// Selection and the fresh under-lock idle check share one clock reading.
	now := time.Now()
	selected, plan, err := selectAndConfirmGC(g, ws.Wts, o, ws.TTL, ws.Root, now)
	if err != nil {
		return handleGCAbort(g, o, err)
	}
	if len(selected) == 0 {
		return gcReport(g, abortGCResult(o))
	}
	return runGCRemove(g, ws.Root, selected, plan, o, ws.Idx, now)
}

// A vanished checkout leaves only stale metadata; failure must not block gc.
func reconcileGCTempStore() {
	if err := temp.ReconcileTempStore(); err != nil {
		ui.Warnf("temp store reconciliation: %v; continuing with current records", err)
	}
}

// reportResult dispatches the report if removals, remote deletes, branch
// deletes or failures occurred, joining any report failure with the run
// error. The branch/remote arms exist for branch-only delete retries.
func reportResult(g *globals, res gcResult, runErr error) error {
	if len(res.Removed) > 0 || len(res.Failed) > 0 || len(res.Skipped) > 0 || len(res.RemoteDeleted) > 0 || len(res.DeletedBranches) > 0 || res.DeletedBranch != "" {
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
func runGCRemove(g *globals, root string, selected []core.Worktree, plan remotePlan, o gcOptions, idx core.SessionIndex, now time.Time) error {
	var res gcResult
	runErr := withProgress(g, true, func(p *ui.Progress) error {
		var rerr error
		res, rerr = gcCleanup(root, selected, plan, o.keepSessions, idx, gcMode{deleteTemp: true, idleOnly: len(o.paths) == 0}, now, p)
		return rerr
	})
	return reportResult(g, res, runErr)
}

func promptCandidateSelection(candidates []core.Worktree, now time.Time, ttl time.Duration, keepRemote bool) ([]core.Worktree, error) {
	if !ui.Interactive() {
		return nil, usagef("gc: nothing selected; use --all or --path")
	}
	header, body, _ := pickerGrid(candidates, core.ColumnContext{Now: now, TTL: ttl}, pickerBudget())
	sel, err := ui.MultiSelect(gcPickerTitle(len(candidates)), gcPickerDescription(candidates, keepRemote, header), body)
	if err != nil {
		return nil, err
	}
	out := make([]core.Worktree, 0, len(sel))
	for _, i := range sel {
		out = append(out, candidates[i])
	}
	return out, nil
}

// pickerGrid renders the candidate grid at a width budget. Budgeting the
// terminal minus huh's chrome and option prefix is mandatory: with either
// omitted, lipgloss wraps the row and the columns visibly misalign. The budget
// is a parameter so tests pin deterministic widths without a terminal.
func pickerGrid(candidates []core.Worktree, ctx core.ColumnContext, budget int) (string, []string, []int) {
	cols := core.PickerColumns()
	headers := core.ColumnHeaders(cols)
	rows := core.GridRows(cols, candidates, ctx)
	policy := core.NewGridPolicy(headers)
	headers, rows, widths, aligns := ui.SizedGrid(headers, rows, budget, core.ColumnRightAligns(cols), policy)
	header, body := ui.RenderGrid(headers, rows, widths, aligns)
	return header, body, widths
}

// pickerBudget is the label width huh can render without wrapping.
func pickerBudget() int {
	return max(0, ui.WidthBudget()-ui.PickerChromeWidth-ui.PickerOptionPrefixWidth)
}

// gcPickerDescription is the picker's description: the policy line, then the
// grid header indented to sit exactly above the option keys (huh indents only
// by its frame, not by the option prefix). The temp clause appears only when
// a temp candidate is present, mirroring gcConsentTitle.
func gcPickerDescription(candidates []core.Worktree, keepRemote bool, header string) string {
	return gcPickerPolicy(keepRemote, len(tempBranchNames(candidates)) > 0) + "\n" + strings.Repeat(" ", ui.PickerOptionPrefixWidth) + header
}

// gcPickerTitle keeps the "Select worktrees to remove" contract (pinned by the
// TUI test) and states "unused" once, since every candidate is UNUSED.
func gcPickerTitle(n int) string {
	return fmt.Sprintf("Select worktrees to remove (%d unused, space toggle, ctrl+a all)", n)
}

// gcPickerPolicy discloses the destructive remote effect once, before consent,
// instead of repeating it on every row. The temp clause is conditional: with
// no temp candidates there is no local branch to force-delete, so claiming
// one would cry wolf and train users to skip the prompt.
func gcPickerPolicy(keepRemote, hasTemp bool) string {
	local := ""
	if hasTemp {
		local = "Temp LOCAL branches will be force-deleted after cleanup succeeds. "
	}
	if keepRemote {
		return local + "Remote branches are kept (--keep-remote)."
	}
	return local + "Remote upstream branches will be deleted; pass --keep-remote to keep them."
}

// gcSelection scans UNUSED, non-main worktrees: explicit --path wins, --all
// takes every scanned candidate. The ui.Interactive choice is deferred to
// chooseCandidates so it runs outside any progress renderer.
func gcSelection(wts []core.Worktree, o gcOptions, ttl time.Duration, now time.Time, p *ui.Progress) ([]core.Worktree, error) {
	if len(o.paths) > 0 {
		return gcByPath(wts, o.paths, ttl, now)
	}
	candidates := unusedCandidates(wts, now, ttl)
	markDirty(candidates, p)
	return candidates, nil
}

// unusedCandidates filters to non-main UNUSED worktrees; the main worktree is
// never a candidate — losing it would destroy the repo itself.
func unusedCandidates(wts []core.Worktree, now time.Time, ttl time.Duration) []core.Worktree {
	var candidates []core.Worktree
	for _, w := range wts {
		if !w.Main && w.Unused(now, ttl) {
			candidates = append(candidates, w)
		}
	}
	return candidates
}

// markDirty computes UNCOMMITTED for each candidate under the phase; the picker
// column and the forced-removal warning are its only consumers, so it is never
// computed for every worktree in discover.
func markDirty(candidates []core.Worktree, p *ui.Progress) {
	p.Phase("checking candidates", len(candidates))
	for i := range candidates {
		candidates[i].Dirty = git.WorktreeDirty(candidates[i].Path)
		p.Advance(1)
		p.Detail(core.DisplayPath(candidates[i].Path))
	}
}

func findWorktreeByPath(wts []core.Worktree, want, raw string, now time.Time, ttl time.Duration) (*core.Worktree, error) {
	for _, w := range wts {
		if w.Path != want {
			continue
		}
		if w.Main {
			return nil, fmt.Errorf("refusing to remove the main worktree %s", w.Path)
		}
		// loadWorkspace errors before --path resolution, so the ttlErr branch
		// that guarded this warning is dead; warn unconditionally instead.
		if !w.Unused(now, ttl) {
			ui.Warnf("%s is ACTIVE (%s)", w.Path, w.Status(now, ttl))
		}
		// --path selection is consent, but the removal is still forced: carry
		// Dirty so removeOneWorktree can disclose uncommitted changes.
		w.Dirty = git.WorktreeDirty(w.Path)
		return &w, nil
	}
	return nil, fmt.Errorf("--path %s: not a worktree of this repository", raw)
}

func appendPathCandidate(wts []core.Worktree, p string, seen map[string]bool, now time.Time, ttl time.Duration) (*core.Worktree, error) {
	want, err := fs.CanonicalAbs(p)
	if err != nil || seen[want] {
		return nil, err
	}
	seen[want] = true
	return findWorktreeByPath(wts, want, p, now, ttl)
}

func gcByPath(wts []core.Worktree, paths []string, ttl time.Duration, now time.Time) ([]core.Worktree, error) {
	seen := map[string]bool{}
	var out []core.Worktree
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

func removeOneWorktree(root string, w core.Worktree, idleOnly bool, now time.Time, res *gcResult, p *ui.Progress) bool {
	removed, stale, err := removeVerifiedWorktree(root, w, idleOnly, now)
	reportRemovalError(w, err, res, p)
	// Stale unverified sessions cannot change the verdict; disclose the ignores.
	for _, path := range stale {
		ui.WarnProgress(p, "unverified agent session %s predates known activity; ignoring", path)
	}
	if !removed {
		return false
	}
	// git.WorktreeRemove is always --force, so uncommitted changes are lost.
	// Report only once the verdict is known: a skipped checkout lost nothing,
	// and claiming a forced removal there would contradict the skip warning.
	if w.Dirty {
		ui.WarnProgress(p, "%s had uncommitted changes; removed anyway", w.Path)
	}
	res.Removed = append(res.Removed, w.Path)
	p.Detail(core.DisplayPath(w.Path))
	return true
}

// reportRemovalError classifies temp races as skips: promotion or identity
// drift between discovery and locked verification is not a tooling failure.
func reportRemovalError(w core.Worktree, err error, res *gcResult, p *ui.Progress) {
	if errors.Is(err, temp.ErrTempActive) || errors.Is(err, temp.ErrTempUnverified) || errors.Is(err, temp.ErrTempPromoted) || errors.Is(err, temp.ErrTempMetadataChanged) {
		ui.WarnProgress(p, "skipping %s: %s", w.Path, err)
		res.Skipped = append(res.Skipped, w.Path)
	} else if err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: w.Path, Error: err.Error()})
	}
}

func removeVerifiedWorktree(root string, w core.Worktree, idleOnly bool, now time.Time) (removed bool, stale []string, err error) {
	if w.Temp {
		// Temp verification needs git metadata, so an out-of-band .git backlink
		// must be repaired before the identity check, not after removal fails.
		if git.WorktreeLinkMissing(w.Path) {
			if err := git.WorktreeRepair(root); err != nil {
				return false, nil, err
			}
		}
		return temp.RemoveTempWorktreeIf(w.Path, w.Branch, w.TempIdentity, idleOnly, now, harness.FreshWorktreeActivity)
	}
	removed, err = temp.RemovePermanentWorktree(root, w.Path)
	return removed, nil, err
}

func purgeTempBranch(root string, w core.Worktree, res *gcResult) {
	if !w.Temp {
		return
	}
	if err := git.BranchDelete(root, w.Branch); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: w.Path, Error: "delete branch " + w.Branch + ": " + err.Error()})
	} else {
		res.DeletedBranches = append(res.DeletedBranches, w.Branch)
	}
}

// purgeWorktree cleans up after a successful removal: the remote upstream
// (unless kept), then the agent session history (unless kept).
func purgeWorktree(root string, w core.Worktree, plan remotePlan, keepSessions bool, res *gcResult, idx core.SessionIndex, p *ui.Progress, outcomes map[string]error) {
	// Remote delete runs only after the local worktree is gone, so a failed
	// local removal never orphans a still-needed remote branch.
	if !plan.keep && w.Upstream.OnRemote() {
		purgeRemoteWorktree(root, w, plan, res, p, outcomes)
	}
	if !keepSessions {
		purgeWorktreeSessions(w.Path, res, idx)
	}
}

// remoteDeleteVeto is the single source of truth for "gc must not delete this
// upstream": "" means delete, anything else is the human-readable skip reason.
// The plan records it once; purgeRemoteWorktree reads the plan.
func remoteDeleteVeto(root string, w core.Worktree, surviving []core.Worktree) string {
	if isRemoteDefaultBranch(root, w.Upstream) {
		return fmt.Sprintf("it is the default branch of %s", w.Upstream.Remote)
	}
	if survivingPath, tracked := survivingUpstreamTracked(surviving, w.Upstream); tracked {
		return fmt.Sprintf("still tracked by %s", survivingPath)
	}
	return ""
}

func purgeRemoteWorktree(root string, w core.Worktree, plan remotePlan, res *gcResult, p *ui.Progress, outcomes map[string]error) {
	// A vetoed upstream is skipped with the reason disclosed once per upstream,
	// since sharers would otherwise repeat the same warning.
	key := w.Upstream.Key()
	if reason, ok := plan.vetoes[key]; ok {
		if _, seen := outcomes[key]; !seen {
			outcomes[key] = nil
			ui.WarnProgress(p, "skipping remote delete %s: %s", w.Upstream.Short, reason)
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

func purgeWorktreeSessions(path string, res *gcResult, idx core.SessionIndex) {
	n, err := harness.PurgeSessions(path, idx)
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
func isRemoteDefaultBranch(root string, u git.Upstream) bool {
	if root == "" {
		return false
	}
	if defRef := git.RemoteDefaultBranchRef(root, u.Remote); defRef != "" {
		return u.Ref == defRef
	}
	// A remote without a locally recorded HEAD cannot be resolved offline, so
	// protect the repo-wide default name on ANY remote. This over-protects a
	// coincidentally-named branch but can never delete a shared default branch.
	name, _ := git.DefaultBranch(root)
	if name != "" {
		return u.Ref == "refs/heads/"+name
	}
	return false
}

// survivingUpstreamTracked reports whether u is still tracked by a surviving
// worktree (one not selected for removal in this gc run).
func survivingUpstreamTracked(surviving []core.Worktree, u git.Upstream) (string, bool) {
	for _, s := range surviving {
		if s.Upstream.OnRemote() && s.Upstream.Remote == u.Remote && s.Upstream.Ref == u.Ref {
			return s.Path, true
		}
	}
	return "", false
}

// survivingWorktrees returns worktrees from all that are not in selected. Used
// both for pre-removal planning (all minus selected) and post-removal protection
// (selected minus removed) — the function is generic over both.
func survivingWorktrees(all, selected []core.Worktree) []core.Worktree {
	sel := make(map[string]bool, len(selected))
	for _, w := range selected {
		sel[w.Path] = true
	}
	var surviving []core.Worktree
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
// The delete runs once per upstream and the failure is reported once, except
// that every affected temp checkout also records it, so each keeps its own
// local recovery branch (permanent checkouts never delete one).
func deleteRemote(root string, w core.Worktree, res *gcResult, p *ui.Progress, outcomes map[string]error) {
	key := w.Upstream.Key()
	err, seen := outcomes[key]
	if !seen {
		err = attemptRemoteDelete(root, w, res, p)
		outcomes[key] = err
	}
	if err != nil && (!seen || w.Temp) {
		failRemote(res, w.Path, w.Upstream.Short, err)
	}
}

// attemptRemoteDelete performs one upstream delete; the result is the shared
// outcome deleteRemote records. Nil means deleted or already gone.
func attemptRemoteDelete(root string, w core.Worktree, res *gcResult, p *ui.Progress) error {
	remoteExists, err := git.RemoteRefExists(root, w.Upstream.Remote, w.Upstream.Ref)
	if err != nil {
		return err
	}
	if !remoteExists {
		return nil
	}
	// A notice is disclosed before the destructive delete, which --path/--yes
	// performs without a prompt.
	ui.NoticeProgress(p, "deleting remote %s", w.Upstream.Short)
	if err := git.DeleteRemoteBranch(root, w.Upstream.Remote, w.Upstream.Ref); err != nil {
		return err
	}
	res.RemoteDeleted = append(res.RemoteDeleted, w.Upstream.Short)
	return nil
}

// gcMode selects the removal policy. idleOnly rechecks temp activity under the
// registry lock; deleteTemp deletes the local temp branch after cleanup.
type gcMode struct {
	deleteTemp bool
	idleOnly   bool
}

// Discovery supplies the purge index. Temp removal separately reads fresh
// activity under its registry lock; cached evidence never authorizes removal.
func gcCleanup(root string, selected []core.Worktree, plan remotePlan, keepSessions bool, idx core.SessionIndex, mode gcMode, now time.Time, p *ui.Progress) (gcResult, error) {
	res := emptyGCResult(keepSessions, plan.keptRemote())
	removeSelected(root, selected, plan, keepSessions, mode, now, &res, idx, p)
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%d cleanup failure(s)", len(res.Failed))
	}
	return res, nil
}

// removeSelected removes each selected worktree, sharing one session index
// and one per-upstream remote outcome across the run.
func removeSelected(root string, selected []core.Worktree, plan remotePlan, keepSessions bool, mode gcMode, now time.Time, res *gcResult, idx core.SessionIndex, p *ui.Progress) {
	p.Phase("removing worktrees", len(selected))
	var removed []core.Worktree
	for _, w := range selected {
		if removeOneWorktree(root, w, mode.idleOnly, now, res, p) {
			removed = append(removed, w)
		}
		p.Advance(1)
	}
	// Resolve survivors before any remote delete: even a later ACTIVE skip
	// must protect its upstream from an earlier selected checkout.
	protectRemovalSurvivors(root, removed, survivingWorktrees(selected, removed), &plan)
	finishSelectedCleanup(root, selected, removed, plan, keepSessions, mode, res, idx, p)
}

func protectRemovalSurvivors(root string, removed, survivors []core.Worktree, plan *remotePlan) {
	if plan.vetoes == nil {
		plan.vetoes = map[string]string{}
	}
	for _, w := range removed {
		if w.Upstream.OnRemote() {
			if reason := remoteDeleteVeto(root, w, survivors); reason != "" {
				plan.vetoes[w.Upstream.Key()] = reason
			}
		}
	}
}

func finishSelectedCleanup(root string, selected, removed []core.Worktree, plan remotePlan, keepSessions bool, mode gcMode, res *gcResult, idx core.SessionIndex, p *ui.Progress) {
	outcomes := map[string]error{}
	for _, w := range removed {
		purgeWorktree(root, w, plan, keepSessions, res, idx, p, outcomes)
	}
	// Prune precedes branch deletion; finishTempCleanup defines recovery policy.
	pruneWorktrees(res, root, p)
	if !mode.deleteTemp {
		return
	}
	removedSet := pathSet(res.Removed)
	for _, w := range selected {
		if w.Temp && (removedSet[w.Path] || worktreeCleanupFailed(w.Path, res)) {
			finishTempCleanup(root, w, res, p)
		}
	}
}

func pathSet(paths []string) map[string]bool {
	s := make(map[string]bool, len(paths))
	for _, p := range paths {
		s[p] = true
	}
	return s
}

func worktreeCleanupFailed(path string, res *gcResult) bool {
	for _, failure := range res.Failed {
		if failure.Path == path {
			return true
		}
	}
	return false
}

// Only this checkout's own cleanup failure keeps its branch. Prune is
// repo-global, so a prune failure vetoing every temp branch would orphan
// branches whose records are already gone and no later gc can reclaim.
func finishTempCleanup(root string, w core.Worktree, res *gcResult, p *ui.Progress) {
	if worktreeCleanupFailed(w.Path, res) {
		ui.WarnProgress(p, "kept local branch %s due to cleanup failure; fix the cause and complete failed cleanup, then retry `wtm gc --path %q` if the checkout remains, otherwise `wtm delete %q --yes`", w.Branch, w.Path, w.Branch)
		return
	}
	purgeTempBranch(root, w, res)
	if worktreeCleanupFailed(w.Path, res) {
		ui.WarnProgress(p, "kept local branch %s: branch deletion failed; retry `wtm gc --path %q` or `wtm delete %q --yes`", w.Branch, w.Path, w.Branch)
	}
}

// pruneWorktrees runs prune unconditionally: the old `len(res.Removed) > 0`
// gate skipped it exactly when every removal failed, leaving stale admin
// entries that wedge later runs.
func pruneWorktrees(res *gcResult, root string, p *ui.Progress) {
	p.Phase("pruning", 0)
	if err := git.WorktreePrune(root); err != nil {
		res.Failed = append(res.Failed, gcFailure{Path: root, Error: "prune: " + err.Error()})
		return
	}
	res.Pruned = true
}

func printGCOutcomes(res gcResult) {
	for _, p := range res.Removed {
		fmt.Printf("removed %s\n", p)
	}
	for _, b := range res.DeletedBranches {
		fmt.Printf("deleted branch %s\n", b)
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
	// A skip is a real outcome; each reason is disclosed on stderr as it
	// happens, so the summary stays reason-neutral.
	if len(res.Skipped) > 0 {
		fmt.Printf("skipped %d\n", len(res.Skipped))
	}
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
	if g.JSON {
		return ui.PrintJSON(res)
	}
	if len(res.Removed) == 0 && len(res.Failed) == 0 && len(res.Skipped) == 0 && len(res.RemoteDeleted) == 0 && len(res.DeletedBranches) == 0 && res.DeletedBranch == "" {
		fmt.Println("nothing to do")
		return nil
	}
	printGCHuman(res)
	return nil
}
