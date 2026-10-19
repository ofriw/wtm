package main

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// status_table.go — the status table: the column registry and cell grammar,
// the width fit, and the legends that explain the cost marks.

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
