package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/temp"
	"wtm/internal/ui"
)

// status.go — the worktree domain: the shared model and discovery. Discovery
// relies on injected hooks (session indexing and MCP inventory) so this
// package never imports the harness. The status command handler lives in cli.

// Worktree is the shared model: one git worktree plus its harness inventory.
type Worktree struct {
	Path     string
	Branch   string
	Upstream git.Upstream // configured tracking branch; zero value = none
	Main     bool
	Dirty    bool // uncommitted changes; surfaced for gc (picker label, forced-removal warning), never a filter
	LastUsed time.Time

	// Temp metadata is applied only after registry identity and branch checks.
	Temp         bool
	TempTTL      time.Duration
	TempCreated  time.Time
	TempIdentity string
	// Caps uses the capability registry's ids and states.
	Caps map[string]CapState
}

// Unused reports whether gc may reclaim a worktree: a temp one past its own
// idle window, a permanent one past the global unusedTTL. It is the single
// candidate predicate for status and gc.
func (w Worktree) Unused(now time.Time, ttl time.Duration) bool {
	if w.Temp {
		return temp.TempExpired(w.TempCreated, w.LastUsed, w.TempTTL, now)
	}
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
}

// TempExpiresAt is the deadline a temp worktree is measured against; temp.Deadline
// owns the formula. Zero for a permanent worktree.
func (w Worktree) TempExpiresAt() time.Time {
	if !w.Temp {
		return time.Time{}
	}
	return temp.Deadline(w.TempCreated, w.LastUsed, w.TempTTL)
}

// Status classifies the worktree as ACTIVE or UNUSED using the same cutoff as
// Unused, so the table and the gc selection never disagree.
func (w Worktree) Status(now time.Time, ttl time.Duration) string {
	if w.Unused(now, ttl) {
		return "UNUSED"
	}
	return "ACTIVE"
}

// BranchName returns the branch name, or "(detached)" when none is checked out.
func (w Worktree) BranchName() string {
	if w.Branch == "" {
		return "(detached)"
	}
	return w.Branch
}

// Options carries the neutral discovery inputs the CLI passes into core; the
// domain layer owns no flag machinery.
type Options struct {
	Root     string
	JSON     bool
	Progress string
}

// ResolveRoot returns the repo root: --root if given, else the main worktree
// of the repo containing the current directory.
func ResolveRoot(o Options) (string, error) {
	if o.Root != "" {
		return fs.CanonicalAbs(o.Root)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := git.RepoRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("no repository here; use --root: %w", err)
	}
	return fs.Canonical(root), nil
}

// Workspace is one discovery pass: the repo root, the TTL, the inventory and
// the session index the inventory was built from. Reclamation reuses that index
// instead of walking every harness session root a second time.
type Workspace struct {
	Root string
	TTL  time.Duration
	Wts  []Worktree
	Idx  SessionIndex
}

// SessionDiscovery is an injected session-indexing function; cli wires it to
// the harness.
type SessionDiscovery func(*ui.Progress) (SessionIndex, []temp.UnverifiedSession, error)

// MCPDiscovery inventories a worktree root's MCP configs; the harness provides
// the real implementation, core only defines the seam (it must not import the
// harness).
type MCPDiscovery func(root string, p *ui.Progress) map[string]bool

// Hooks carries the harness-provided discovery functions core cannot call
// directly (it must not import the harness).
type Hooks struct {
	Sessions SessionDiscovery
	MCP      MCPDiscovery
}

// LoadWorkspace resolves the repo, TTL and inventory. discovery — the slow
// path — uses the renderer when showProgress or --progress always requests it.
// Discovery reads but never reconciles the temp registry. Explicit removal
// passes failClosed-style hooks to reject unreadable sessions before any
// destructive action.
func LoadWorkspace(o Options, showProgress bool, hooks Hooks) (Workspace, error) {
	var ws Workspace
	if err := validateDiscoveryHooks(hooks); err != nil {
		return ws, err
	}
	root, err := ResolveRoot(o)
	if err != nil {
		return ws, err
	}
	ttl, err := UnusedDuration()
	if err != nil {
		return ws, err
	}
	err = ui.RunMaybeProgress(os.Stderr, o.Progress, o.JSON, showProgress, func(p *ui.Progress) error {
		var derr error
		ws.Wts, ws.Idx, derr = discoverWithSessions(root, p, hooks)
		return derr
	})
	ws.Root, ws.TTL = root, ttl
	return ws, err
}

func buildWorktree(g git.Worktree, idx SessionIndex, ups map[string]git.Upstream, temps temp.Store, p *ui.Progress, mcp MCPDiscovery) Worktree {
	w := worktreeInventory(g, idx, ups, p, mcp)
	applyTempRecord(&w, temps, p)
	return w
}

