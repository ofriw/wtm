package core

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"wtm/internal/temp"
	"wtm/internal/ui"
)

// columns.go — the status table schema: one column registry shared by the
// status table and the gc picker. The generic layout engine lives in ui and
// the cell stylings live in cli, so this package owns the columns only.

// ColumnContext carries the volatile inputs a cell may need, so cell funcs
// stay pure and the registry stays the single source of truth. Active is the
// project's applicable capabilities, so the grouped cell and the wide columns
// agree on what to show and what to mark n/a. GridRows fills it, so no caller
// can build a grid whose cells disagree about applicability.
type ColumnContext struct {
	Now    time.Time
	TTL    time.Duration
	Active []Capability
}

// WorktreeColumn is one grid column: its header, its projection from a
// worktree, and its fit policy. ShrinkRank is the elision order (0 = pinned);
// Floor is the smallest width a shrinkable column may be elided to. This
// registry is the single source of truth for the status table and the gc
// picker, so the two can never drift.
type WorktreeColumn struct {
	Header     string
	Cell       func(Worktree, ColumnContext) string
	RightAlign bool
	ShrinkRank int
	Floor      int
}

// cell funcs wrap the display formatters so the registry stays declarative.
func cellPath(w Worktree, _ ColumnContext) string     { return DisplayPath(w.Path) }
func cellBranch(w Worktree, _ ColumnContext) string   { return w.BranchName() }
func cellUpstream(w Worktree, _ ColumnContext) string { return upstreamCell(w) }
func cellLastUsed(w Worktree, _ ColumnContext) string { return lastUsedCell(w) }
func cellStatus(w Worktree, c ColumnContext) string   { return w.Status(c.Now, c.TTL) }

// cellTemp shows the remaining idle window, or a dash for permanent checkouts.
func cellTemp(w Worktree, c ColumnContext) string {
	if !w.Temp {
		return "-"
	}
	return temp.RemainingWindowLabel(w.TempExpiresAt(), c.Now)
}

// cellIntegrations lists the integrations the worktree actually has: the token
// per present capability, token~ per partial. Absent capabilities are omitted,
// so the cell claims only what is there; "-" when there is nothing to claim.
func cellIntegrations(w Worktree, c ColumnContext) string {
	var tokens []string
	for _, capItem := range c.Active {
		if s := w.Caps[capItem.ID]; s != CapAbsent {
			tokens = append(tokens, capItem.GroupedToken(s))
		}
	}
	if len(tokens) == 0 {
		return "-"
	}
	return strings.Join(tokens, " ")
}

func cellUncommitted(w Worktree, _ ColumnContext) string { return dirtyCell(w) }

// IntegrationsHeader is the INTEGRATIONS column header, shared by core's
// schema and cli's grouped-cell renderer so they cannot disagree about which
// column carries the grouped capability tokens.
const IntegrationsHeader = "INTEGRATIONS"

var (
	// Core shrink ranks start at 2: rank 1 belongs to the --wide capability
	// columns, which elide before any core column gives up width.
	colPath         = WorktreeColumn{Header: "PATH", Cell: cellPath, ShrinkRank: 2, Floor: 12}
	colBranch       = WorktreeColumn{Header: "BRANCH", Cell: cellBranch, ShrinkRank: 4, Floor: 8}
	colUpstream     = WorktreeColumn{Header: "UPSTREAM", Cell: cellUpstream, ShrinkRank: 3, Floor: 8}
	colLastUsed     = WorktreeColumn{Header: "LAST USED", Cell: cellLastUsed, RightAlign: true, ShrinkRank: 5, Floor: 10}
	colStatus       = WorktreeColumn{Header: "STATUS", Cell: cellStatus}
	colTemp         = WorktreeColumn{Header: "TEMP", Cell: cellTemp}
	colIntegrations = WorktreeColumn{Header: IntegrationsHeader, Cell: cellIntegrations, ShrinkRank: 6, Floor: 6}
	colUncommitted  = WorktreeColumn{Header: "UNCOMMITTED", Cell: cellUncommitted}
)

