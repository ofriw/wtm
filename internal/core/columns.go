package core

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"wtm/internal/fs"
	"wtm/internal/temp"
	"wtm/internal/ui"
)

// columns.go — the status table schema: one column registry shared by the
// status table and the gc picker, plus the domain stylings that color its
// cells. The generic layout engine lives in ui; this package owns the columns
// and their rendering.

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
func cellPath(w Worktree, _ ColumnContext) string     { return displayPath(w.Path) }
func cellBranch(w Worktree, _ ColumnContext) string   { return w.BranchName() }
func cellUpstream(w Worktree, _ ColumnContext) string { return upstreamCell(w) }
func cellLastUsed(w Worktree, _ ColumnContext) string { return lastUsedCell(w) }
func cellStatus(w Worktree, c ColumnContext) string   { return w.Status(c.Now, c.TTL) }

// cellTemp shows the remaining idle window, or a dash for permanent checkouts.
// CellTemp is the TEMP column cell; exported for tests pinning the cell
// contract independent of a table render.
func CellTemp(w Worktree, c ColumnContext) string {
	return cellTemp(w, c)
}

func cellTemp(w Worktree, c ColumnContext) string {
	if !w.Temp {
		return "-"
	}
	return temp.RemainingWindowLabel(w.TempExpiresAt(), c.Now)
}

// CellIntegrations lists the integrations the worktree actually has: the token
// per present capability, token~ per partial. Absent capabilities are omitted,
// so the cell claims only what is there; "-" when there is nothing to claim.
func CellIntegrations(w Worktree, c ColumnContext) string {
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

var (
	// Core shrink ranks start at 2: rank 1 belongs to the --wide capability
	// columns, which elide before any core column gives up width.
	ColPath         = WorktreeColumn{Header: "PATH", Cell: cellPath, ShrinkRank: 2, Floor: 12}
	ColBranch       = WorktreeColumn{Header: "BRANCH", Cell: cellBranch, ShrinkRank: 4, Floor: 8}
	ColUpstream     = WorktreeColumn{Header: "UPSTREAM", Cell: cellUpstream, ShrinkRank: 3, Floor: 8}
	ColLastUsed     = WorktreeColumn{Header: "LAST USED", Cell: cellLastUsed, RightAlign: true, ShrinkRank: 5, Floor: 10}
	ColStatus       = WorktreeColumn{Header: "STATUS", Cell: cellStatus}
	ColTemp         = WorktreeColumn{Header: "TEMP", Cell: cellTemp}
	ColIntegrations = WorktreeColumn{Header: "INTEGRATIONS", Cell: CellIntegrations, ShrinkRank: 6, Floor: 6}
	ColUncommitted  = WorktreeColumn{Header: "UNCOMMITTED", Cell: cellUncommitted}
)

// CoreColumns is the shared prefix of every status schema: identity, tracking
// and activity. StatusColumns appends the grouped INTEGRATIONS cell; --wide
// replaces it with one column per capability, so a new core column reaches both.
var CoreColumns = []WorktreeColumn{ColPath, ColBranch, ColUpstream, ColLastUsed, ColStatus, ColTemp}

// StatusColumns is the default status schema; tests and the right-align choice
// derive from it so the header contract has one source.
var StatusColumns = append(slices.Clone(CoreColumns), ColIntegrations)

// AllColumns is every status column plus the gc picker's UNCOMMITTED. The fit
// policy is derived from ColumnRegistry(), which adds the capability columns.
func AllColumns() []WorktreeColumn {
	return append(slices.Clone(StatusColumns), ColUncommitted)
}

// ColumnRegistry maps header names to their column definition. It includes
// AllColumns plus every capability column, so the shrink logic can look up
// any column's rank and floor by header name. A function (not a package var)
// so the registry has no hidden ordering dependency on package initialization
// and the slice cannot be mutated at runtime.
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
		assertNoCollision(c)
		r[col.Header] = col
	}
	return r
}
func assertNoCollision(c Capability) {
	for _, col := range AllColumns() {
		if col.Header == c.Column().Header {
			panic(fmt.Sprintf("capability %q collides with core column %q", c.ID, col.Header))
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

// ShrinkPolicy computes the shrink priority and floors for the given headers
// from the column registry, so ui's generic sizing needs no knowledge of the
// registry.
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
		return slices.Clone(StatusColumns)
	}
	cols := slices.Clone(CoreColumns)
	for _, c := range requireCapabilities() {
		cols = append(cols, c.Column())
	}
	return cols
}

// PrintStatusTable renders the status table for the given column schema; exported
// for tests that pin rendering independent of a status command run.
func PrintStatusTable(cols []WorktreeColumn, wts []Worktree, now time.Time, ttl time.Duration, colorMode string) {
	printStatusTable(cols, wts, now, ttl, colorMode)
}

func printStatusTable(cols []WorktreeColumn, wts []Worktree, now time.Time, ttl time.Duration, colorMode string) {
	ctx := ColumnContext{Now: now, TTL: ttl}
	headers := ColumnHeaders(cols)
	priority, floors := ShrinkPolicy(headers)
	ui.PrintTable(headers, GridRows(cols, wts, ctx), colorMode, statusCell, ColumnRightAligns(cols), priority, floors, IsCapabilityHeader)
}

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
			s := temp.FormatTempDeadline(w.TempExpiresAt())
			j.ExpiresAt = &s
		}
		out = append(out, j)
	}
	return fs.PrintJSON(out)
}

