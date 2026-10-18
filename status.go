package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

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

// unused reports whether gc may reclaim a worktree: a temp one past its own
// idle window, a permanent one past the global unusedTTL. It is the single
// candidate predicate for status and gc.
func (w worktree) unused(now time.Time, ttl time.Duration) bool {
	if w.Temp {
		return tempExpired(w.TempCreated, w.LastUsed, w.TempTTL, now)
	}
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
}

// tempExpiresAt is the deadline a temp worktree is measured against.
// tempDeadline owns the formula. Zero for a permanent worktree.
func (w worktree) tempExpiresAt() time.Time {
	if !w.Temp {
		return time.Time{}
	}
	return tempDeadline(w.TempCreated, w.LastUsed, w.TempTTL)
}

func (w worktree) status(now time.Time, ttl time.Duration) string {
	if w.unused(now, ttl) {
		return "UNUSED"
	}
	return "ACTIVE"
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

// Permanent cleanup has no fresh idle probe. Missing ownership therefore keeps
// every permanent candidate active; temp candidates use the locked fresh probe.
// When all unverified sessions have known ages, protection scopes to permanent
// worktrees older than the newest unverified session. A single unknown-age session
// preserves the original fail-closed contract.
func guardPermanentActivity(wts []worktree, unknown []unverifiedSession, now time.Time, p *progress) {
	if len(unknown) == 0 {
		return
	}
	for _, u := range unknown {
		warnProgress(p, "unverified session ownership: %s", u.Path)
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
func keepPermanentsActive(wts []worktree, now time.Time, p *progress) {
	for i := range wts {
		if !wts[i].Temp {
			wts[i].LastUsed = now
			warnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

// keepPermanentsOlderThan applies the same age gate as
// unverifiedSession.blocksRemoval: only a session newer than known activity can
// hide recent use. The equality edge differs by intent — this guard protects on
// == (fail closed for permanents), the temp path removes on strict >.
func keepPermanentsOlderThan(wts []worktree, newest, now time.Time, p *progress) {
	for i := range wts {
		if !wts[i].Temp && !wts[i].LastUsed.After(newest) {
			wts[i].LastUsed = now
			warnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

func hasUnknownAgeSession(unknown []unverifiedSession) bool {
	for _, u := range unknown {
		if u.ModTime.IsZero() {
			return true
		}
	}
	return false
}

func newestKnownSession(unknown []unverifiedSession) time.Time {
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
	out, err := git(path, "log", "-1", "--format=%ct")
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
	out, err := git(path, "--no-optional-locks", "ls-files", "-m", "-o", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	staged, err := git(path, "--no-optional-locks", "diff", "--cached", "--name-only", "--diff-filter=d", "-z")
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

// freshWorktreeActivity refreshes both sources under a fresh session index
// at now; discovery's cached index cannot authorize removal. Errors are fatal:
// removal must fail closed when activity is unreadable. Unverified sessions
// ride along so the caller can scope them per candidate instead of failing
// globally.
func freshWorktreeActivity(path string, now time.Time) (time.Time, []unverifiedSession, error) {
	idx, unverified, err := indexSessionsStrict(nil)
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("refresh agent sessions: %w", err)
	}
	last, err := readWorktreeActivityStrict(path, idx, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	return last, unverified, nil
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first agent session.
// nowFunc() is the WTM_NOW test seam: in the container harness it is pinned to a
// fixed time so goldens stay byte-stable. Future mtimes are clamped to that same
// now so a session newer than the test clock cannot break determinism.
func worktreeLastUsed(path string, idx sessionIndex) time.Time {
	newest, _ := readWorktreeActivityLenient(path, idx, nowFunc())
	return newest
}

// readWorktreeActivityLenient collects activity timestamps relative to now,
// ignoring errors. Used for display where failure must not block output.
func readWorktreeActivityLenient(path string, idx sessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, false)
}

// readWorktreeActivityStrict collects activity timestamps relative to now,
// failing on any error. Used immediately before idle-based removal where
// unreadable activity must prevent reclamation.
func readWorktreeActivityStrict(path string, idx sessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, true)
}

// collectActivity merges agent sessions, the checkout backlink and Git
// modification times, clamping any future timestamp to now.
func collectActivity(path string, idx sessionIndex, now time.Time, strict bool) (time.Time, error) {
	newest, err := sessionLastUsed(path, idx, strict)
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
	dir, err := gitPrivateDir(path)
	if err != nil {
		return err
	}
	return updateNewest(newest, filepath.Join(dir, "index"))
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

func cmdStatus(g *globals, args []string) error {
	wide, err := parseStatusFlags(g, args)
	if err != nil {
		return err
	}
	ws, err := loadWorkspace(g, false, false)
	if err != nil {
		return err
	}
	ws.loadUsage()
	now := nowFunc()
	current := currentWorktreePath(ws)
	if g.json {
		return printStatusJSON(ws.wts, now, ws.ttl, current)
	}
	renderStatus(ws, wide, now, current, g.color)
	return nil
}

// renderStatus prints the table, its legend then the detail block in that order.
func renderStatus(ws workspace, wide bool, now time.Time, current, colorMode string) {
	budget := widthBudget()
	cols := statusSchema(wide, ws.wts)
	printStatusTable(cols, ws.wts, now, ws.ttl, colorMode, budget)
	printCostLegend(costLegendNeeded(cols, ws.wts, current, now, ws.ttl, budget), colorMode, budget)
	printStatusDetail(ws.wts, current, now, colorMode, budget)
}

// parseStatusFlags parses and validates the status flags. --wide shapes only
// the table; the JSON contract always reports every integration, so the
// combination is a usage error, not a silent no-op.
func parseStatusFlags(g *globals, args []string) (bool, error) {
	fs := subFlags("status", g)
	wide := fs.Bool("wide", false, "show one column per integration")
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return false, err
	}
	if len(pos) > 0 {
		return false, usagef("status: unexpected argument %q", pos[0])
	}
	if *wide && g.json {
		return false, usagef("status: --wide has no effect with --json (JSON already reports all integrations)")
	}
	return *wide, nil
}

// nowFunc is the clock seam. Production reads the wall clock; the container
// harness pins WTM_NOW so every clock read is deterministic in goldens: status
// ages, worktreeLastUsed and the gc TTL decision all draw from this one seam.
// A malformed value is a test-setup error, so disclose it instead of hiding it.
func nowFunc() time.Time {
	s := os.Getenv("WTM_NOW")
	if s == "" {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		warnf("WTM_NOW %q is not RFC3339: using the wall clock", s)
		return time.Now()
	}
	return t
}

// loadUsage attaches every worktree's cost/active-time summary. The catalog is
// loaded only when the repo owns a Claude session, so a Pi-only repo never
// reaches the network. An unreadable session degrades to a warning: status is
// read-only and must still report what it knows.
func (ws *workspace) loadUsage() {
	cat := ws.usageCatalog()
	// Read the credential proof once: it is a per-run snapshot, not per-worktree.
	subs := loadSubscriptions()
	for i := range ws.wts {
		ws.attachUsage(i, cat, subs)
	}
}

// usageCatalog loads the price catalog only for a repo with Claude sessions, so
// a Pi-only repo never reaches the network.
func (ws *workspace) usageCatalog() catalog {
	if !ws.idx.hasHarness(ws.wts, claudeAgent.name) {
		return nil
	}
	cat, err := loadCatalog()
	if err != nil {
		warnf("claude cost unavailable: %s", err)
		// A nil catalog makes every harness report unknown, which is the honest
		// answer when no catalog is available at all.
		return nil
	}
	return cat
}

// attachUsage stores one worktree's usage, keeping a total loss so the loss
// reads as unknown, never as no usage.
func (ws *workspace) attachUsage(i int, cat catalog, subs subscriptions) {
	u, err := buildUsage(ws.wts[i].Path, ws.idx, cat, subs)
	if err != nil {
		warnf("usage for %s: %s", displayPath(ws.wts[i].Path), err)
	}
	if u.hasEvents() || u.partial {
		ws.wts[i].Usage = &u
	}
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

func lastUsedCell(w worktree) string {
	if w.LastUsed.IsZero() {
		return "never"
	}
	return w.LastUsed.Format("2006-01-02")
}

// upstreamCell is the status display for a branch's upstream; "-" when the
// branch has none, so whether gc will delete a remote branch is obvious.
func upstreamCell(w worktree) string {
	if !w.Upstream.present() {
		return "-"
	}
	return w.Upstream.Short
}

// dirtyCell is the gc picker's uncommitted-change signal. "-" keeps the column
// scannable and mirrors the inventory columns' neutral value.
func dirtyCell(w worktree) string {
	if w.Dirty {
		return "dirty"
	}
	return "-"
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

// columnContext carries the volatile inputs a cell may need, so cell funcs
// stay pure and the registry stays the single source of truth. active is the
// project's applicable capabilities, so the grouped cell and the wide columns
// agree on what to show and what to mark n/a. gridRows fills it, so no caller
// can build a grid whose cells disagree about applicability.
type columnContext struct {
	now    time.Time
	ttl    time.Duration
	active []capability
}

// worktreeColumn is one grid column: its header, its projection from a
// worktree, and its fit policy. shrinkRank is the elision order (0 = pinned);
// floor is the smallest width a shrinkable column may be elided to. This
// registry is the single source of truth for the status table and the gc
// picker, so the two can never drift apart.
type worktreeColumn struct {
	header     string
	cell       func(worktree, columnContext) string
	rightAlign bool
	shrinkRank int
	floor      int
	// unelidable columns must not be middle-elided: a cut currency hides its
	// magnitude, which reads as a wrong number. Such a column keeps its natural
	// width and is dropped whole (with the stderr disclosure) when it cannot fit.
	unelidable bool
}

// cell funcs wrap the display formatters so the registry stays declarative.
func cellPath(w worktree, _ columnContext) string     { return displayPath(w.Path) }
func cellBranch(w worktree, _ columnContext) string   { return w.branchName() }
func cellUpstream(w worktree, _ columnContext) string { return upstreamCell(w) }
func cellLastUsed(w worktree, _ columnContext) string { return lastUsedCell(w) }
func cellStatus(w worktree, c columnContext) string   { return w.status(c.now, c.ttl) }

// cellAge is the wall-clock life of the worktree; "-" when the creation mark
// is absent, so an unknown age never reads as a real zero.
func cellAge(w worktree, c columnContext) string {
	if w.Created.IsZero() {
		return "-"
	}
	return humanDuration(c.now.Sub(w.Created))
}

// partialValue prefixes the shared "not exact" marker. A partial total must not
// render a lost-data value as exact; zero-with-loss stays unknown below.
func partialValue(s string, partial bool) string {
	if partial {
		return "~" + s
	}
	return s
}

// activeCellValue owns the ACTIVE cell grammar and reports whether the value
// carries the partial marker. A partial total reads "unknown" when it has no
// readable events, or no readable span, and "~<duration>" when it has some.
// The legend reads the flag, so it never explains a marker that cannot render.
func activeCellValue(w worktree) (string, bool) {
	if w.Usage == nil {
		return "-", false
	}
	if w.Usage.partial {
		active := w.Usage.active()
		if !w.Usage.hasEvents() || active == 0 {
			return "unknown", false
		}
		return partialValue(humanDuration(active), true), true
	}
	return humanDuration(w.Usage.active()), false
}

// cellActive is the union of the worktree's session spans; "-" only when the
// worktree has no events. A worktree with point events has a zero span, which
// reads as "0s": measured-but-empty usage must not hide behind "-".
func cellActive(w worktree, _ columnContext) string {
	s, _ := activeCellValue(w)
	return s
}

// cellCost is the billed amount: what the credentials actually charge. "-" when
// there are no events and "unknown" when any contributor is unpriced. A proven
// subscription's pay-as-you-go price renders beside it in its own LIST column,
// so one header always labels one right-aligned number.
func cellCost(w worktree, _ columnContext) string {
	if w.Usage == nil {
		return "-"
	}
	return w.Usage.cost().billed.cell()
}

// cellList is the LIST cell for one worktree: the pay-as-you-go price of the
// same tokens, "-" unless a proven subscription made it differ.
func cellList(w worktree, _ columnContext) string { return listCell(w.cost()) }

// listCell renders a pair's list side. One rule serves the worktree cell and
// the model cell, so both recede to "-" on the same condition.
func listCell(p costPair) string {
	if !p.showsList() {
		return "-"
	}
	return p.list.cell()
}

// cellTemp shows the remaining idle window, or a dash for permanent checkouts.
func cellTemp(w worktree, c columnContext) string {
	if !w.Temp {
		return "-"
	}
	return remainingWindowLabel(w.tempExpiresAt(), c.now)
}

// cellIntegrations lists the integrations the worktree actually has: the token
// per present capability, token~ per partial. Absent capabilities are omitted,
// so the cell claims only what is there; "-" when there is nothing to claim.
func cellIntegrations(w worktree, c columnContext) string {
	var tokens []string
	for _, capItem := range c.active {
		if s := w.Caps[capItem.id]; s != capAbsent {
			tokens = append(tokens, capItem.groupedToken(s))
		}
	}
	if len(tokens) == 0 {
		return "-"
	}
	return strings.Join(tokens, " ")
}

func cellUncommitted(w worktree, _ columnContext) string { return dirtyCell(w) }

var (
	// Core shrink ranks start at 2: rank 1 belongs to the data-dependent
	// columns (the capabilities, the token group and LIST), which give up width
	// first.
	// The identity columns give up width first, in PATH/UPSTREAM/BRANCH/LAST USED
	// order, so the cost/time data outlives the columns that name a worktree.
	// AGE, ACTIVE and COST elide next; their floor is their header width, so
	// those three are never rendered narrower than their headers. INTEGRATIONS
	// gives up width last.
	colPath     = worktreeColumn{header: "PATH", cell: cellPath, shrinkRank: 2, floor: 12}
	colBranch   = worktreeColumn{header: "BRANCH", cell: cellBranch, shrinkRank: 4, floor: 8}
	colUpstream = worktreeColumn{header: "UPSTREAM", cell: cellUpstream, shrinkRank: 3, floor: 8}
	colLastUsed = worktreeColumn{header: "LAST USED", cell: cellLastUsed, rightAlign: true, shrinkRank: 5, floor: 10}
	colAge      = worktreeColumn{header: "AGE", cell: cellAge, rightAlign: true, shrinkRank: 6, floor: 3}
	colActive   = worktreeColumn{header: "ACTIVE", cell: cellActive, rightAlign: true, shrinkRank: 7, floor: 6}
	colCost     = worktreeColumn{header: "COST", cell: cellCost, rightAlign: true, shrinkRank: 8, floor: 4, unelidable: true}
	// colList is the data-dependent pay-as-you-go column beside COST (see
	// withListColumn). It drops whole and it drops first: a counterfactual price
	// is the most dispensable number here, and a price must never middle-cut.
	colList   = worktreeColumn{header: "LIST", cell: cellList, rightAlign: true, shrinkRank: 1, unelidable: true}
	colStatus = worktreeColumn{header: "STATUS", cell: cellStatus}
	// colTemp is pinned (rank 0): the reclaim countdown must never be elided, so
	// it survives every width while AGE/ACTIVE/COST give up room first.
	colTemp         = worktreeColumn{header: "TEMP", cell: cellTemp}
	colIntegrations = worktreeColumn{header: "INTEGRATIONS", cell: cellIntegrations, shrinkRank: 9, floor: 6}
	colUncommitted  = worktreeColumn{header: "UNCOMMITTED", cell: cellUncommitted}

	// Detail-block headers carry no cell: the block renders pre-formatted model
	// rows, and these entries exist only so the detail grid obeys the same shrink
	// policy as the status table. The token group (see tokenColumns) is the pure
	// data and gives up width first, dropping whole (rank 1); MODEL is identity
	// and follows; TIME next; COST reuses colCost.
	colModel = worktreeColumn{header: "MODEL", shrinkRank: 2}
	colTime  = worktreeColumn{header: "TIME", rightAlign: true, shrinkRank: 7, floor: 6}
)

// tokenColumns builds the token group's grid columns from detailTokenMeta, so a
// header cannot drift from the cell under it. The group stands or falls together
// in the width fit, because a hit rate alone — without the counts it comes from —
// says nothing.
func tokenColumns() []worktreeColumn {
	cols := make([]worktreeColumn, len(detailTokenMeta))
	for i, d := range detailTokenMeta {
		cols[i] = worktreeColumn{header: d.glyph + " " + d.label, rightAlign: true, shrinkRank: 1}
	}
	return cols
}

// coreColumns is the shared prefix of every status schema: identity, tracking,
// activity, cost and the temp countdown. statusColumns appends the grouped
// INTEGRATIONS cell; --wide replaces it with one column per capability, so a
// new core column reaches both.
var coreColumns = []worktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colAge, colActive, colCost, colStatus, colTemp}

// statusColumns is the default status schema; tests and the right-align choice
// derive from it so the header contract has one source.
var statusColumns = append(slices.Clone(coreColumns), colIntegrations)

// allColumns is every column a grid can render: the status columns, the gc
// picker's UNCOMMITTED, the detail block's columns, and LIST. The fit policy is
// derived from columnRegistry(), which adds the capability columns.
func allColumns() []worktreeColumn {
	cols := append(slices.Clone(statusColumns), colUncommitted)
	// colList is data-dependent in the printed schemas (see withListColumn) but
	// always registered, so the width policy knows its rank and its unelidable
	// rule before any row decides to show it.
	return append(append(cols, detailColumns()...), colList)
}

// columnRegistry maps header names to their column definition. It includes
// allColumns plus every capability column, so the shrink logic can look up
// any column's rank and floor by header name. A function (not a package var)
// so the registry has no hidden ordering dependency on package initialization
// and the slice cannot be mutated at runtime.
func columnRegistry() map[string]worktreeColumn { return columnRegistryFor(capabilities()) }

func init() {
	// WHY (an eager guard, not registry state — see the no-package-var rule
	// above): capability ids become --wide headers; a collision with a core
	// header must fail at startup, not at the first status render. Go
	// initializes the agent/capability descriptors before init runs.
	_ = columnRegistry()
}

// columnRegistryFor builds the registry for the given capabilities and panics
// when a capability header would overwrite a core column. The capability list
// is a parameter so the collision guard is testable with a synthetic entry.
func columnRegistryFor(caps []capability) map[string]worktreeColumn {
	cols := allColumns()
	r := make(map[string]worktreeColumn, len(cols)+len(caps))
	core := make(map[string]bool, len(cols))
	for _, c := range cols {
		r[c.header] = c
		core[c.header] = true
	}
	for _, c := range caps {
		col := c.column()
		if core[col.header] {
			panic(fmt.Sprintf("capability %q collides with core column %q", c.id, col.header))
		}
		r[col.header] = col
	}
	return r
}

func columnHeaders(cols []worktreeColumn) []string {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	return headers
}

func columnRightAligns(cols []worktreeColumn) []string {
	var right []string
	for _, c := range cols {
		if c.rightAlign {
			right = append(right, c.header)
		}
	}
	return right
}

// gridRows projects worktrees through a column selection; it is the only place
// a table's cells are produced, so status and the gc picker share one schema.
func gridRows(cols []worktreeColumn, wts []worktree, ctx columnContext) [][]string {
	// WHY here, not in the caller: a caller that forgot applicability would
	// render every INTEGRATIONS cell as "-" with no error.
	ctx.active = activeCapabilities(wts)
	rows := make([][]string, 0, len(wts))
	for _, w := range wts {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = c.cell(w, ctx)
		}
		rows = append(rows, row)
	}
	return rows
}

// statusColumnsFor picks the schema: the compact grouped default, or the
// --wide view that expands every registered capability into its own column.
// Both share coreColumns and the wide tail comes from the capability registry,
// so no core list is hand-maintained twice. Column order in --wide follows
// capabilities(): project integrations first (chunkhound, mcp), then agents in
// agents() order (pi, claude). Reordering agents() changes the column order.
func statusColumnsFor(wide bool) []worktreeColumn {
	if !wide {
		return slices.Clone(statusColumns)
	}
	cols := slices.Clone(coreColumns)
	for _, c := range capabilities() {
		cols = append(cols, c.column())
	}
	return cols
}

// statusSchema is the status table's printed schema: the chosen columns plus
// the data-dependent LIST column beside COST when any worktree carries a list
// price. One function, so the renderer and the legend cannot disagree.
func statusSchema(wide bool, wts []worktree) []worktreeColumn {
	return withListColumn(statusColumnsFor(wide), repoShowsList(wts))
}

// repoShowsList reports whether any worktree carries a list price, so the table
// grows its LIST column only when it has a value to show. A column of dashes is
// noise, so the schema follows the data, exactly as the capability columns do.
func repoShowsList(wts []worktree) bool {
	for _, w := range wts {
		if w.cost().showsList() {
			return true
		}
	}
	return false
}

// withListColumn inserts the data-dependent LIST column beside COST. A schema
// without COST is returned unchanged, so any caller can pass any schema. The
// head is cloned, so a caller's slice (often a package-level schema) is never
// mutated through append.
func withListColumn(cols []worktreeColumn, show bool) []worktreeColumn {
	i := slices.IndexFunc(cols, func(c worktreeColumn) bool { return c.header == colCost.header })
	if !show || i < 0 {
		return cols
	}
	out := slices.Clone(cols[:i+1])
	return append(append(out, colList), cols[i+1:]...)
}

func printStatusTable(cols []worktreeColumn, wts []worktree, now time.Time, ttl time.Duration, colorMode string, budget int) {
	ctx := columnContext{now: now, ttl: ttl}
	printTable(columnHeaders(cols), gridRows(cols, wts, ctx), colorMode, statusCell, columnRightAligns(cols), budget)
}

type jsonWorktree struct {
	Path          string                      `json:"path"`
	Branch        string                      `json:"branch"`
	Upstream      string                      `json:"upstream"`
	Main          bool                        `json:"main"`
	Current       bool                        `json:"current"`
	LastUsed      *string                     `json:"lastUsed"`
	Status        string                      `json:"status"`
	AgeSeconds    *int64                      `json:"ageSeconds"`
	Cost          *float64                    `json:"cost"`
	CostEstimated bool                        `json:"costEstimated,omitempty"`
	ListCost      *optCost                    `json:"listCost,omitempty"`
	ListCostEst   bool                        `json:"listCostEstimated,omitempty"`
	ActiveSeconds *int64                      `json:"activeSeconds,omitempty"`
	Usage         map[string]jsonHarnessUsage `json:"usage,omitempty"`
	// Partial reports a total that lost an unreadable session, so a partial
	// sum never reads as complete. Omitted when the total is whole.
	Partial bool `json:"partial,omitempty"`
	// InputOutputRatio is the worktree-level input:output ratios; omitted when
	// the worktree recorded no output to divide by.
	InputOutputRatio *jsonInputOutputRatio `json:"inputOutputRatio,omitempty"`
	Integrations     map[string]string     `json:"integrations"`
	// Temp metadata carries the reclaim countdown; expiresAt is null for a
	// permanent checkout, so a consumer never re-derives the deadline.
	Temp      bool    `json:"temp"`
	ExpiresAt *string `json:"expiresAt"`
}

// jsonHarnessUsage is one harness's machine contract. cost is always present
// (null when unpriced); activeSeconds and models follow the harness's events.
type jsonHarnessUsage struct {
	Cost          *float64                  `json:"cost"`
	CostEstimated bool                      `json:"costEstimated,omitempty"`
	ListCost      *optCost                  `json:"listCost,omitempty"`
	ListCostEst   bool                      `json:"listCostEstimated,omitempty"`
	ActiveSeconds int64                     `json:"activeSeconds"`
	Partial       bool                      `json:"partial,omitempty"`
	Models        map[string]jsonModelUsage `json:"models"`
}

type jsonModelUsage struct {
	Cost            *float64                 `json:"cost"`
	CostEstimated   bool                     `json:"costEstimated,omitempty"`
	ListCost        *optCost                 `json:"listCost,omitempty"`
	ListCostEst     bool                     `json:"listCostEstimated,omitempty"`
	ActiveSeconds   int64                    `json:"activeSeconds"`
	Partial         bool                     `json:"partial,omitempty"`
	Tiers           map[string]jsonTierUsage `json:"tiers,omitempty"`
	CacheMissTokens *int64                   `json:"cacheMissTokens,omitempty"`
	CacheHitRate    *float64                 `json:"cacheHitRate,omitempty"`
}

// jsonTierUsage is one token tier's machine contract: the token count plus the
// billed cost and optional list price, mirroring the model-level cost shape so
// the billed/list invariant holds identically at every level.
type jsonTierUsage struct {
	Tokens        int64    `json:"tokens"`
	Cost          *float64 `json:"cost"`
	CostEstimated bool     `json:"costEstimated,omitempty"`
	ListCost      *optCost `json:"listCost,omitempty"`
	ListCostEst   bool     `json:"listCostEstimated,omitempty"`
}

// jsonInputOutputRatio is the machine contract for the summary's three
// input:output ratios. The three share one denominator, so total = cached +
// uncached before rounding; each field is trimmed to 1e-6 independently (see
// roundRate), so the rounded values can differ by that much. The JSON carries
// each so a consumer never re-derives the split.
type jsonInputOutputRatio struct {
	Total    float64 `json:"total"`
	Cached   float64 `json:"cached"`
	Uncached float64 `json:"uncached"`
	// Partial marks shares computed from a total that lost records. The values
	// remain the readable subset; the flag says they may change.
	Partial bool `json:"partial,omitempty"`
}

// optCost is the optional list price beside a machine cost. The key is present
// only when a proven subscription makes the billed and list values differ (see
// listCostJSON); its nil pointer is therefore not a "no subscription" signal.
// When present, a nil v renders null: the subscription is proven but the list
// is unknown.
type optCost struct{ v *float64 }

func (o optCost) MarshalJSON() ([]byte, error) {
	if o.v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.v)
}

// listCostJSON emits the list price only when showsList() is true and at least
// one side is priced. Unproven output, a proven equal price, and a proven
// both-unknown partial all omit the key. When present, a null value means the
// list is unknown.
func listCostJSON(p costPair) *optCost {
	if !p.showsList() || p.bothUnpriced() {
		return nil
	}
	return &optCost{v: p.list.jsonValue()}
}

func printStatusJSON(wts []worktree, now time.Time, ttl time.Duration, current string) error {
	active := activeCapabilities(wts)
	out := make([]jsonWorktree, 0, len(wts))
	for _, w := range wts {
		out = append(out, jsonWorktreeFrom(w, now, ttl, active, current))
	}
	return printJSON(out)
}

// jsonWorktreeFrom projects one worktree into the machine contract.
func jsonWorktreeFrom(w worktree, now time.Time, ttl time.Duration, active []capability, current string) jsonWorktree {
	j := jsonWorktree{
		// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
		Path: w.Path, Branch: w.branchName(), Upstream: w.Upstream.Short, Main: w.Main,
		Current: w.Path == current, Status: w.status(now, ttl),
		Integrations: integrationStates(w, active),
	}
	applyCostJSON(&j, w.cost())
	j.LastUsed = rfc3339Ptr(w.LastUsed)
	j.AgeSeconds = ageSeconds(w, now)
	j.InputOutputRatio = inputOutputRatioJSON(w.Usage)
	applyUsageJSON(&j, w.Usage)
	applyTempJSON(&j, w)
	return j
}

// applyCostJSON projects the billed/list pair; listCost is emitted only when a
// proven subscription makes it differ, and its estimate flag qualifies it.
func applyCostJSON(j *jsonWorktree, cost costPair) {
	j.Cost = cost.billed.jsonValue()
	j.CostEstimated = cost.billed.isEstimated()
	if lc := listCostJSON(cost); lc != nil {
		j.ListCost = lc
		j.ListCostEst = cost.list.isEstimated()
	}
}

// applyTempJSON mirrors the table's TEMP cell: expiresAt is emitted only for a
// temp checkout, so a permanent one serializes temp:false/expiresAt:null.
func applyTempJSON(j *jsonWorktree, w worktree) {
	j.Temp = w.Temp
	if w.Temp {
		s := formatTempDeadline(w.tempExpiresAt())
		j.ExpiresAt = &s
	}
}

func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func ageSeconds(w worktree, now time.Time) *int64 {
	if w.Created.IsZero() {
		return nil
	}
	s := seconds(now.Sub(w.Created))
	return &s
}

// applyUsageJSON adds the event-derived fields; an eventless worktree omits
// them. A total loss keeps partial:true but no measured values: activeSeconds
// and usage would otherwise read as a false measured zero.
func applyUsageJSON(j *jsonWorktree, u *worktreeUsage) {
	if u == nil {
		return
	}
	j.Partial = u.partial
	if !u.hasEvents() {
		return
	}
	a := seconds(u.active())
	j.ActiveSeconds = &a
	j.Usage = usageJSON(u)
}

// cost is the worktree's price pair; empty when it has no events.
func (w worktree) cost() costPair {
	if w.Usage == nil {
		return costPair{}
	}
	return w.Usage.cost()
}

func usageJSON(u *worktreeUsage) map[string]jsonHarnessUsage {
	out := make(map[string]jsonHarnessUsage, len(u.harnesses))
	for name, h := range u.harnesses {
		out[name] = harnessUsageJSON(h)
	}
	return out
}

// harnessUsageJSON projects one harness's totals and its per-model contracts.
func harnessUsageJSON(h *harnessUsage) jsonHarnessUsage {
	models := make(map[string]jsonModelUsage, len(h.models))
	for model, m := range h.models {
		models[model] = modelUsageJSON(m)
	}
	hu := jsonHarnessUsage{Cost: h.cost.billed.jsonValue(), CostEstimated: h.cost.billed.isEstimated(), ActiveSeconds: seconds(h.active()), Partial: h.partial, Models: models}
	if lc := listCostJSON(h.cost); lc != nil {
		hu.ListCost = lc
		hu.ListCostEst = h.cost.list.isEstimated()
	}
	return hu
}

// modelUsageJSON projects one model's totals, derived cache metrics and list price.
func modelUsageJSON(m *modelUsage) jsonModelUsage {
	mu := jsonModelUsage{Cost: m.cost.billed.jsonValue(), CostEstimated: m.cost.billed.isEstimated(), ActiveSeconds: seconds(m.active()), Partial: m.partial, Tiers: tiersJSON(m.tokens, m.tiers)}
	mu.CacheMissTokens, mu.CacheHitRate = cacheJSON(m.tokens)
	if lc := listCostJSON(m.cost); lc != nil {
		mu.ListCost = lc
		mu.ListCostEst = m.cost.list.isEstimated()
	}
	return mu
}

// tiersJSON projects a model's token tiers into the machine contract. It returns
// nil when no tier recorded tokens, so a cost-only legacy record (an older Pi
// session or spawn result) omits the breakdown instead of four empty tiers.
func tiersJSON(tokens promptTokens, tiers tokenTiers) map[string]jsonTierUsage {
	if tokens.total() == 0 {
		return nil
	}
	out := make(map[string]jsonTierUsage, numTiers)
	for t := tier(0); t < numTiers; t++ {
		tu := jsonTierUsage{Tokens: tokens.tier(t), Cost: tiers[t].billed.jsonValue(), CostEstimated: tiers[t].billed.isEstimated()}
		if lc := listCostJSON(tiers[t]); lc != nil {
			tu.ListCost = lc
			tu.ListCostEst = tiers[t].list.isEstimated()
		}
		out[tierMeta[t]] = tu
	}
	return out
}

// cacheJSON projects a model's derived cache metrics into the machine contract.
// Both keys are omitted when the model recorded no prompt tokens. The hit rate
// is also omitted when the guard trips — hits with no recorded build make the
// ratio undefined — so the contract carries no false 100%.
func cacheJSON(t promptTokens) (*int64, *float64) {
	if t.context() == 0 {
		return nil, nil
	}
	miss := t.miss()
	if rate, ok := t.hitRate(); ok {
		r := roundRate(rate)
		return &miss, &r
	}
	return &miss, nil
}

// set populates one field by JSON key. The switch keeps the struct type-safe
// while the meta list drives the enumeration, so display and JSON share one
// source of truth for the ratio set.
func (r *jsonInputOutputRatio) set(key string, v float64) {
	switch key {
	case "total":
		r.Total = v
	case "cached":
		r.Cached = v
	case "uncached":
		r.Uncached = v
	}
}

// inputOutputRatioJSON projects the worktree's input:output ratios. It returns
// nil when no output was recorded: a ratio without a denominator is undefined,
// so the contract stays silent rather than emit an infinity.
func inputOutputRatioJSON(u *worktreeUsage) *jsonInputOutputRatio {
	if u == nil {
		return nil
	}
	t := u.tokens()
	if !t.ratiosDefined() {
		return nil
	}
	r := jsonInputOutputRatio{Partial: u.partial}
	for _, d := range detailRatioMeta {
		v, _ := t.outputRatio(d.num(t))
		r.set(d.jsonKey, roundRate(v))
	}
	return &r
}

// costLegend is the disclosure the rendered cost columns need: the "~"
// estimate reasons the table carries and whether its LIST column survives the
// width fit. The detail block's own legend is computed by printStatusDetail
// from the same fitDetailBlock result it renders with, so the breakdown needs
// no separate legend struct here.
type costLegend struct {
	tableReasons estimateReasons
	list         bool
	partial      bool
}

// costLegendNeeded reports what the table-level cost disclosure must explain.
// It reuses the same width fit as the renderer, so a dropped column cannot leave
// a legend with nothing to explain. The detail block's legend is computed by
// printStatusDetail from the same fit it renders with, so no detail fields are
// needed here.
func costLegendNeeded(cols []worktreeColumn, wts []worktree, _ string, now time.Time, ttl time.Duration, budget int) costLegend {
	var l costLegend
	l.table(cols, wts, columnContext{now: now, ttl: ttl}, budget)
	return l
}

// table folds the status table's needs in: the reasons its rendered cells carry
// and its LIST column, when the width fit keeps it.
func (l *costLegend) table(cols []worktreeColumn, wts []worktree, ctx columnContext, budget int) {
	rows := gridRows(cols, wts, ctx)
	if costColumnKept(cols, rows, budget) {
		for _, w := range wts {
			l.tableReasons = l.tableReasons.merge(w.cost().reasons())
		}
	}
	l.list = columnKept(cols, rows, colList.header, budget)
	l.partial = columnKept(cols, rows, colActive.header, budget) && anyPartialActive(wts)
}

// anyPartialActive reports whether any table row renders a partial "~" ACTIVE
// value, so the legend never explains a marker that cannot render.
func anyPartialActive(wts []worktree) bool {
	for _, w := range wts {
		if _, marked := activeCellValue(w); marked {
			return true
		}
	}
	return false
}

// detailFit is the breakdown block's width fit and its legend needs. One fit
// serves the renderer and the legend pass, so the two cannot disagree about
// what the block renders.
type detailFit struct {
	cols    []worktreeColumn
	rows    [][]string
	reasons estimateReasons
	list    bool
	partial bool
	dropped bool
}

// fitDetailBlock fits the block at the budget silently; the caller discloses a
// dropped token group (the renderer) or stays silent (the legend pass). The
// reasons fold in the summary pair's caveats (unless the summary hid the cost
// row) and the model rows' caveats (unless the fit dropped COST). The fit
// mirrors the renderer exactly — the token group drops first and frees width —
// so a column the block actually renders is never judged dropped here.
func fitDetailBlock(cur worktree, now time.Time, budget int) detailFit {
	var f detailFit
	if !summaryCostElided(detailSummaryRows(cur, now), budget) {
		f.reasons = f.reasons.merge(cur.cost().reasons())
	}
	if cur.Usage == nil {
		return f
	}
	cols, rows := detailSchema(cur.Usage)
	f.cols, f.rows, f.dropped = fitTokenGroupPlan(cols, rows, budget)
	if costColumnKept(f.cols, f.rows, budget) {
		f.reasons = f.reasons.merge(modelsEstimated(cur.Usage))
	}
	f.list = columnKept(f.cols, f.rows, colList.header, budget)
	f.partial = detailPartial(cur, f.cols, f.rows, now, budget)
	return f
}

// detailPartial reports whether the fitted detail block renders a partial
// marker: the summary ACTIVE value, a surviving ratio row, a model time, or a
// surviving token cell. A total loss renders only "unknown", so it needs no
// dangling "~" legend.
func detailPartial(cur worktree, cols []worktreeColumn, rows [][]string, now time.Time, budget int) bool {
	if cur.Usage == nil || !cur.Usage.partial {
		return false
	}
	if detailSummaryPartial(cur, now, budget) {
		return true
	}
	return detailModelPartial(cur.Usage, cols, rows, budget)
}

// detailSummaryPartial reports a partial marker in the summary: the ACTIVE value
// or a ratio row that survives the width fit.
func detailSummaryPartial(cur worktree, now time.Time, budget int) bool {
	if _, marked := activeCellValue(cur); marked {
		return true
	}
	return cur.Usage.tokens().ratiosDefined() && !summaryRatioHidden(detailSummaryRows(cur, now), budget)
}

// detailModelPartial reports a partial model row whose TIME or token cell survives.
func detailModelPartial(u *worktreeUsage, cols []worktreeColumn, rows [][]string, budget int) bool {
	timeKept := columnKept(cols, rows, colTime.header, budget)
	tokensKept := tokenGroupKept(cols)
	for _, m := range u.mergedModels() {
		if m.partial && ((timeKept && m.active() > 0) || (tokensKept && m.tokens.total() > 0)) {
			return true
		}
	}
	return false
}

// costColumnKept reports whether the COST column survives the width fit, so a
// caller can know whether a cost cell will actually render.
func costColumnKept(cols []worktreeColumn, rows [][]string, budget int) bool {
	return columnKept(cols, rows, colCost.header, budget)
}

// columnKept reports whether a named column survives the width fit, so a caller
// can know whether that column's cells (and any legend explaining them) render.
func columnKept(cols []worktreeColumn, rows [][]string, header string, budget int) bool {
	widths := columnWidths(columnHeaders(cols), rows, budget)
	for i, c := range cols {
		if c.header == header {
			return widths[i] != 0
		}
	}
	return false
}

// modelsEstimated reports which legend reasons any model row rests on. Only the
// model total counts: per-tier costs are no longer rendered, and a tier's
// assumption propagates into the total, so the total's own reasons already
// cover every estimate the visible rows rest on.
func modelsEstimated(u *worktreeUsage) estimateReasons {
	var r estimateReasons
	if u == nil {
		return r
	}
	for _, h := range u.harnesses {
		for _, m := range h.models {
			r = r.merge(m.cost.reasons())
		}
	}
	return r
}

// legendEntry is one labeled clause: the label names the thing, the text explains
// it, and both stay apart so every "=" can share one column (see legendLines).
type legendEntry struct{ label, text string }

// legendGroup is a titled group of clauses. The title prints alone, then its
// clauses indent under it, so a group heading never reads as another clause. An
// empty title prints its clauses flush left: the LIST line and the "~" caveats
// belong to no group.
type legendGroup struct {
	title string
	items []legendEntry
}

// listLegend is the LIST column's SSOT explanation, shared by printCostLegend
// and printDetailLegend so each block explains its own LIST column.
var listLegend = legendEntry{"list", "pay-as-you-go price of the same tokens"}

// estimateLabel is the shared label of the "~" caveat clauses. Each reason text
// stays its own SSOT constant in rates.go / usage.go. partialLabel shares the
// same "not exact" glyph but names loss, not an assumption.
const (
	estimateLabel = "~ estimated"
	partialLabel  = "~ partial"
	partialLegend = "unreadable session records may change this value"
)

// legendIndent nests a titled group's clauses under its heading.
const legendIndent = "  "

// indent is a group's clause indent: two cells under a title, none without one.
func (g legendGroup) indent() string {
	if g.title == "" {
		return ""
	}
	return legendIndent
}

// legendLines lays out one legend from its groups: labels padded to the widest
// rendered label across every group, so all "=" share one column whether or not
// a clause sits under a title, then the text. width<=0 prints each clause whole.
func legendLines(groups []legendGroup, width int) []string {
	label := 0
	for _, g := range groups {
		for _, e := range g.items {
			label = max(label, dispWidth(g.indent()+e.label))
		}
	}
	var lines []string
	for _, g := range groups {
		if g.title != "" {
			lines = append(lines, g.title)
		}
		for _, e := range g.items {
			named := g.indent() + e.label
			prefix := named + strings.Repeat(" ", label-dispWidth(named)) + " = "
			lines = append(lines, hangWrapped(prefix, e.text, width)...)
		}
	}
	return lines
}

// hangWrapped puts prefix before the first text line and indents each
// continuation line to the text column, so wrapping never splits the block's
// "=" alignment. A width too small for the prefix falls back to plain wrapping.
func hangWrapped(prefix, text string, width int) []string {
	avail := width - dispWidth(prefix)
	if width <= 0 || avail <= 0 {
		return wrapWords(prefix+text, width)
	}
	parts := wrapWords(text, avail)
	indent := strings.Repeat(" ", dispWidth(prefix))
	for i := 1; i < len(parts); i++ {
		parts[i] = indent + parts[i]
	}
	parts[0] = prefix + parts[0]
	return parts
}

// printCostLegend discloses, once under the table, the caveats the rendered
// cost columns rest on: what LIST holds, then each assumed price a "~" cell
// in the table rests on. Detail marks print under the breakdown (see
// printDetailLegend), so each mark is explained beside the cells that carry
// it. The reason texts are SSOT constants, so the explanation cannot
// drift from the mark it explains. A table with reported prices and no LIST
// column prints nothing here, and one kind of estimate prints only its line.
// The LIST line prints under both blocks when both render the column, so the
// table scanner never meets an unexplained second price.
func printCostLegend(l costLegend, colorMode string, budget int) {
	if !l.tableReasons.any() && !l.list && !l.partial {
		return
	}
	fmt.Println()
	printLegend(legendLines([]legendGroup{tableCostLegend(l)}, budget), colorMode)
}

// tableCostLegend builds the untitled cost clauses in display order.
func tableCostLegend(l costLegend) legendGroup {
	var group legendGroup
	if l.list {
		group.items = append(group.items, listLegend)
	}
	if l.partial {
		group.items = append(group.items, legendEntry{partialLabel, partialLegend})
	}
	if l.tableReasons.cache {
		group.items = append(group.items, legendEntry{estimateLabel, estimateCacheWriteLifetime})
	}
	if l.tableReasons.overage {
		group.items = append(group.items, legendEntry{estimateLabel, estimateSubscriptionOverage})
	}
	return group
}

// detailColumns is the detail block's static table schema: one MODEL row per
// model, TIME, COST, then the token group (total input, the token classes, then
// the derived miss/hit% metrics). A function, like columnRegistry, so the
// headers derive from the metadata lists with no package-init ordering
// dependency. It shares the status column registry (see allColumns), so the
// block obeys the same width policy as the table above: the token group and
// LIST give up width first (dropping whole), MODEL next, then TIME, and COST is
// unelidable, so the cost column outlives the columns that name the rows. LIST
// is inserted beside COST by detailSchema when a model carries a list price.
func detailColumns() []worktreeColumn {
	cols := []worktreeColumn{colModel, colTime, colCost}
	return append(cols, tokenColumns()...)
}

// detailSchema is the breakdown's printed schema and its matching rows. Schema
// and rows are built together, so a cell can never land on the wrong column.
func detailSchema(u *worktreeUsage) ([]worktreeColumn, [][]string) {
	showList := detailShowsList(u)
	return withListColumn(detailColumns(), showList), detailRows(u, showList)
}

// detailShowsList reports whether any model row carries a list price, so the
// breakdown grows its LIST column only when it has a value to show.
func detailShowsList(u *worktreeUsage) bool {
	if u == nil {
		return false
	}
	for _, m := range u.mergedModels() {
		if m.cost.showsList() {
			return true
		}
	}
	return false
}

// printStatusDetail renders the current worktree's usage under the table. The
// block is omitted when no harness in the repo has events; when the current
// worktree alone is empty, the block says so instead of showing empty rows.
func printStatusDetail(wts []worktree, current string, now time.Time, colorMode string, budget int) {
	if !repoHasUsage(wts) {
		return
	}
	cur := findWorktree(wts, current)
	if cur == nil {
		return
	}
	fmt.Println()
	printDetailSummary(*cur, now, colorMode, budget)
	if cur.Usage == nil {
		fmt.Println("no agent usage recorded")
		return
	}
	fmt.Println()
	printDetailTable(*cur, now, colorMode, budget)
}

// printDetailSummary prints the identity and measure rows above the breakdown.
func printDetailSummary(cur worktree, now time.Time, colorMode string, budget int) {
	printKeyValues(dropElidedSummaryRows(detailSummaryRows(cur, now), budget), detailTitleRows, colorMode, budget)
}

// printDetailTable prints the fitted model rows and their legend.
func printDetailTable(cur worktree, now time.Time, colorMode string, budget int) {
	f := fitDetailBlock(cur, now, budget)
	if f.dropped {
		warnf("terminal too narrow; hid %s; widen the terminal or use --json", strings.Join(tokenColumnNames(), ", "))
	}
	printTable(columnHeaders(f.cols), f.rows, colorMode, statusCell, columnRightAligns(f.cols), budget)
	printDetailLegend(f.reasons, f.list, f.partial, tokenGroupKept(f.cols) && detailHasTokens(cur.Usage), colorMode, budget)
}

// tokenHeaderSet is the set of detail headers owned by the token group.
func tokenHeaderSet() map[string]bool {
	set := make(map[string]bool, len(detailTokenMeta))
	for _, c := range tokenColumns() {
		set[c.header] = true
	}
	return set
}

// tokenColumnNames lists the token-group headers in display order, for
// disclosure.
func tokenColumnNames() []string {
	cols := tokenColumns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.header
	}
	return names
}

// fitTokenGroupPlan is the token group's pure fit decision: the schema and
// rows that survive (the group drops whole, so the grid stays index-aligned),
// and whether anything was dropped. It is silent, so the renderer discloses
// once and the legend pass can ask the same question without repeating the
// disclosure.
func fitTokenGroupPlan(cols []worktreeColumn, rows [][]string, budget int) ([]worktreeColumn, [][]string, bool) {
	widths := columnWidths(columnHeaders(cols), rows, budget)
	token := tokenHeaderSet()
	drop := false
	for i, c := range cols {
		drop = drop || (token[c.header] && widths[i] == 0)
	}
	if !drop {
		return cols, rows, false
	}
	kept, out := withoutTokenGroup(cols, rows)
	return kept, out, true
}

func withoutTokenGroup(cols []worktreeColumn, rows [][]string) ([]worktreeColumn, [][]string) {
	token := tokenHeaderSet()
	keep := make([]int, 0, len(cols))
	kept := make([]worktreeColumn, 0, len(cols))
	for i, c := range cols {
		if !token[c.header] {
			keep = append(keep, i)
			kept = append(kept, c)
		}
	}
	out := make([][]string, len(rows))
	for r, row := range rows {
		out[r] = selectStrings(row, keep)
	}
	return kept, out
}

// detailTitleRows is how many leading detail-block rows are section titles;
// printKeyValues bolds their labels.
const detailTitleRows = 1

// detailCostLabel names the summary cost row; the builder and the narrow-
// terminal drop share it, so they cannot disagree on which row is the cost.
const detailCostLabel = "cost"

// detailSummaryRows is the block's identity row plus its age/active/cost
// measures and, when the worktree recorded output, its input:output ratios. The
// identity value keeps the full path: identifying the current worktree is the
// block's purpose, and the table above carries the elided form.
func detailSummaryRows(cur worktree, now time.Time) [][]string {
	rows := [][]string{
		{"current worktree", displayPath(cur.Path)},
		{"age", detailAge(cur, now)},
		{"active time", detailActive(cur)},
		{detailCostLabel, detailCost(cur)},
	}
	if cur.Usage != nil {
		rows = append(rows, tokenRatioRows(cur.Usage.tokens(), cur.Usage.partial)...)
	}
	return rows
}

// detailRatioMeta is the summary's input:output ratio group in display order:
// the label that names each share and the numerator it divides by. All three
// divide the same denominator (the recorded output), so they are comparable and
// add up (total = cached + uncached). The row builder and the narrow-terminal
// drop read this one list, so they cannot drift.
var detailRatioMeta = []struct {
	label   string
	jsonKey string
	num     func(promptTokens) int64
}{
	{"total in:out", "total", promptTokens.context},
	{"cached in:out", "cached", func(t promptTokens) int64 { return t.cacheRead }},
	{"uncached in:out", "uncached", promptTokens.miss},
}

// tokenRatioRows renders the summary's ratio group. A whole total with no output
// shows nothing: a ratio without a denominator is undefined. A partial total
// shows unknown rows instead, because lost records may hold the output.
func tokenRatioRows(t promptTokens, partial bool) [][]string {
	if !t.ratiosDefined() {
		if !partial {
			return nil
		}
		// A lost record may carry the missing output, so the ratio is unknown
		// rather than absent.
		rows := make([][]string, 0, len(detailRatioMeta))
		for _, d := range detailRatioMeta {
			rows = append(rows, []string{d.label, "unknown"})
		}
		return rows
	}
	rows := make([][]string, 0, len(detailRatioMeta))
	for _, d := range detailRatioMeta {
		v, _ := t.outputRatio(d.num(t))
		rows = append(rows, []string{d.label, partialValue(formatRatio(v), partial)})
	}
	return rows
}

func repoHasUsage(wts []worktree) bool {
	for _, w := range wts {
		if w.Usage != nil && w.Usage.hasEvents() {
			return true
		}
	}
	return false
}

func findWorktree(wts []worktree, path string) *worktree {
	for i := range wts {
		if wts[i].Path == path {
			return &wts[i]
		}
	}
	return nil
}

// dropElidedSummaryRows drops the summary rows the fitted value column cannot
// hold whole: the cost row alone, and the ratio group as a unit. A middle-cut
// number reads as a wrong one, and a partial set of shares reads as arbitrary,
// so each drop is disclosed on stderr instead of rendered elided.
func dropElidedSummaryRows(rows [][]string, budget int) [][]string {
	groups := summaryHiddenGroups(rows, budget)
	var hidden []string
	for _, group := range groups {
		warnf("terminal too narrow; hid %s; widen the terminal or use --json", strings.Join(group, ", "))
		hidden = append(hidden, group...)
	}
	return slices.DeleteFunc(slices.Clone(rows), func(row []string) bool {
		return slices.Contains(hidden, row[0])
	})
}

// summaryRatioHidden reports whether the ratio group is hidden by the summary's
// width fit, so the detail legend never explains a marker that cannot render.
func summaryRatioHidden(rows [][]string, budget int) bool {
	for _, group := range summaryHiddenGroups(rows, budget) {
		if slices.Contains(group, detailRatioMeta[0].label) {
			return true
		}
	}
	return false
}

// summaryHiddenGroups is the shared, silent decision for rendering and legends.
func summaryHiddenGroups(rows [][]string, budget int) [][]string {
	fitted := summaryFittedWidth(rows, budget)
	var hidden [][]string
	for _, group := range [][]string{{detailCostLabel}, ratioLabels()} {
		for _, label := range group {
			if summaryValueElided(rows, label, fitted) {
				hidden = append(hidden, group)
				break
			}
		}
	}
	return hidden
}

// ratioLabels lists the ratio group's labels in display order, for the narrow-
// terminal disclosure. The group drops as a unit, so the warning names every
// label even when one member is what overflowed.
func ratioLabels() []string {
	labels := make([]string, len(detailRatioMeta))
	for i, d := range detailRatioMeta {
		labels[i] = d.label
	}
	return labels
}

// summaryFittedWidth is the value-column width the summary block renders at, so
// a caller can tell whether a row's value survives whole. Callers pass the
// block's own rows, which always have a first row.
func summaryFittedWidth(rows [][]string, budget int) int {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return 0
	}
	_, fitted := sizedKeyValues(rows, budget)
	return fitted[1]
}

// summaryValueElided reports whether a named summary row's value is wider than
// the fitted value column, so its rendered cell would middle-cut.
func summaryValueElided(rows [][]string, label string, fitted int) bool {
	for _, r := range rows {
		if r[0] == label {
			return dispWidth(r[1]) > fitted
		}
	}
	return false
}

// summaryCostElided reports whether the summary's fitted value column cannot
// hold the cost value whole. A middle-cut pair reads as a wrong number, so the
// caller hides the row instead of eliding it.
func summaryCostElided(rows [][]string, budget int) bool {
	for _, group := range summaryHiddenGroups(rows, budget) {
		if slices.Contains(group, detailCostLabel) {
			return true
		}
	}
	return false
}

func detailAge(w worktree, now time.Time) string { return cellAge(w, columnContext{now: now}) }

func detailActive(w worktree) string { return cellActive(w, columnContext{}) }

// detailCost is the summary block's cost value: the billed/list pair, because a
// headerless key/value line has no column header to align, so one compact value
// reads better than two rows. The breakdown renders the same pair as its COST
// and LIST columns; both derive from costPair.
func detailCost(w worktree) string {
	if w.Usage == nil {
		return "-"
	}
	return w.Usage.cost().cell()
}

// detailMetric is one column of the token group: the glyph that tags it, the
// label that names it, and the cell that renders one model's value. Tiers and
// derived metrics share the shape, so the header and the values under it read
// one list and cannot drift.
type detailMetric struct {
	glyph string
	label string
	cell  func(*modelUsage) string
}

// detailTokenMeta is the token group in display order. "in" is the TOTAL prompt
// size (uncached input + cache read + cache write), not the uncached slice, so it
// can never read smaller than "hit"; the derived miss and hit% close the group.
//
// Glyph invariant (guarded by TestStatusDetailTokenGlyphs): one display cell, text
// presentation, no U+FE0F, no color. A 2-cell or color glyph (emoji) overflows
// its box and clips the neighbouring label, and a fallback-font dingbat renders
// thin beside the bold header. So hit/write use the hooked cache arrows and
// miss/hit% use the universally covered MULTIPLICATION SIGN and BLACK CIRCLE.
var detailTokenMeta = []detailMetric{
	{"↑", "in", totalInputCell},
	{"↓", "out", tierCount(tierOutput)},
	{"↩", "hit", tierCount(tierCacheRead)},
	{"↪", "write", tierCount(tierCacheWrite)},
	{"×", "miss", missCount},
	{"●", "hit%", hitRateCell},
}

// tokenLegendTitle headings the token clauses, so the group reads as a titled
// block rather than one more definition. The clauses sit under it (see
// legendGroup).
const tokenLegendTitle = "tokens:"

// tokenLegend spells out the token group's arithmetic, one clause per entry so
// each definition reads on its own. A legend, not just headers, because "in" is
// the total while "hit" and "miss" are its parts: the header alone cannot say how
// the total is counted. hit% caps at 99.9% (see maxHitRate); the legend keeps
// the short arithmetic and the cap lives here so the row width stays stable.
var tokenLegend = []legendEntry{
	{"in", "uncached input + cache read + cache write (total input)"},
	{"miss", "uncached input + cache write"},
	{"hit%", "hit / in"},
}

// detailHasTokens reports whether any model recorded tokens, so the token legend
// prints only when it explains a visible number, never beside a column of dashes.
func detailHasTokens(u *worktreeUsage) bool {
	if u == nil {
		return false
	}
	for _, h := range u.harnesses {
		for _, m := range h.models {
			if m.tokens.total() > 0 {
				return true
			}
		}
	}
	return false
}

// tokenGroupKept reports whether the fitted grid still carries the token group,
// so the legend prints only when its columns do.
func tokenGroupKept(cols []worktreeColumn) bool {
	token := tokenHeaderSet()
	for _, c := range cols {
		if token[c.header] {
			return true
		}
	}
	return false
}

// printDetailLegend prints the breakdown's legend below its rows: the "~"
// estimate reasons its cells carry, what the LIST column holds, then the
// token-group arithmetic, in column order. Columns and their explanation
// therefore stay in one block, and a breakdown that keeps none prints nothing.
func printDetailLegend(reasons estimateReasons, list, partial, tokens bool, colorMode string, budget int) {
	if !reasons.any() && !list && !partial && !tokens {
		return
	}
	// Wrap to the grid's width, so the text that explains the block never widens
	// it; budget==0 (unbounded output) prints every clause whole.
	fmt.Println()
	printLegend(legendLines(detailLegendGroups(costLegend{tableReasons: reasons, list: list, partial: partial}, tokens), budget), colorMode)
}

// detailLegendGroups builds the cost head then the token group in print order.
func detailLegendGroups(l costLegend, tokens bool) []legendGroup {
	var groups []legendGroup
	if head := tableCostLegend(l); len(head.items) > 0 {
		groups = append(groups, head)
	}
	if tokens {
		groups = append(groups, legendGroup{title: tokenLegendTitle, items: tokenLegend})
	}
	return groups
}

// wrapWords breaks text into lines of at most width display cells on word
// boundaries. width<=0 or a line that already fits returns the text unchanged.
func wrapWords(text string, width int) []string {
	if width <= 0 || dispWidth(text) <= width {
		return []string{text}
	}
	var lines []string
	cur := ""
	for _, w := range strings.Fields(text) {
		switch {
		case cur == "":
			cur = w
		case dispWidth(cur)+1+dispWidth(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	return append(lines, cur)
}

// tierCount adapts one token tier to a detailMetric cell.
func tierCount(t tier) func(*modelUsage) string {
	return func(m *modelUsage) string { return tokenCount(m, t) }
}

// detailRows flattens the current worktree into one row per model, ordered by
// billed cost. Each row carries the model's time, its billed cost, the list
// price when the schema shows one, and one cell per token-group column, so the
// breakdown stays beside the model without harness grouping or sub-rows.
func detailRows(u *worktreeUsage, showList bool) [][]string {
	models := u.mergedModels()
	var rows [][]string
	for _, name := range u.sortedModelNames(models) {
		rows = append(rows, detailRow(name, models[name], showList))
	}
	return rows
}

// detailRow projects one model: name, time, billed cost, optional list price,
// then one cell per token-group column.
func detailRow(name string, m *modelUsage, showList bool) []string {
	row := []string{name, modelTimeCell(m), m.cost.billed.cell()}
	if showList {
		row = append(row, listCell(m.cost))
	}
	for _, d := range detailTokenMeta {
		row = append(row, d.cell(m))
	}
	return row
}

// modelTimeCell owns the partial-time grammar: a partial total with no readable
// span is unknown, otherwise a marked duration.
func modelTimeCell(m *modelUsage) string {
	active := m.active()
	if !m.partial {
		return humanDuration(active)
	}
	if active == 0 {
		return "unknown"
	}
	return partialValue(humanDuration(active), true)
}

// partialTokenCell renders one token count from a total that may have lost
// records. present gates presence; value is shown. A missing value with loss is
// unknown, a readable partial value carries the "~" marker, and a whole total
// keeps the existing "-"/count grammar.
func partialTokenCell(value int64, present, partial bool) string {
	if partial {
		if !present {
			return "unknown"
		}
		return partialValue(formatTokens(value), true)
	}
	if !present {
		return "-"
	}
	return formatTokens(value)
}

// tokenCount renders one tier's count: the compact form when the tier recorded
// tokens, "-" when it recorded none.
func tokenCount(m *modelUsage, t tier) string {
	n := m.tokens.tier(t)
	return partialTokenCell(n, n > 0, m.partial)
}

// totalInputCell renders the model's total prompt tokens (uncached input, cache
// reads and cache writes): the base every cache share is taken from. "-" when the
// model recorded no prompt tokens.
func totalInputCell(m *modelUsage) string {
	context := m.tokens.context()
	return partialTokenCell(context, context != 0, m.partial)
}

// missCount renders the miss subtotal: "-" when the model recorded no prompt
// tokens, else the compact count. A zero beside a nonzero hit is an honest sign
// that the build left the recorded window (see hitRateCell).
func missCount(m *modelUsage) string {
	context := m.tokens.context()
	return partialTokenCell(m.tokens.miss(), context != 0, m.partial)
}

// hitRateCell renders one model's cache-hit share: the percent, "unknown" when
// hits have no recorded build (the ratio is undefined and would read a false
// 100%), and "-" when the model recorded no prompt tokens at all.
func hitRateCell(m *modelUsage) string {
	rate, ok := m.tokens.hitRate()
	if ok {
		return partialValue(formatPercent(rate), m.partial)
	}
	if m.partial {
		return "unknown"
	}
	if m.tokens.context() == 0 {
		return "-"
	}
	return "unknown"
}

// integrationStates is the stable JSON contract: every capability id is always
// present, so adding an agent adds a key without changing the shape consumers
// switch on.
func integrationStates(w worktree, active []capability) map[string]string {
	m := make(map[string]string, len(capabilities()))
	for _, c := range capabilities() {
		m[c.id] = wideText(w.Caps[c.id], applicable(active, c.id))
	}
	return m
}
