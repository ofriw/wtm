package main

import (
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
	}
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
	guardPermanentActivity(wts, unknown, time.Now(), p)
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
func guardPermanentActivity(wts []worktree, unknown []unverifiedSession, now time.Time, p *progress) {
	if len(unknown) == 0 {
		return
	}
	for _, u := range unknown {
		warnProgress(p, "unverified session ownership: %s", u.Path)
	}
	for i := range wts {
		if !wts[i].Temp {
			wts[i].LastUsed = now
			warnProgress(p, "%s: keeping permanent worktree ACTIVE because session ownership is unverified", displayPath(wts[i].Path))
		}
	}
}

// readTempStoreLenient reads the temp store for read-only commands. A corrupt
// store only loses temp metadata (the checkouts stay durable), so it must never
// break status: warn and proceed with no temp records.
func readTempStoreLenient(p *progress) tempStore {
	temps, err := readTempStore()
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
func worktreeLastUsed(path string, idx sessionIndex) time.Time {
	newest, _ := readWorktreeActivityLenient(path, idx, time.Now())
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
	fs := subFlags("status", g)
	wide := fs.Bool("wide", false, "show one column per integration")
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("status: unexpected argument %q", pos[0])
	}
	// --wide shapes only the table; the JSON contract always reports every
	// integration, so the combination is a usage error, not a silent no-op.
	if *wide && g.json {
		return usagef("status: --wide has no effect with --json (JSON already reports all integrations)")
	}
	ws, err := loadWorkspace(g, false, false)
	if err != nil {
		return err
	}
	now := time.Now()
	if g.json {
		return printStatusJSON(ws.wts, now, ws.ttl)
	}
	printStatusTable(statusColumnsFor(*wide), ws.wts, now, ws.ttl, g.color)
	return nil
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
}

// cell funcs wrap the display formatters so the registry stays declarative.
func cellPath(w worktree, _ columnContext) string     { return displayPath(w.Path) }
func cellBranch(w worktree, _ columnContext) string   { return w.branchName() }
func cellUpstream(w worktree, _ columnContext) string { return upstreamCell(w) }
func cellLastUsed(w worktree, _ columnContext) string { return lastUsedCell(w) }
func cellStatus(w worktree, c columnContext) string   { return w.status(c.now, c.ttl) }

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
	// Core shrink ranks start at 2: rank 1 belongs to the --wide capability
	// columns, which elide before any core column gives up width.
	colPath         = worktreeColumn{header: "PATH", cell: cellPath, shrinkRank: 2, floor: 12}
	colBranch       = worktreeColumn{header: "BRANCH", cell: cellBranch, shrinkRank: 4, floor: 8}
	colUpstream     = worktreeColumn{header: "UPSTREAM", cell: cellUpstream, shrinkRank: 3, floor: 8}
	colLastUsed     = worktreeColumn{header: "LAST USED", cell: cellLastUsed, rightAlign: true, shrinkRank: 5, floor: 10}
	colStatus       = worktreeColumn{header: "STATUS", cell: cellStatus}
	colTemp         = worktreeColumn{header: "TEMP", cell: cellTemp}
	colIntegrations = worktreeColumn{header: "INTEGRATIONS", cell: cellIntegrations, shrinkRank: 6, floor: 6}
	colUncommitted  = worktreeColumn{header: "UNCOMMITTED", cell: cellUncommitted}
)

// coreColumns is the shared prefix of every status schema: identity, tracking
// and activity. statusColumns appends the grouped INTEGRATIONS cell; --wide
// replaces it with one column per capability, so a new core column reaches both.
var coreColumns = []worktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colStatus, colTemp}

// statusColumns is the default status schema; tests and the right-align choice
// derive from it so the header contract has one source.
var statusColumns = append(slices.Clone(coreColumns), colIntegrations)

// allColumns is every status column plus the gc picker's UNCOMMITTED. The fit
// policy is derived from columnRegistry(), which adds the capability columns.
func allColumns() []worktreeColumn {
	return append(slices.Clone(statusColumns), colUncommitted)
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

func printStatusTable(cols []worktreeColumn, wts []worktree, now time.Time, ttl time.Duration, colorMode string) {
	ctx := columnContext{now: now, ttl: ttl}
	printTable(columnHeaders(cols), gridRows(cols, wts, ctx), colorMode, statusCell, columnRightAligns(cols))
}

type jsonWorktree struct {
	Path         string            `json:"path"`
	Branch       string            `json:"branch"`
	Upstream     string            `json:"upstream"`
	Main         bool              `json:"main"`
	LastUsed     *string           `json:"lastUsed"`
	Status       string            `json:"status"`
	Integrations map[string]string `json:"integrations"`
	Temp         bool              `json:"temp"`
	ExpiresAt    *string           `json:"expiresAt"`
}

func printStatusJSON(wts []worktree, now time.Time, ttl time.Duration) error {
	active := activeCapabilities(wts)
	out := make([]jsonWorktree, 0, len(wts))
	for _, w := range wts {
		j := jsonWorktree{
			// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
			Path: w.Path, Branch: w.branchName(), Upstream: w.Upstream.Short, Main: w.Main, Status: w.status(now, ttl),
			Temp: w.Temp, Integrations: integrationStates(w, active),
		}
		if !w.LastUsed.IsZero() {
			s := w.LastUsed.UTC().Format(time.RFC3339)
			j.LastUsed = &s
		}
		if w.Temp {
			s := formatTempDeadline(w.tempExpiresAt())
			j.ExpiresAt = &s
		}
		out = append(out, j)
	}
	return printJSON(out)
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
