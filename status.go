package main

import (
	"encoding/json"
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
	// Caps maps each capability id to its state; the capability registry is the
	// SSOT for ids, so the row no longer grows a field per integration.
	Caps map[string]capState
	// Created is the worktree creation mark: the .git entry mtime. Zero when the
	// mark is absent, which renders as an unknown age.
	Created time.Time
	// Usage is the cost/active-time summary; nil when the worktree has no agent
	// events. Only status fills it; gc and add leave it nil.
	Usage *worktreeUsage
}

func (w worktree) unused(now time.Time, ttl time.Duration) bool {
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
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
// failClosed is the session-index policy: reclamation must never act on an
// index that skipped an unreadable transcript, so it passes true.
func loadWorkspace(g *globals, showProgress, failClosed bool) (workspace, error) {
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
		ws.wts, ws.idx, derr = discover(root, p, failClosed)
		return derr
	})
	ws.root, ws.ttl = root, ttl
	return ws, err
}

func buildWorktree(g gworktree, idx sessionIndex, ups map[string]upstream, p *progress) worktree {
	pth := canonical(g.Path)
	configs := mcpConfigs(pth, p)
	return worktree{
		Path:   pth,
		Branch: g.Branch,
		// Detached branches have no name, so ups[""] is the zero value.
		Upstream: ups[g.Branch],
		Main:     g.Main,
		// Dirty is no longer pre-computed: status never shows it and the gc
		// picker is the only consumer (set lazily in gcSelection's candidate loop).
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

// discover inventories every worktree of the repo rooted at root, reporting
// each slow step to p (a nil no-op when progress is off). It also returns the
// session index it built, so callers never rebuild it.
func discover(root string, p *progress, failClosed bool) ([]worktree, sessionIndex, error) {
	raw, idx, ups, err := discoverInputs(root, p, failClosed)
	if err != nil {
		return nil, nil, err
	}
	wts := scanWorktrees(raw, idx, ups, p)
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

// discoverInputs walks the three O(repo) sources discovery needs: the worktree
// list, one session index for the whole inventory, and one upstream map for
// every branch. failClosed is that index's unreadable-session policy (see
// sessionErrPolicy): reclamation passes true, display passes false.
func discoverInputs(root string, p *progress, failClosed bool) ([]gworktree, sessionIndex, map[string]upstream, error) {
	p.phase("reading worktrees", 0)
	raw, err := worktrees(root)
	if err != nil {
		return nil, nil, nil, err
	}
	p.phase("indexing sessions", 0)
	idx, err := indexSessions(agents(), sessionErrPolicy{failClosed: failClosed, p: p})
	if err != nil {
		return nil, nil, nil, err
	}
	return raw, idx, discoverUpstreams(root, p), nil
}

// scanWorktrees builds the inventory and reports per-worktree progress; the
// loop is split out so discover stays a readable sequence of phases.
func scanWorktrees(raw []gworktree, idx sessionIndex, ups map[string]upstream, p *progress) []worktree {
	p.phase("scanning worktrees", len(raw))
	wts := make([]worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx, ups, p))
		p.advance(1)
		p.detail(displayPath(g.Path))
	}
	return wts
}

func updateNewest(target *time.Time, path string) {
	if info, err := os.Stat(path); err == nil && info.ModTime().After(*target) {
		*target = info.ModTime()
	}
}

func scanGitModTimes(path string, newest *time.Time) {
	if out, err := git(path, "log", "-1", "--format=%ct"); err == nil {
		if s, err := strconv.ParseInt(out, 10, 64); err == nil {
			if commit := time.Unix(s, 0); commit.After(*newest) {
				*newest = commit
			}
		}
	}
	if out, err := git(path, "ls-files", "-m", "-o", "--exclude-standard", "-z"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if rel != "" && !strings.HasPrefix(rel, ".chunkhound") {
				updateNewest(newest, filepath.Join(path, rel))
			}
		}
	}
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first agent session.
// nowFunc() is the WTM_NOW test seam: in the container harness it is pinned to a
// fixed time so goldens stay byte-stable. Future mtimes are clamped to that same
// now so a session newer than the test clock cannot break determinism.
func worktreeLastUsed(path string, idx sessionIndex) time.Time {
	now := nowFunc()
	newest := lastUsed(path, idx)
	updateNewest(&newest, filepath.Join(path, ".git"))
	scanGitModTimes(path, &newest)
	if newest.After(now) {
		return now
	}
	return newest
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
	cols := statusColumnsFor(wide)
	printStatusTable(cols, ws.wts, now, ws.ttl, g.color)
	printEstimateLegend(estimateLegendNeeded(cols, ws.wts, current, now, ws.ttl), g.color)
	printStatusDetail(ws.wts, current, now, g.color)
	return nil
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
	var cat catalog
	if ws.idx.hasHarness(ws.wts, claudeAgent.name) {
		c, err := loadCatalog()
		if err != nil {
			warnf("claude cost unavailable: %s", err)
			// cat stays nil: every harness will report unknown, which is the honest
			// answer when no catalog is available at all.
		} else {
			cat = c
		}
	}
	// Read the credential proof once: it is a per-run snapshot, not per-worktree.
	subs := loadSubscriptions()
	for i := range ws.wts {
		u, err := buildUsage(ws.wts[i].Path, ws.idx, cat, subs)
		if err != nil {
			warnf("usage for %s: %s", displayPath(ws.wts[i].Path), err)
		}
		if u.hasEvents() || u.partial {
			// A worktree whose every session was unreadable keeps its usage so
			// the loss reads as unknown, never as no usage.
			ws.wts[i].Usage = &u
		}
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

// cellActive is the union of the worktree's session spans; "-" only when the
// worktree has no events. A worktree with point events has a zero span, which
// reads as "0s": measured-but-empty usage must not hide behind "-".
func cellActive(w worktree, _ columnContext) string {
	if w.Usage == nil {
		return "-"
	}
	return humanDuration(w.Usage.active())
}

// cellCost is the sum of every harness cost; "-" when there are no events and
// "unknown" when any contributor is unpriced. A proven subscription renders
// billed/list, so both the actual charge and the list price stay visible.
func cellCost(w worktree, _ columnContext) string {
	if w.Usage == nil {
		return "-"
	}
	return w.Usage.cost().cell()
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
	// Core shrink ranks start at 2: rank 1 belongs to the capability columns.
	// The identity columns give up width first, in PATH/UPSTREAM/BRANCH/LAST USED
	// order, so the cost/time data outlives the columns that name a worktree.
	// AGE, ACTIVE and COST elide next; their floor is their header width, so
	// those three are never rendered narrower than their headers. INTEGRATIONS
	// gives up width last.
	colPath         = worktreeColumn{header: "PATH", cell: cellPath, shrinkRank: 2, floor: 12}
	colBranch       = worktreeColumn{header: "BRANCH", cell: cellBranch, shrinkRank: 4, floor: 8}
	colUpstream     = worktreeColumn{header: "UPSTREAM", cell: cellUpstream, shrinkRank: 3, floor: 8}
	colLastUsed     = worktreeColumn{header: "LAST USED", cell: cellLastUsed, rightAlign: true, shrinkRank: 5, floor: 10}
	colAge          = worktreeColumn{header: "AGE", cell: cellAge, rightAlign: true, shrinkRank: 6, floor: 3}
	colActive       = worktreeColumn{header: "ACTIVE", cell: cellActive, rightAlign: true, shrinkRank: 7, floor: 6}
	colCost         = worktreeColumn{header: "COST", cell: cellCost, rightAlign: true, shrinkRank: 8, floor: 4, unelidable: true}
	colStatus       = worktreeColumn{header: "STATUS", cell: cellStatus}
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
// activity and cost. statusColumns appends the grouped INTEGRATIONS cell;
// --wide replaces it with one column per capability, so a new core column
// reaches both.
var coreColumns = []worktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colAge, colActive, colCost, colStatus}

// statusColumns is the default status schema; tests and the right-align choice
// derive from it so the header contract has one source.
var statusColumns = append(slices.Clone(coreColumns), colIntegrations)

// allColumns is every column a grid can render: the status columns, the gc
// picker's UNCOMMITTED, and the detail block's columns. The fit policy is
// derived from columnRegistry(), which adds the capability columns.
func allColumns() []worktreeColumn {
	cols := append(slices.Clone(statusColumns), colUncommitted)
	return append(cols, detailColumns()...)
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
	headers := columnHeaders(cols)
	rows := alignCostSubFields(headers, gridRows(cols, wts, ctx))
	printTable(headers, rows, colorMode, statusCell, columnRightAligns(cols))
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
	Partial      bool              `json:"partial,omitempty"`
	Integrations map[string]string `json:"integrations"`
}

// jsonHarnessUsage is one harness's machine contract. cost is always present
// (null when unpriced); activeSeconds and models follow the harness's events.
type jsonHarnessUsage struct {
	Cost          *float64                  `json:"cost"`
	CostEstimated bool                      `json:"costEstimated,omitempty"`
	ListCost      *optCost                  `json:"listCost,omitempty"`
	ListCostEst   bool                      `json:"listCostEstimated,omitempty"`
	ActiveSeconds int64                     `json:"activeSeconds"`
	Models        map[string]jsonModelUsage `json:"models"`
}

type jsonModelUsage struct {
	Cost            *float64                 `json:"cost"`
	CostEstimated   bool                     `json:"costEstimated,omitempty"`
	ListCost        *optCost                 `json:"listCost,omitempty"`
	ListCostEst     bool                     `json:"listCostEstimated,omitempty"`
	ActiveSeconds   int64                    `json:"activeSeconds"`
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

// optCost is the optional list price beside a machine cost. A nil pointer omits
// the key entirely (no proven subscription); a pointer is always present, and
// its nil value renders null (a subscription is proven but the list is unknown).
type optCost struct{ v *float64 }

func (o optCost) MarshalJSON() ([]byte, error) {
	if o.v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.v)
}

// listCostJSON adds the list price only when the pair shows one, so unproven
// output and a proven equal price stay exactly as before. A proven total with
// neither side priced omits the key: null beside "unknown" adds no signal.
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
	cost := w.cost()
	j := jsonWorktree{
		// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
		Path: w.Path, Branch: w.branchName(), Upstream: w.Upstream.Short, Main: w.Main,
		Current: w.Path == current, Status: w.status(now, ttl),
		Integrations:  integrationStates(w, active),
		Cost:          cost.billed.jsonValue(),
		CostEstimated: cost.billed.isEstimated(),
	}
	// listCost is emitted only when a proven subscription makes it differ; its
	// estimate flag then qualifies that same value.
	if lc := listCostJSON(cost); lc != nil {
		j.ListCost = lc
		j.ListCostEst = cost.list.isEstimated()
	}
	j.LastUsed = rfc3339Ptr(w.LastUsed)
	j.AgeSeconds = ageSeconds(w, now)
	applyUsageJSON(&j, w.Usage)
	return j
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

// applyUsageJSON adds the event-derived fields; an eventless worktree omits them.
func applyUsageJSON(j *jsonWorktree, u *worktreeUsage) {
	if u == nil {
		return
	}
	a := seconds(u.active())
	j.ActiveSeconds = &a
	j.Usage = usageJSON(u)
	j.Partial = u.partial
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
		models := make(map[string]jsonModelUsage, len(h.models))
		for model, m := range h.models {
			mu := jsonModelUsage{Cost: m.cost.billed.jsonValue(), CostEstimated: m.cost.billed.isEstimated(), ActiveSeconds: seconds(m.active()), Tiers: tiersJSON(m.tokens, m.tiers)}
			mu.CacheMissTokens, mu.CacheHitRate = cacheJSON(m.tokens)
			if lc := listCostJSON(m.cost); lc != nil {
				mu.ListCost = lc
				mu.ListCostEst = m.cost.list.isEstimated()
			}
			models[model] = mu
		}
		hu := jsonHarnessUsage{Cost: h.cost.billed.jsonValue(), CostEstimated: h.cost.billed.isEstimated(), ActiveSeconds: seconds(h.active()), Models: models}
		if lc := listCostJSON(h.cost); lc != nil {
			hu.ListCost = lc
			hu.ListCostEst = h.cost.list.isEstimated()
		}
		out[name] = hu
	}
	return out
}

// tiersJSON projects a model's token tiers into the machine contract. It returns
// nil when no tier recorded tokens, so a cost-only record (a spawn child, an
// older Pi session) omits the breakdown instead of reporting four empty tiers.
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

// estimateLegendNeeded reports which legend reasons the table and the detail
// block will render. It reuses the same width fit as the renderers, so a
// dropped COST column cannot leave a legend with no mark to explain.
func estimateLegendNeeded(cols []worktreeColumn, wts []worktree, current string, now time.Time, ttl time.Duration) estimateReasons {
	var r estimateReasons
	ctx := columnContext{now: now, ttl: ttl}
	if costColumnKept(cols, gridRows(cols, wts, ctx)) {
		for _, w := range wts {
			r = r.merge(w.cost().reasons())
		}
	}
	cur := findWorktree(wts, current)
	if cur == nil {
		return r
	}
	if !summaryCostElided(detailSummaryRows(*cur, now)) {
		r = r.merge(cur.cost().reasons())
	}
	if cur.Usage != nil && costColumnKept(detailColumns(), detailRows(cur.Usage)) {
		r = r.merge(modelsEstimated(cur.Usage))
	}
	return r
}

// costColumnKept reports whether the COST column survives the width fit, so a
// caller can know whether a cost cell will actually render.
func costColumnKept(cols []worktreeColumn, rows [][]string) bool {
	return columnKept(cols, alignCostSubFields(columnHeaders(cols), rows), colCost.header)
}

// alignCostSubFields pads the COST column's billed/list pair into two aligned
// sub-fields, so every worktree or model row shows its billed value (and its
// list value, when a proven subscription adds one) in the same columns. COST
// stays one grid column with one header: the pair is aligned inside the cell,
// so the whole-or-drop rule (colCost.unelidable) still holds and the column can
// never be middle-cut. A row without a list keeps the billed position and
// leaves the list region blank. No row with a list means nothing to align, so a
// table of single prices renders exactly as before. Idempotent: already-padded
// cells measure the same, because spaces carry no sub-field value.
func alignCostSubFields(headers []string, rows [][]string) [][]string {
	i := slices.Index(headers, colCost.header)
	if i < 0 {
		return rows
	}
	// Pass 1: measure. "/" is only ever the pair separator: formatCost emits
	// only "$", digits, "." and "~", so Cut splits safely.
	maxBilled, maxList, hasList := 0, 0, false
	for _, row := range rows {
		billed, list, ok := strings.Cut(row[i], "/")
		maxBilled = max(maxBilled, dispWidth(strings.TrimSpace(billed)))
		if ok {
			hasList = true
			maxList = max(maxList, dispWidth(strings.TrimSpace(list)))
		}
	}
	if !hasList {
		return rows
	}
	// Pass 2: right-align each sub-field, so billed values and the separator
	// line up across rows.
	out := make([][]string, len(rows))
	for r, row := range rows {
		billed, list, ok := strings.Cut(row[i], "/")
		cell := padLeft(strings.TrimSpace(billed), maxBilled)
		if ok {
			cell += "/" + padLeft(strings.TrimSpace(list), maxList)
		} else {
			cell += strings.Repeat(" ", maxList+1)
		}
		out[r] = slices.Clone(row)
		out[r][i] = cell
	}
	return out
}

// padLeft left-pads s with spaces to w display cells, so a right-aligned field
// lines up in its sub-column.
func padLeft(s string, w int) string { return strings.Repeat(" ", w-dispWidth(s)) + s }

// columnKept reports whether a named column survives the width fit, so a caller
// can know whether that column's cells (and any legend explaining them) render.
func columnKept(cols []worktreeColumn, rows [][]string, header string) bool {
	widths := columnWidths(columnHeaders(cols), rows, widthBudget())
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

// printEstimateLegend discloses, once under the table, each assumed price the
// rendered cells rest on. The reason text is the SSOT constant, so the
// explanation cannot drift from the estimate it explains. A table of reported
// prices prints nothing here, and a table with one kind of estimate prints
// only its line.
func printEstimateLegend(r estimateReasons, colorMode string) {
	if !r.any() {
		return
	}
	fmt.Println()
	var lines []string
	if r.cache {
		lines = append(lines, "~ estimated: "+estimateCacheWriteLifetime)
	}
	if r.overage {
		lines = append(lines, "~ estimated: "+estimateSubscriptionOverage)
	}
	printLegend(lines, colorMode)
}

// detailColumns is the detail block's table schema: one MODEL row per model,
// TIME, COST, then the token group (total input, the token classes, then the
// derived miss/hit% metrics). A function, like columnRegistry, so the headers
// derive from the metadata lists with no package-init ordering dependency. It
// shares the status column registry (see allColumns), so the block obeys the
// same width policy as the table above: the token group gives up width first
// (dropping whole), MODEL next, then TIME, and COST is unelidable, so the cost
// column outlives the columns that name the rows.
func detailColumns() []worktreeColumn {
	cols := []worktreeColumn{colModel, colTime, colCost}
	return append(cols, tokenColumns()...)
}

// printStatusDetail renders the current worktree's usage under the table. The
// block is omitted when no harness in the repo has events; when the current
// worktree alone is empty, the block says so instead of showing empty rows.
func printStatusDetail(wts []worktree, current string, now time.Time, colorMode string) {
	if !repoHasUsage(wts) {
		return
	}
	cur := findWorktree(wts, current)
	if cur == nil {
		return
	}
	fmt.Println()
	printKeyValues(dropElidedCostRow(detailSummaryRows(*cur, now)), detailTitleRows, colorMode)
	if cur.Usage == nil {
		fmt.Println("no agent usage recorded")
		return
	}
	fmt.Println()
	// The breakdown reuses the table's cell grammar, so an unpriced cost
	// recedes here exactly as it does in the status table above.
	rows := alignCostSubFields(columnHeaders(detailColumns()), detailRows(cur.Usage))
	cols, rows := fitTokenGroup(detailColumns(), rows)
	printTable(columnHeaders(cols), rows, colorMode, statusCell, columnRightAligns(cols))
	if tokenGroupKept(cols) && detailHasTokens(cur.Usage) {
		printTokenLegend(colorMode)
	}
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

// fitTokenGroup drops the whole token group when the width fit cannot keep every
// column in it. A partial breakdown — some metrics shown, some hidden — reads as
// arbitrary, so the group stands or falls together and is disclosed once. Their
// cells leave the rows with them, so the grid stays index-aligned.
func fitTokenGroup(cols []worktreeColumn, rows [][]string) ([]worktreeColumn, [][]string) {
	widths := columnWidths(columnHeaders(cols), rows, widthBudget())
	token := tokenHeaderSet()
	drop := false
	for i, c := range cols {
		drop = drop || (token[c.header] && widths[i] == 0)
	}
	if !drop {
		return cols, rows
	}
	warnf("terminal too narrow; hid %s; widen the terminal or use --json", strings.Join(tokenColumnNames(), ", "))
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
// measures. The identity value keeps the full path: identifying the current
// worktree is the block's purpose, and the table above carries the elided form.
func detailSummaryRows(cur worktree, now time.Time) [][]string {
	return [][]string{
		{"current worktree", displayPath(cur.Path)},
		{"age", detailAge(cur, now)},
		{"active time", detailActive(cur)},
		{detailCostLabel, detailCost(cur)},
	}
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

// dropElidedCostRow drops the summary cost row when the fitted value column
// cannot hold it whole. A middle-cut pair ($0….20) reads as a wrong number,
// so the row is hidden with the same stderr disclosure as a dropped table
// column instead of being elided.
func dropElidedCostRow(rows [][]string) [][]string {
	if !summaryCostElided(rows) {
		return rows
	}
	for i, r := range rows {
		if r[0] == detailCostLabel {
			warnf("terminal too narrow; hid cost; widen the terminal or use --json")
			return append(slices.Clone(rows[:i]), rows[i+1:]...)
		}
	}
	return rows
}

// summaryCostElided reports whether the summary's fitted value column cannot
// hold the cost value whole. A middle-cut pair reads as a wrong number, so the
// caller hides the row instead of eliding it.
func summaryCostElided(rows [][]string) bool {
	if len(rows) == 0 || len(rows[0]) == 0 {
		return false
	}
	_, fitted := sizedKeyValues(rows)
	for _, r := range rows {
		if r[0] == detailCostLabel {
			return dispWidth(r[1]) > fitted[1]
		}
	}
	return false
}

func detailAge(w worktree, now time.Time) string { return cellAge(w, columnContext{now: now}) }

func detailActive(w worktree) string { return cellActive(w, columnContext{}) }

func detailCost(w worktree) string { return cellCost(w, columnContext{}) }

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
// Arrows serve the directional classes: they are narrow and render consistently,
// unlike the emoji pair they replace.
var detailTokenMeta = []detailMetric{
	{"↑", "in", totalInputCell},
	{"↓", "out", tierCount(tierOutput)},
	{"🎯", "hit", tierCount(tierCacheRead)},
	{"✍️", "write", tierCount(tierCacheWrite)},
	{"✗", "miss", missCount},
	{"◉", "hit%", hitRateCell},
}

// tokenLegend spells out the token group's arithmetic, one clause per line so
// each definition reads on its own. A legend, not just headers, because "in" is
// a total while "hit" and "miss" are its parts: the header alone cannot say how
// the total is counted. hit% caps at 99.9% (see maxHitRate); the legend keeps
// the short arithmetic and the cap lives here so the row width stays stable.
var tokenLegend = []string{
	"tokens: in = uncached input + cache read + cache write (total input)",
	"miss = uncached input + cache write",
	"hit% = hit / in",
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

// printTokenLegend prints the token legend below the breakdown, one clause per
// line, each wrapped to the grid's width so the text that explains the block
// never widens it. A blank line separates the legend from the data.
// widthBudget()==0 means the terminal is unbounded (piped output), so every
// clause prints whole.
func printTokenLegend(colorMode string) {
	fmt.Println()
	var lines []string
	for _, clause := range tokenLegend {
		lines = append(lines, wrapWords(clause, widthBudget())...)
	}
	printLegend(lines, colorMode)
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
// billed cost. Each row carries the model's time, cost, and one cell per
// token-group column, so the breakdown stays beside the model without harness
// grouping or sub-rows.
func detailRows(u *worktreeUsage) [][]string {
	models := u.mergedModels()
	var rows [][]string
	for _, name := range u.sortedModelNames(models) {
		m := models[name]
		row := []string{name, humanDuration(m.active()), m.cost.cell()}
		for _, d := range detailTokenMeta {
			row = append(row, d.cell(m))
		}
		rows = append(rows, row)
	}
	return rows
}

// tokenCount renders one tier's count: the compact form when the tier recorded
// tokens, "-" when it recorded none.
func tokenCount(m *modelUsage, t tier) string {
	if n := m.tokens.tier(t); n > 0 {
		return formatTokens(n)
	}
	return "-"
}

// totalInputCell renders the model's total prompt tokens (uncached input, cache
// reads and cache writes): the base every cache share is taken from. "-" when the
// model recorded no prompt tokens.
func totalInputCell(m *modelUsage) string {
	if m.tokens.context() == 0 {
		return "-"
	}
	return formatTokens(m.tokens.context())
}

// missCount renders the miss subtotal: "-" when the model recorded no prompt
// tokens, else the compact count. A zero beside a nonzero hit is an honest sign
// that the build left the recorded window (see hitRateCell).
func missCount(m *modelUsage) string {
	if m.tokens.context() == 0 {
		return "-"
	}
	return formatTokens(m.tokens.miss())
}

// hitRateCell renders one model's cache-hit share: the percent, "unknown" when
// hits have no recorded build (the ratio is undefined and would read a false
// 100%), and "-" when the model recorded no prompt tokens at all.
func hitRateCell(m *modelUsage) string {
	rate, ok := m.tokens.hitRate()
	if ok {
		return formatPercent(rate)
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