func worktreeInventory(g git.Worktree, idx SessionIndex, ups map[string]git.Upstream, p *ui.Progress, mcp MCPDiscovery) Worktree {
	pth := fs.Canonical(g.Path)
	configs := mcp(pth, p)
	return Worktree{
		Path: pth, Branch: g.Branch, Upstream: ups[g.Branch], Main: g.Main,
		// Dirty is set lazily by the gc picker; status does not use it.
		LastUsed: worktreeLastUsed(pth, idx),
		Caps:     capabilityStates(CapInput{Root: pth, MCP: configs}),
	}
}

// applyTempRecord marks w temporary when its record verifies. Every failure
// falls toward permanent, never toward reclaimable: a zero TTL would reclaim
// instantly, so an unparsable window is refused rather than defaulted.
func applyTempRecord(w *Worktree, temps temp.Store, p *ui.Progress) {
	rec, ok := temps[w.Path]
	if !ok {
		return
	}
	if err := temp.VerifyTempIdentity(w.Path, rec); err != nil {
		ui.WarnProgress(p, "temp record for %s: %s; treating as permanent", DisplayPath(w.Path), err)
		return
	}
	ttl, err := temp.ParseTTL(rec.TTL)
	if err != nil {
		ui.WarnProgress(p, "temp record for %s: %s; treating as permanent", DisplayPath(w.Path), err)
		return
	}
	w.Temp, w.TempTTL, w.TempCreated, w.TempIdentity = true, ttl, rec.CreatedAt, rec.Identity
}

// validateDiscoveryHooks rejects missing hook wiring at every discovery entry
// path, so a caller that omits a hook fails with a diagnostic instead of a nil
// dereference deeper in the flow.
func validateDiscoveryHooks(hooks Hooks) error {
	if hooks.Sessions == nil || hooks.MCP == nil {
		return fmt.Errorf("discovery hooks are required: sessions=%v mcp=%v", hooks.Sessions != nil, hooks.MCP != nil)
	}
	return nil
}

func discoverWithSessions(root string, p *ui.Progress, hooks Hooks) ([]Worktree, SessionIndex, error) {
	raw, idx, unknown, ups, temps, err := discoverInputs(root, p, hooks)
	if err != nil {
		return nil, nil, err
	}
	wts := scanWorktrees(raw, idx, ups, temps, p, hooks.MCP)
	guardPermanentActivity(wts, unknown, time.Now(), p)
	sortWorktrees(wts)
	return wts, idx, nil
}

func discoverUpstreams(root string, p *ui.Progress) map[string]git.Upstream {
	p.Phase("reading upstreams", 0)
	ups, err := git.BranchUpstreams(root)
	if err != nil {
		ui.WarnProgress(p, "upstreams unavailable: %s", err)
		return map[string]git.Upstream{}
	}
	return ups
}

// discoverInputs reads each shared source once: worktrees, sessions, upstreams
// and temp records. Reclamation retains unknown files for candidate-level checks.
func discoverInputs(root string, p *ui.Progress, hooks Hooks) ([]git.Worktree, SessionIndex, []temp.UnverifiedSession, map[string]git.Upstream, temp.Store, error) {
	p.Phase("reading worktrees", 0)
	raw, err := git.Worktrees(root)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	p.Phase("indexing sessions", 0)
	idx, unknown, err := hooks.Sessions(p)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return raw, idx, unknown, discoverUpstreams(root, p), readTempStoreLenient(p), nil
}

// Permanent cleanup has no fresh idle probe. Missing ownership therefore keeps
// every permanent candidate active; temp candidates use the locked fresh probe.
// When all unverified sessions have known ages, protection scopes to permanent
// worktrees older than the newest unverified session. A single unknown-age session
// preserves the original fail-closed contract.
func guardPermanentActivity(wts []Worktree, unknown []temp.UnverifiedSession, now time.Time, p *ui.Progress) {
	if len(unknown) == 0 {
		return
	}
	for _, u := range unknown {
		ui.WarnProgress(p, "unverified session ownership: %s", u.Path)
	}
	if hasUnknownAgeSession(unknown) {
		keepPermanentsActive(wts, now, p)
		return
	}
	keepPermanentsOlderThan(wts, newestKnownSession(unknown), now, p)
}

// keepPermanentsActive is the blanket fail-closed guard: one unknown-age
// session grants every permanent worktree fresh activity, so gc cannot prove
// any of them abandoned.
func keepPermanentsActive(wts []Worktree, now time.Time, p *ui.Progress) {
	for i := range wts {
		if !wts[i].Temp {
			wts[i].LastUsed = now
			ui.WarnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", DisplayPath(wts[i].Path))
		}
	}
}

// keepPermanentsOlderThan applies the same age gate as
// temp.UnverifiedSession.BlockRemoval: only a session newer than known activity
// can hide recent use. The equality edge differs by intent — this guard
// protects on == (fail closed for permanents), the temp path removes on strict >.
func keepPermanentsOlderThan(wts []Worktree, newest, now time.Time, p *ui.Progress) {
	for i := range wts {
		if !wts[i].Temp && !wts[i].LastUsed.After(newest) {
			wts[i].LastUsed = now
			ui.WarnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", DisplayPath(wts[i].Path))
		}
	}
}