// integrationStates is the stable JSON contract: every capability id is always
// present, so adding an agent adds a key without changing the shape consumers
// switch on.
func IntegrationStates(w Worktree, active []Capability) map[string]string {
	caps := requireCapabilities()
	m := make(map[string]string, len(caps))
	for _, c := range caps {
		m[c.ID] = c.WideText(w.Caps[c.ID], Applicable(active, c.ID))
	}
	return m
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

// DisplayPath is the exported form used by cli reporting.
func DisplayPath(p string) string { return displayPath(p) }

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

// ---------- domain stylings ----------

// booleanStyle maps a boolean cell to its style.
func booleanStyle(active bool) lipgloss.Style {
	if active {
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	}
	return lipgloss.NewStyle().Faint(true)
}

// statusCellStyle maps a status-table cell to its style. Basic ANSI colors are
// deliberate: the terminal theme owns the palette, and every profile (16/256/
// truecolor/NO_COLOR) degrades cleanly. UNUSED is Yellow, not Red: it is the
// gc candidate, not an error.
func statusCellStyle(header, value string) lipgloss.Style {
	switch header {
	case "STATUS":
		if value == "ACTIVE" {
			return lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Green)
		}
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	case "TEMP":
		return tempCellStyle(value)
	case "UPSTREAM":
		return booleanStyle(value != "-")
	case "BRANCH", "LAST USED":
		if value == "(detached)" || value == "never" {
			return lipgloss.NewStyle().Faint(true)
		}
	}
	// Capability columns only appear in the opt-in --wide view.
	if IsCapabilityHeader(header) {
		return capabilityCellStyle(value)
	}
	return lipgloss.NewStyle()
}

// capabilityCellStyle maps a --wide capability cell. The state word is the
// state, so it maps straight to the one semantic color scheme instead of
// through a value-string wrapper.
func capabilityCellStyle(value string) lipgloss.Style {
	switch value {
	case "yes":
		return capStateStyle(CapPresent)
	case "partial":
		return capStateStyle(CapPartial)
	default: // "-" and "n/a"
		return capStateStyle(CapAbsent)
	}
}

func tempCellStyle(value string) lipgloss.Style {
	if value == "-" {
		return lipgloss.NewStyle().Faint(true)
	}
	if value == "expired" {
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Cyan)
}

// capStateStyle is the one semantic color scheme: present green, partial yellow,
// absent/n-a faint. A word or a shape carries the state; color only reinforces it.
func capStateStyle(s CapState) lipgloss.Style {
	switch s {
	case CapPresent:
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	case CapPartial:
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	default:
		return lipgloss.NewStyle().Faint(true)
	}
}

// statusCell renders one plain status cell with its color. The grouped
// INTEGRATIONS cell colors token-by-token; every other column takes one style.
func statusCell(header, value string) string {
	if header == ColIntegrations.Header {
		return groupedCellStyle(value)
	}
	return statusCellStyle(header, value).Render(value)
}

// groupedCellStyle colors a grouped INTEGRATIONS cell token by token: present
// green, partial yellow, absent faint. Shape (via the shared markers) carries
// the state; color only reinforces it.
func groupedCellStyle(value string) string {
	if value == "-" {
		return capStateStyle(CapAbsent).Render(value)
	}
	tokens := strings.Fields(value)
	for i, tok := range tokens {
		// WHY: the ~ suffix GroupedToken writes is the state, so color keys
		// off it directly instead of through a token-string wrapper.
		state := CapPresent
		if strings.HasSuffix(tok, partialMarker) {
			state = CapPartial
		}
		tokens[i] = capStateStyle(state).Render(tok)
	}
	return strings.Join(tokens, " ")
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
