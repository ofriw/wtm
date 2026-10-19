package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// workspace.go — the worktree/workspace model, the discovery pipeline that
// fills it, and the shared path display: root resolution, inventory, session
// indexing, temp records and the activity scan that seeds LAST USED.

// worktree is the shared model: one git worktree plus its harness inventory.
type worktree struct {
	Path     string
	Branch   string
	Upstream upstream // configured tracking branch; zero value = none
	Main     bool
	Dirty    bool // uncommitted changes; surfaced for gc (picker label, forced-removal warning), never a filter
	LastUsed time.Time

	// Temp metadata is applied only after registry identity and branch checks.
	Temp         bool
	TempTTL      time.Duration
	TempCreated  time.Time
	TempIdentity string
	// Caps uses the capability registry's ids and states.
	Caps map[string]capState
	// Created is the worktree creation mark: the .git entry mtime. Zero when the
	// mark is absent, which renders as an unknown age.
	Created time.Time
	// Usage is the cost/active-time summary; nil when the worktree has no agent
	// events. Only status fills it; gc and add leave it nil.
	Usage *worktreeUsage
}

func (w worktree) branchName() string {
	if w.Branch == "" {
		return "(detached)"
	}
	return w.Branch
}

// resolveRoot returns the repo root: --root if given, else the main worktree
// of the repo containing the current directory.
func resolveRoot(g *globals) (string, error) {
	if g.root != "" {
		return canonicalAbs(g.root)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := repoRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("no repository here; use --root: %w", err)
	}
	return canonical(root), nil
}

// workspace is one discovery pass: the repo root, the TTL, the inventory and
// the session index the inventory was built from. Reclamation reuses that index
// instead of walking every harness session root a second time.
type workspace struct {
	root string
	ttl  time.Duration
	wts  []worktree
	idx  sessionIndex
}

// loadWorkspace resolves the repo, TTL and inventory. discovery — the slow
// path — uses the renderer when showProgress or --progress always requests it.
// Discovery reads but never reconciles the temp registry. Explicit removal
// passes failClosed to reject unreadable sessions before any destructive action.
func loadWorkspace(g *globals, showProgress, failClosed bool) (workspace, error) {
	return loadWorkspaceWithSessions(g, showProgress, discoverySessions(failClosed))
}

// Idle GC collects file evidence for fresh candidate checks; explicit --path
// removal bypasses idleness, not strict session-read safety.
func loadGCWorkspace(g *globals, paths []string) (workspace, error) {
	if len(paths) > 0 {
		return loadWorkspace(g, true, true)
	}
	return loadWorkspaceWithSessions(g, true, indexSessionsStrict)
}

type sessionDiscovery func(*progress) (sessionIndex, []unverifiedSession, error)

func loadWorkspaceWithSessions(g *globals, showProgress bool, sessions sessionDiscovery) (workspace, error) {
	var ws workspace
	root, err := resolveRoot(g)
	if err != nil {
		return ws, err
	}
	ttl, err := unusedDuration()
	if err != nil {
		return ws, err
	}
	err = withProgress(g, showProgress, func(p *progress) error {
		var derr error
		ws.wts, ws.idx, derr = discoverWithSessions(root, p, sessions)
		return derr
	})
	ws.root, ws.ttl = root, ttl
	return ws, err
}

func buildWorktree(g gworktree, idx sessionIndex, ups map[string]upstream, temps tempStore, p *progress) worktree {
	w := worktreeInventory(g, idx, ups, p)
	applyTempRecord(&w, temps, p)
	return w
}

func worktreeInventory(g gworktree, idx sessionIndex, ups map[string]upstream, p *progress) worktree {
	pth := canonical(g.Path)
	configs := mcpConfigs(pth, p)
	return worktree{
		Path: pth, Branch: g.Branch, Upstream: ups[g.Branch], Main: g.Main,
		// Dirty is set lazily by the gc picker; status does not use it.
		LastUsed: worktreeLastUsed(pth, idx),
		Caps:     capabilityStates(capInput{root: pth, mcp: configs}),
		Created:  worktreeCreated(pth),
	}
}