// CoreColumns is the shared prefix of every status schema: identity, tracking
// and activity. It returns a fresh copy per call: the global schema is
// immutable, so callers may mutate or retain the returned slice freely.
// StatusColumns appends the grouped INTEGRATIONS cell; --wide replaces it with
// one column per capability, so a new core column reaches both.
func CoreColumns() []WorktreeColumn {
	return []WorktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colStatus, colTemp}
}

// StatusColumns is the default status schema; tests and the right-align choice
// derive from it so the header contract has one source. Fresh copy per call.
func StatusColumns() []WorktreeColumn {
	return append(CoreColumns(), colIntegrations)
}

// AllColumns is every status column plus the gc picker's UNCOMMITTED. The fit
// policy is derived from ColumnRegistry(), which adds the capability columns.
func AllColumns() []WorktreeColumn {
	return append(StatusColumns(), colUncommitted)
}

// PickerColumns is the gc picker's projection of the shared status grid:
// STATUS is dropped because every candidate is UNUSED (said once in the title),
// and UNCOMMITTED replaces the integration columns. TEMP shows the idle window
// that makes a temporary checkout reclaimable. It derives from the same private
// column registry as status, so the two grids cannot drift.
func PickerColumns() []WorktreeColumn {
	return []WorktreeColumn{colPath, colBranch, colLastUsed, colTemp, colUncommitted, colUpstream}
}

// ColumnRegistry maps header names to their column definition. It includes
// AllColumns plus every capability column, so the shrink logic can look up
// any column's rank and floor by header name. A function (not a package var)
// so the registry has no hidden ordering dependency on package initialization
// and always reflects the currently registered capabilities.
func ColumnRegistry() map[string]WorktreeColumn { return ColumnRegistryFor(Capabilities()) }

// columnRegistryFor builds the registry for the given capabilities and panics
// when a capability header would overwrite a core column. The capability list
// is a parameter so the collision guard is testable with a synthetic entry.
// Registration happens at the composition root (cli.Run → harness.RegisterCapabilities),
// not in any init; the same assertNoCollision guard runs both here and at
// registration, so a bad header fails at startup rather than at first render.
func ColumnRegistryFor(caps []Capability) map[string]WorktreeColumn {
	cols := AllColumns()
	r := make(map[string]WorktreeColumn, len(cols)+len(caps))
	for _, c := range cols {
		r[c.Header] = c
	}
	for _, c := range caps {
		col := c.Column()
		assertNoCollision(c, col, cols)
		r[col.Header] = col
	}
	return r
}

// assertNoCollision panics when a capability header would overwrite a core
// column. cols is the already-built core registry, so the guard runs once per
// column instead of rebuilding the schema per capability.
func assertNoCollision(c Capability, col WorktreeColumn, cols []WorktreeColumn) {
	for _, core := range cols {
		if core.Header == col.Header {
			panic(fmt.Sprintf("capability %q collides with core column %q", c.ID, core.Header))
		}
	}
}

// ColumnHeaders returns the header of each column in order.
func ColumnHeaders(cols []WorktreeColumn) []string {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.Header
	}
	return headers
}

// ColumnRightAligns returns the headers that right-align.
func ColumnRightAligns(cols []WorktreeColumn) []string {
	var right []string
	for _, c := range cols {
		if c.RightAlign {
			right = append(right, c.Header)
		}
	}
	return right
}

// GridRows projects worktrees through a column selection; it is the only place
// a table's cells are produced, so status and the gc picker share one schema.
func GridRows(cols []WorktreeColumn, wts []Worktree, ctx ColumnContext) [][]string {
	// WHY here, not in the caller: a caller that forgot applicability would
	// render every INTEGRATIONS cell as "-" with no error.
	ctx.Active = ActiveCapabilities(wts)
	rows := make([][]string, 0, len(wts))
	for _, w := range wts {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = c.Cell(w, ctx)
		}
		rows = append(rows, row)
	}
	return rows
}

// ShrinkPolicy computes the shrink policy for the given headers from the
// column registry: elision priority and floors. ui's generic sizing needs no
// knowledge of the registry; the capability-header identifier that discloses
// dropped --wide columns is core.IsCapabilityHeader.
func ShrinkPolicy(headers []string) ([]string, map[string]int) {
	reg := ColumnRegistry()
	return dynamicShrinkPriority(headers, reg), dynamicShrinkFloor(headers, reg)
}