func hasUnknownAgeSession(unknown []temp.UnverifiedSession) bool {
	for _, u := range unknown {
		if u.ModTime.IsZero() {
			return true
		}
	}
	return false
}

func newestKnownSession(unknown []temp.UnverifiedSession) time.Time {
	var newest time.Time
	for _, u := range unknown {
		if u.ModTime.After(newest) {
			newest = u.ModTime
		}
	}
	return newest
}

// readTempStoreLenient reads the temp store for read-only commands. A corrupt
// store only loses temp metadata (the checkouts stay durable), so it must never
// break status: warn and proceed with no temp records.
func readTempStoreLenient(p *ui.Progress) temp.Store {
	temps, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		ui.WarnProgress(p, "temp store: %s; no temp worktrees tracked", err)
		return temp.Store{}
	}
	return temps
}

// scanWorktrees builds the inventory and reports per-worktree progress; the
// loop is split out so discover stays a readable sequence of phases.
func scanWorktrees(raw []git.Worktree, idx SessionIndex, ups map[string]git.Upstream, temps temp.Store, p *ui.Progress, mcp MCPDiscovery) []Worktree {
	p.Phase("scanning worktrees", len(raw))
	wts := make([]Worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx, ups, temps, p, mcp))
		p.Advance(1)
		p.Detail(DisplayPath(g.Path))
	}
	return wts
}

func updateNewest(target *time.Time, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("activity stat %s: %w", path, err)
	}
	if info.ModTime().After(*target) {
		*target = info.ModTime()
	}
	return nil
}

func scanGitModTimes(path string, newest *time.Time, strict bool) error {
	if err := scanIndexTime(path, newest); err != nil && strict {
		return err
	}
	if err := scanCommitTime(path, newest); err != nil && strict {
		return err
	}
	return scanChangedFileTimes(path, newest, strict)
}

func scanCommitTime(path string, newest *time.Time) error {
	out, err := git.Run(path, "log", "-1", "--format=%ct")
	if err != nil {
		return err
	}
	s, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return fmt.Errorf("parse git commit time: %w", err)
	}
	if commit := time.Unix(s, 0); commit.After(*newest) {
		*newest = commit
	}
	return nil
}

// changedActivityFiles includes staged files but not staged deletions: their
// activity survives in the private index, while missing unstaged files fail closed.
func changedActivityFiles(path string) (string, error) {
	out, err := git.Run(path, "--no-optional-locks", "ls-files", "-m", "-o", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	staged, err := git.Run(path, "--no-optional-locks", "diff", "--cached", "--name-only", "--diff-filter=d", "-z")
	return out + "\x00" + staged, err
}

func scanChangedFileTimes(path string, newest *time.Time, strict bool) error {
	out, err := changedActivityFiles(path)
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(out, "\x00") {
		if rel != "" && !strings.HasPrefix(rel, ".chunkhound") {
			if err := updateNewest(newest, filepath.Join(path, rel)); err != nil && strict {
				// A vanished changed file cannot be checked for activity: fail
				// closed and say how the user resolves the resulting deadlock.
				// errors.Is, not os.IsNotExist: updateNewest wraps with %w.
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w; commit or restore the deleted file, or remove the checkout explicitly", err)
				}
				return err
			}
		}
	}
	return nil
}

// ReadWorktreeActivityStrict collects activity timestamps relative to now,
// failing on any error. Used immediately before idle-based removal where
// unreadable activity must prevent reclamation.
func ReadWorktreeActivityStrict(path string, idx SessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, true)
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first agent session.
func worktreeLastUsed(path string, idx SessionIndex) time.Time {
	newest, _ := collectActivity(path, idx, time.Now(), false)
	return newest
}

// collectActivity merges agent sessions, the checkout backlink and Git
// modification times, clamping any future timestamp to now.
func collectActivity(path string, idx SessionIndex, now time.Time, strict bool) (time.Time, error) {
	newest, err := SessionLastUsed(path, idx, strict)
	if err != nil {
		return newest, err
	}
	if err := updateNewest(&newest, filepath.Join(path, ".git")); err != nil && strict {
		return newest, err
	}
	if err := scanGitModTimes(path, &newest, strict); err != nil && strict {
		return newest, err
	}
	if newest.After(now) {
		return now, nil
	}
	return newest, nil
}

// The private index records staging, including deletion with no remaining
// file. Read-only Git queries must not refresh it and restart the idle window.
func scanIndexTime(path string, newest *time.Time) error {
	dir, err := git.PrivateDir(path)
	if err != nil {
		return err
	}
	return updateNewest(newest, filepath.Join(dir, "index"))
}

// sortWorktrees orders main first, then most recently used, then path — a
// total order, so machine output never depends on git's listing order.
func sortWorktrees(wts []Worktree) {
	sort.Slice(wts, func(i, j int) bool {
		a, b := wts[i], wts[j]
		if a.Main != b.Main {
			return a.Main
		}
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		return a.Path < b.Path
	})
}
