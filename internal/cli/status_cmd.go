package cli

import (
	"strings"
	"time"

	"charm.land/lipgloss/v2"

	"wtm/internal/core"
	"wtm/internal/ui"
)

// status_cmd.go — the status command handler and the status table's rendering:
// the domain stylings and the grouped-cell renderer. core owns the schema and
// the JSON contract; presentation lives here so core stays style-free.

// cmdStatus is the status command handler. hooks carry the harness-provided
// session/MCP discovery.
func cmdStatus(g *globals, args []string, hooks core.Hooks) error {
	flags := subFlags("status", g)
	wide := flags.Bool("wide", false, "show one column per integration")
	pos, err := parseSub(flags, g, args, usage)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("status: unexpected argument %q", pos[0])
	}
	// --wide shapes only the table; the JSON contract always reports every
	// integration, so the combination is a usage error, not a silent no-op.
	if *wide && g.JSON {
		return usagef("status: --wide has no effect with --json (JSON already reports all integrations)")
	}
	ws, err := core.LoadWorkspace(g.opts(), false, hooks)
	if err != nil {
		return err
	}
	now := time.Now()
	if g.JSON {
		return core.PrintStatusJSON(ws.Wts, now, ws.TTL)
	}
	printStatusTable(core.StatusColumnsFor(*wide), ws.Wts, now, ws.TTL, g.Color)
	return nil
}

// printStatusTable renders the table for a column schema with the shared grid
// engine and the status cell renderer. Styling keys off the rendered header
// (and the capability registry for --wide columns), never off column ids.
func printStatusTable(cols []core.WorktreeColumn, wts []core.Worktree, now time.Time, ttl time.Duration, colorMode string) {
	ctx := core.ColumnContext{Now: now, TTL: ttl}
	headers := core.ColumnHeaders(cols)
	ui.PrintTable(headers, core.GridRows(cols, wts, ctx), colorMode, statusCell, core.ColumnRightAligns(cols), core.NewGridPolicy(headers))
}

// ---------- domain stylings ----------

// booleanStyle maps a boolean cell to its style.
func booleanStyle(active bool) lipgloss.Style {
	if active {
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	}
	return lipgloss.NewStyle().Faint(true)
}

// statusCellStyle maps a status-table cell to its style, keyed off the column's
// header. Basic ANSI colors are deliberate: the terminal theme owns the
// palette, and every profile (16/256/truecolor/NO_COLOR) degrades cleanly.
// UNUSED is Yellow, not Red: it is the gc candidate, not an error.
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
	// Capability columns only appear in the opt-in --wide view; the header
	// lookup keys off the registry so a new integration colors automatically.
	if core.IsCapabilityHeader(header) {
		return capabilityCellStyle(value)
	}
	return lipgloss.NewStyle()
}

// capabilityCellStyle maps a --wide capability cell. The state word is the
// state, so it maps straight to the one semantic color scheme instead of
// through a value-string wrapper.
func capabilityCellStyle(value string) lipgloss.Style {
	switch value {
	case core.StateWordYes:
		return capStateStyle(core.CapPresent)
	case core.StateWordPartial:
		return capStateStyle(core.CapPartial)
	default: // StateWordAbsent and StateWordNA
		return capStateStyle(core.CapAbsent)
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
func capStateStyle(s core.CapState) lipgloss.Style {
	switch s {
	case core.CapPresent:
		return lipgloss.NewStyle().Foreground(lipgloss.Green)
	case core.CapPartial:
		return lipgloss.NewStyle().Foreground(lipgloss.Yellow)
	default:
		return lipgloss.NewStyle().Faint(true)
	}
}

// statusCell renders one plain status cell with its color. The grouped
// INTEGRATIONS cell colors token-by-token; every other column takes one style.
func statusCell(header, value string) string {
	if header == core.IntegrationsHeader {
		return groupedCellStyle(value)
	}
	return statusCellStyle(header, value).Render(value)
}

// groupedCellStyle colors a grouped INTEGRATIONS cell token by token: present
// green, partial yellow, absent faint. Shape (via the shared markers) carries
// the state; color only reinforces it.
func groupedCellStyle(value string) string {
	if value == "-" {
		return capStateStyle(core.CapAbsent).Render(value)
	}
	tokens := strings.Fields(value)
	for i, tok := range tokens {
		// WHY: the ~ suffix GroupedToken writes is the state, so color keys
		// off it directly instead of through a token-string wrapper.
		state := core.CapPresent
		if strings.HasSuffix(tok, core.PartialMarker) {
			state = core.CapPartial
		}
		tokens[i] = capStateStyle(state).Render(tok)
	}
	return strings.Join(tokens, " ")
}
