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

// loadWorkspace resolves the repo, TTL and inventory against the caller's
// progress handle: the command entry owns the renderer, so a single session can
// span discovery, pricing and parsing. A nil p is a no-op handle. Discovery
// reads but never reconciles the temp registry. Explicit removal passes
// failClosed to reject unreadable sessions before any destructive action.
func loadWorkspace(g *globals, p *progress, failClosed bool) (workspace, error) {
	return loadWorkspaceWithSessions(g, p, discoverySessions(failClosed))
}

// loadWorkspaceProgress opens a discovery-only progress session. Commands whose
// slow work is discovery alone (delete, promote) use it; status opens its own
// session so pricing and parsing share the same line.
func loadWorkspaceProgress(g *globals, failClosed bool) (workspace, error) {
	var ws workspace
	err := withProgress(g, progressOptions{}, func(p *progress) error {
		var derr error
		ws, derr = loadWorkspace(g, p, failClosed)
		return derr
	})
	return ws, err
}

// Idle GC collects file evidence for fresh candidate checks; explicit --path
// removal bypasses idleness, not strict session-read safety. gc owns its
// renderer because the interactive picker must not run underneath one.
func loadGCWorkspace(g *globals, paths []string) (workspace, error) {
	sessions := sessionDiscovery(indexSessionsStrict)
	if len(paths) > 0 {
		sessions = discoverySessions(true)
	}
	var ws workspace
	err := withProgress(g, progressOptions{}, func(p *progress) error {
		var derr error
		ws, derr = loadWorkspaceWithSessions(g, p, sessions)
		return derr
	})
	return ws, err
}

type sessionDiscovery func(*progress) (sessionIndex, []unverifiedSession, error)

func loadWorkspaceWithSessions(g *globals, p *progress, sessions sessionDiscovery) (workspace, error) {
	var ws workspace
	root, err := resolveRoot(g)
	if err != nil {
		return ws, err
	}
	ttl, err := unusedDuration()
	if err != nil {
		return ws, err
	}
	ws.wts, ws.idx, err = discoverWithSessions(root, p, sessions)
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
		LastUsed: worktreeLastUsed(pth, idx, p),
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
	in, err := discoverInputs(root, p, sessions)
	if err != nil {
		return nil, nil, err
	}
	wts := scanWorktrees(in, p)
	guardPermanentActivity(wts, in.unknown, nowFunc(), p)
	sortWorktrees(wts)
	return wts, in.idx, nil
}

// discoverUpstreams returns the configured tracking branches, or the warning
// that explains their absence. It reports upward because discovery emits
// warnings through one shared path, which keeps their order stable.
func discoverUpstreams(root string) (map[string]upstream, string) {
	ups, err := branchUpstreams(root)
	if err != nil {
		return map[string]upstream{}, fmt.Sprintf("upstreams unavailable: %s", err)
	}
	return ups, ""
}

// discoveryInputs is one batch of shared reads. Every source is read exactly
// once and reused by inventory, activity and reclamation.
type discoveryInputs struct {
	raw     []gworktree
	idx     sessionIndex
	unknown []unverifiedSession
	ups     map[string]upstream
	temps   tempStore
}

// discoverInputs reads each shared source once. The label names the session
// walk because it dominates the batch; the other three finish in tens of
// milliseconds. Warnings are emitted in a fixed order.
func discoverInputs(root string, p *progress, sessions sessionDiscovery) (discoveryInputs, error) {
	var in discoveryInputs
	p.phase("indexing sessions", 0)
	raw, err := worktrees(root)
	if err != nil {
		return discoveryInputs{}, err
	}
	idx, unknown, err := sessions(p)
	if err != nil {
		return discoveryInputs{}, err
	}
	ups, upsWarn := discoverUpstreams(root)
	temps, tempWarn := readTempStoreLenient()
	in.raw, in.idx, in.unknown, in.ups, in.temps = raw, idx, unknown, ups, temps
	emitWarning(p, upsWarn)
	emitWarning(p, tempWarn)
	return in, nil
}

// emitWarning writes a deferred warning through the one disclosure path.
func emitWarning(p *progress, text string) {
	if text != "" {
		emitProgress(p, warnPrefix+text)
	}
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
func readTempStoreLenient() (tempStore, string) {
	temps, err := readTempStoreUnlocked()
	if err != nil {
		return tempStore{}, fmt.Sprintf("temp store: %s; no temp worktrees tracked", err)
	}
	return temps, ""
}

// scanWorktrees builds the inventory and reports per-worktree progress; the
// loop is split out so discover stays a readable sequence of phases.
func scanWorktrees(in discoveryInputs, p *progress) []worktree {
	p.phase("scanning worktrees", len(in.raw))
	wts := make([]worktree, 0, len(in.raw))
	for _, g := range in.raw {
		p.detail(displayPath(g.Path))
		wts = append(wts, buildWorktree(g, in.idx, in.ups, in.temps, p))
		p.advance(1)
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
