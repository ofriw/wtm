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

// status.go — the worktree domain: the shared model, discovery, and the
// status command. Discovery relies on injected hooks (session indexing and MCP
// inventory) so this package never imports the harness.

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

// unused reports whether gc may reclaim a worktree: a temp one past its own
// idle window, a permanent one past the global unusedTTL. It is the single
// candidate predicate for status and gc.
func (w Worktree) Unused(now time.Time, ttl time.Duration) bool {
	if w.Temp {
		return temp.TempExpired(w.TempCreated, w.LastUsed, w.TempTTL, now)
	}
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
}

// tempExpiresAt is the deadline a temp worktree is measured against.
// temp.TempDeadline owns the formula. Zero for a permanent worktree.
func (w Worktree) TempExpiresAt() time.Time {
	if !w.Temp {
		return time.Time{}
	}
	return temp.TempDeadline(w.TempCreated, w.LastUsed, w.TempTTL)
}

func (w Worktree) Status(now time.Time, ttl time.Duration) string {
	if w.Unused(now, ttl) {
		return "UNUSED"
	}
	return "ACTIVE"
}

func (w Worktree) BranchName() string {
	if w.Branch == "" {
		return "(detached)"
	}
	return w.Branch
}

// ResolveRoot returns the repo root: --root if given, else the main worktree
// of the repo containing the current directory.
func ResolveRoot(g *Globals) (string, error) {
	if g.Root != "" {
		return fs.CanonicalAbs(g.Root)
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
type SessionDiscovery func(*ui.Progress) (SessionIndex, []UnverifiedSession, error)

// Hooks carries the harness-provided discovery functions core cannot call
// directly (it must not import the harness).
type Hooks struct {
	Sessions SessionDiscovery
	MCP      func(root string, p *ui.Progress) map[string]bool
}

// LoadWorkspace resolves the repo, TTL and inventory. discovery — the slow
// path — uses the renderer when showProgress or --progress always requests it.
// Discovery reads but never reconciles the temp registry. Explicit removal
// passes failClosed-style hooks to reject unreadable sessions before any
// destructive action.
func LoadWorkspace(g *Globals, showProgress bool, hooks Hooks) (Workspace, error) {
	return loadWorkspaceWithSessions(g, showProgress, hooks)
}

func loadWorkspaceWithSessions(g *Globals, showProgress bool, hooks Hooks) (Workspace, error) {
	var ws Workspace
	root, err := ResolveRoot(g)
	if err != nil {
		return ws, err
	}
	ttl, err := UnusedDuration()
	if err != nil {
		return ws, err
	}
	err = ui.RunMaybeProgress(os.Stderr, g.Progress, g.JSON, showProgress, func(p *ui.Progress) error {
		var derr error
		ws.Wts, ws.Idx, derr = discoverWithSessions(root, p, hooks)
		return derr
	})
	ws.Root, ws.TTL = root, ttl
	return ws, err
}

// BuildWorktree composes a single inventory entry from its raw inputs; exported
// for tests that pin inventory composition independent of a discovery run.
func BuildWorktree(g git.GWorktree, idx SessionIndex, ups map[string]git.Upstream, temps temp.Store, p *ui.Progress, mcp func(string, *ui.Progress) map[string]bool) Worktree {
	return buildWorktree(g, idx, ups, temps, p, mcp)
}

func buildWorktree(g git.GWorktree, idx SessionIndex, ups map[string]git.Upstream, temps temp.Store, p *ui.Progress, mcp func(string, *ui.Progress) map[string]bool) Worktree {
	w := worktreeInventory(g, idx, ups, p, mcp)
	applyTempRecord(&w, temps, p)
	return w
}

func worktreeInventory(g git.GWorktree, idx SessionIndex, ups map[string]git.Upstream, p *ui.Progress, mcp func(string, *ui.Progress) map[string]bool) Worktree {
	pth := fs.Canonical(g.Path)
	configs := mcp(pth, p)
	return Worktree{
		Path: pth, Branch: g.Branch, Upstream: ups[g.Branch], Main: g.Main,
		// Dirty is set lazily by the gc picker; status does not use it.
		LastUsed: worktreeLastUsed(pth, idx),
		Caps:     CapabilityStates(CapInput{Root: pth, MCP: configs}),
	}
}

// applyTempRecord marks w temporary when its record verifies. Every failure
// falls toward permanent, never toward reclaimable: a zero TTL would reclaim
// instantly, so an unparsable window is refused rather than defaulted.
// ApplyTempRecord marks w temporary when its record verifies; exported for
// tests pinning the temp-record application contract.
func ApplyTempRecord(w *Worktree, temps temp.Store, p *ui.Progress) {
	applyTempRecord(w, temps, p)
}

func applyTempRecord(w *Worktree, temps temp.Store, p *ui.Progress) {
	rec, ok := temps[w.Path]
	if !ok {
		return
	}
	if err := temp.VerifyTempIdentity(w.Path, rec); err != nil {
		ui.WarnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	ttl, err := temp.ParseTTL(rec.TTL)
	if err != nil {
		ui.WarnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	w.Temp, w.TempTTL, w.TempCreated, w.TempIdentity = true, ttl, rec.CreatedAt, rec.Identity
}

// DiscoverWithSessions exposes discoverWithSessions: run full repo discovery
// with an explicit progress handle and harness-provided hooks. cli tests use
// it to observe the notice stream directly.
func DiscoverWithSessions(root string, p *ui.Progress, hooks Hooks) ([]Worktree, SessionIndex, error) {
	return discoverWithSessions(root, p, hooks)
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
func discoverInputs(root string, p *ui.Progress, hooks Hooks) ([]git.GWorktree, SessionIndex, []UnverifiedSession, map[string]git.Upstream, temp.Store, error) {
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
func guardPermanentActivity(wts []Worktree, unknown []UnverifiedSession, now time.Time, p *ui.Progress) {
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
			ui.WarnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

// keepPermanentsOlderThan applies the same age gate as
// UnverifiedSession.BlockRemoval: only a session newer than known activity can
// hide recent use. The equality edge differs by intent — this guard protects on
// == (fail closed for permanents), the temp path removes on strict >.
func keepPermanentsOlderThan(wts []Worktree, newest, now time.Time, p *ui.Progress) {
	for i := range wts {
		if !wts[i].Temp && !wts[i].LastUsed.After(newest) {
			wts[i].LastUsed = now
			ui.WarnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

func hasUnknownAgeSession(unknown []UnverifiedSession) bool {
	for _, u := range unknown {
		if u.ModTime.IsZero() {
			return true
		}
	}
	return false
}

func newestKnownSession(unknown []UnverifiedSession) time.Time {
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
func scanWorktrees(raw []git.GWorktree, idx SessionIndex, ups map[string]git.Upstream, temps temp.Store, p *ui.Progress, mcp func(string, *ui.Progress) map[string]bool) []Worktree {
	p.Phase("scanning worktrees", len(raw))
	wts := make([]Worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx, ups, temps, p, mcp))
		p.Advance(1)
		p.Detail(displayPath(g.Path))
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

// WorktreeLastUsed is the non-strict activity read exposed for tests that pin
// LastUsed composition (sessions + git activity) independent of discovery.
func WorktreeLastUsed(path string, idx SessionIndex) time.Time {
	return worktreeLastUsed(path, idx)
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
// SortWorktrees is the total order used by discovery; exported for tests that
// pin ordering independently of a full discovery run.
func SortWorktrees(wts []Worktree) {
	sortWorktrees(wts)
}

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

// CmdStatus is the status command handler. cli dispatches to it; hooks carry
// the harness-provided session/MCP discovery.
func CmdStatus(g *Globals, args []string, hooks Hooks) error {
	fs := SubFlags("status", g)
	wide := fs.Bool("wide", false, "show one column per integration")
	pos, err := ParseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return Usagef("status: unexpected argument %q", pos[0])
	}
	// --wide shapes only the table; the JSON contract always reports every
	// integration, so the combination is a usage error, not a silent no-op.
	if *wide && g.JSON {
		return Usagef("status: --wide has no effect with --json (JSON already reports all integrations)")
	}
	ws, err := LoadWorkspace(g, false, hooks)
	if err != nil {
		return err
	}
	now := time.Now()
	if g.JSON {
		return PrintStatusJSON(ws.Wts, now, ws.TTL)
	}
	printStatusTable(StatusColumnsFor(*wide), ws.Wts, now, ws.TTL, g.Color)
	return nil
}