// worktreeCreated reads the .git entry mtime as the creation mark. A linked
// worktree's .git is a file, so its mtime is a stable creation proxy. The main
// worktree's .git is a directory, so its AGE resets on index and ref writes;
// the design accepts this because birth time is unavailable on every filesystem.
func worktreeCreated(path string) time.Time {
	info, err := os.Stat(filepath.Join(path, ".git"))
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

// applyTempRecord marks w temporary when its record verifies. Every failure
// falls toward permanent, never toward reclaimable: a zero TTL would reclaim
// instantly, so an unparsable window is refused rather than defaulted.
func applyTempRecord(w *worktree, temps tempStore, p *progress) {
	rec, ok := temps[w.Path]
	if !ok {
		return
	}
	if err := verifyTempIdentity(w.Path, rec); err != nil {
		warnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	ttl, err := parseTTL(rec.TTL)
	if err != nil {
		warnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	w.Temp, w.TempTTL, w.TempCreated, w.TempIdentity = true, ttl, rec.CreatedAt, rec.Identity
}

// discover inventories every worktree of the repo rooted at root, reporting
// each slow step to p. The returned session index is reused by reclamation.
func discover(root string, p *progress, failClosed bool) ([]worktree, sessionIndex, error) {
	return discoverWithSessions(root, p, discoverySessions(failClosed))
}

func discoverWithSessions(root string, p *progress, sessions sessionDiscovery) ([]worktree, sessionIndex, error) {
	raw, idx, unknown, ups, temps, err := discoverInputs(root, p, sessions)
	if err != nil {
		return nil, nil, err
	}
	wts := scanWorktrees(raw, idx, ups, temps, p)
	guardPermanentActivity(wts, unknown, nowFunc(), p)
	sortWorktrees(wts)
	return wts, idx, nil
}

func discoverUpstreams(root string, p *progress) map[string]upstream {
	p.phase("reading upstreams", 0)
	ups, err := branchUpstreams(root)
	if err != nil {
		warnProgress(p, "upstreams unavailable: %s", err)
		return map[string]upstream{}
	}
	return ups
}

// discoverInputs reads each shared source once: worktrees, sessions, upstreams
// and temp records. Reclamation retains unknown files for candidate-level checks.
func discoverInputs(root string, p *progress, sessions sessionDiscovery) ([]gworktree, sessionIndex, []unverifiedSession, map[string]upstream, tempStore, error) {
	p.phase("reading worktrees", 0)
	raw, err := worktrees(root)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	p.phase("indexing sessions", 0)
	idx, unknown, err := sessions(p)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	return raw, idx, unknown, discoverUpstreams(root, p), readTempStoreLenient(p), nil
}

func discoverySessions(failClosed bool) sessionDiscovery {
	return func(p *progress) (sessionIndex, []unverifiedSession, error) {
		idx, err := indexSessions(agents(), sessionErrPolicy{failClosed: failClosed, p: p})
		return idx, nil, err
	}
}

// readTempStoreLenient reads the temp store for read-only commands. A corrupt
// store only loses temp metadata (the checkouts stay durable), so it must never
// break status: warn and proceed with no temp records.
func readTempStoreLenient(p *progress) tempStore {
	temps, err := readTempStoreUnlocked()
	if err != nil {
		warnProgress(p, "temp store: %s; no temp worktrees tracked", err)
		return tempStore{}
	}
	return temps
}

// scanWorktrees builds the inventory and reports per-worktree progress; the
// loop is split out so discover stays a readable sequence of phases.
func scanWorktrees(raw []gworktree, idx sessionIndex, ups map[string]upstream, temps tempStore, p *progress) []worktree {
	p.phase("scanning worktrees", len(raw))
	wts := make([]worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx, ups, temps, p))
		p.advance(1)
		p.detail(displayPath(g.Path))
	}
	return wts
}

// sortWorktrees orders main first, then most recently used, then path — a
// total order, so machine output never depends on git's listing order.
func sortWorktrees(wts []worktree) {
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

// currentWorktreePath resolves the worktree that contains the current
// directory; it falls back to the main worktree when the directory is outside
// the repo (for example with an explicit --root).
func currentWorktreePath(ws workspace) string {
	if cwd, err := os.Getwd(); err == nil {
		if top, err := gitTopLevel(cwd); err == nil {
			c := canonical(top)
			for _, w := range ws.wts {
				if w.Path == c {
					return c
				}
			}
		}
	}
	for _, w := range ws.wts {
		if w.Main {
			return w.Path
		}
	}
	return ws.root
}

// displayPath shortens the home directory to ~ so the PATH column budgets for
// the path a human recognizes, not for an invariant absolute prefix.
func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}