// statusColumnsFor picks the schema: the compact grouped default, or the
// --wide view that expands every registered capability into its own column.
// Both share CoreColumns and the wide tail comes from the capability registry,
// so no core list is hand-maintained twice. Column order in --wide follows
// Capabilities(): project integrations first (chunkhound, mcp), then agents in
// registration order (pi, claude). Reordering registration changes the order.
func StatusColumnsFor(wide bool) []WorktreeColumn {
	if !wide {
		return StatusColumns()
	}
	cols := CoreColumns()
	for _, c := range requireCapabilities() {
		cols = append(cols, c.Column())
	}
	return cols
}

// JSONWorktree is the machine-readable status row shape; the struct tags are
// the output contract consumed by scripts and goldens.
type JSONWorktree struct {
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

// PrintStatusJSON is the machine-readable status contract.
func PrintStatusJSON(wts []Worktree, now time.Time, ttl time.Duration) error {
	active := ActiveCapabilities(wts)
	out := make([]JSONWorktree, 0, len(wts))
	for _, w := range wts {
		j := JSONWorktree{
			// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
			Path: w.Path, Branch: w.BranchName(), Upstream: w.Upstream.Short, Main: w.Main, Status: w.Status(now, ttl),
			Temp: w.Temp, Integrations: IntegrationStates(w, active),
		}
		if !w.LastUsed.IsZero() {
			s := w.LastUsed.UTC().Format(time.RFC3339)
			j.LastUsed = &s
		}
		if w.Temp {
			s := temp.FormatDeadline(w.TempExpiresAt())
			j.ExpiresAt = &s
		}
		out = append(out, j)
	}
	return ui.PrintJSON(out)
}

// integrationStates is the stable JSON contract: every capability id is always
// present, so adding an agent adds a key without changing the shape consumers
// switch on.
func IntegrationStates(w Worktree, active []Capability) map[string]string {
	caps := requireCapabilities()
	m := make(map[string]string, len(caps))
	for _, c := range caps {
		m[c.ID] = c.wideText(w.Caps[c.ID], Applicable(active, c.ID))
	}
	return m
}

// DisplayPath shortens the home directory to ~ so the PATH column budgets for
// the path a human recognizes, not for an invariant absolute prefix.
func DisplayPath(p string) string {
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

func lastUsedCell(w Worktree) string {
	if w.LastUsed.IsZero() {
		return "never"
	}
	return w.LastUsed.Format("2006-01-02")
}

// upstreamCell is the status display for a branch's upstream; "-" when the
// branch has none, so whether gc will delete a remote branch is obvious.
func upstreamCell(w Worktree) string {
	if !w.Upstream.Present() {
		return "-"
	}
	return w.Upstream.Short
}

// dirtyCell is the gc picker's uncommitted-change signal. "-" keeps the column
// scannable and mirrors the inventory columns' neutral value.
func dirtyCell(w Worktree) string {
	if w.Dirty {
		return "dirty"
	}
	return "-"
}

// ---------- shrink helpers (from ui; they need the column registry) ----------

// dynamicShrinkPriority returns the headers that can shrink, sorted by rank,
// drawn from registry so --wide capability columns participate.
func dynamicShrinkPriority(headers []string, registry map[string]WorktreeColumn) []string {
	type ranked struct {
		header string
		rank   int
	}
	var shrinkable []ranked
	for _, h := range headers {
		if c, ok := registry[h]; ok && c.ShrinkRank > 0 {
			shrinkable = append(shrinkable, ranked{h, c.ShrinkRank})
		}
	}
	sort.SliceStable(shrinkable, func(i, j int) bool { return shrinkable[i].rank < shrinkable[j].rank })
	out := make([]string, len(shrinkable))
	for i, r := range shrinkable {
		out[i] = r.header
	}
	return out
}

// dynamicShrinkFloor returns the floor for each shrinkable header, drawn from
// registry so --wide capability columns participate.
func dynamicShrinkFloor(headers []string, registry map[string]WorktreeColumn) map[string]int {
	floors := make(map[string]int)
	for _, h := range headers {
		if c, ok := registry[h]; ok && c.ShrinkRank > 0 {
			floors[h] = c.Floor
		}
	}
	return floors
}
