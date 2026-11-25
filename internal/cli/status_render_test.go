package cli

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"wtm/internal/core"
	"wtm/internal/ui"
)

// status_render_test.go — the status renderers' color contracts: they live here
// because the renderers do. The plain goldens pin the bytes; these pin that
// styling only reinforces them.

func statusHeaders() []string { return core.ColumnHeaders(core.StatusColumns()) }

// TestStatusCellStyle pins the style contract per column header. Capability
// columns use their upper-cased registration id as the header, so the lookup
// keys off the same registry the --wide view renders.
func TestStatusCellStyle(t *testing.T) {
	cases := []struct {
		header, value string
		bold, faint   bool
		fg            color.Color
	}{
		{"STATUS", "ACTIVE", true, false, lipgloss.Green},
		{"STATUS", "UNUSED", false, false, lipgloss.Yellow},
		{"CHUNKHOUND", core.StateWordYes, false, false, lipgloss.Green},
		{"CHUNKHOUND", core.StateWordPartial, false, false, lipgloss.Yellow},
		{"MCP", core.StateWordAbsent, false, true, nil},
		{"MCP", core.StateWordNA, false, true, nil},
		{core.IntegrationsHeader, "ch mc", false, false, nil},
		{"UPSTREAM", "origin/feat", false, false, lipgloss.Green},
		{"UPSTREAM", "-", false, true, nil},
		{"BRANCH", "(detached)", false, true, nil},
		{"BRANCH", "main", false, false, nil},
		{"LAST USED", "never", false, true, nil},
		{"PATH", "/x", false, false, nil},
	}
	for _, tc := range cases {
		s := statusCellStyle(tc.header, tc.value)
		if s.GetBold() != tc.bold || s.GetFaint() != tc.faint {
			t.Errorf("statusCellStyle(%q, %q) bold=%v faint=%v, want %v/%v",
				tc.header, tc.value, s.GetBold(), s.GetFaint(), tc.bold, tc.faint)
		}
		if tc.fg != nil && s.GetForeground() != tc.fg {
			t.Errorf("statusCellStyle(%q, %q) foreground = %v, want %v", tc.header, tc.value, s.GetForeground(), tc.fg)
		}
	}
}

// TestGroupedCellColors pins the grouped cell's semantic palette per token:
// present green, partial yellow, absent faint. The plain goldens pin the
// tokens; this pins the color branch that no golden exercises.
func TestGroupedCellColors(t *testing.T) {
	cases := []struct{ name, value, escape string }{
		{"present", "chunk mcp", "\x1b[32m"},
		{"partial", "chunk~", "\x1b[33m"},
		{"absent", "-", "\x1b[2m"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := statusCell(core.IntegrationsHeader, tc.value)
			if !strings.Contains(got, tc.escape) {
				t.Fatalf("statusCell(%q) = %q, want escape %q", tc.value, got, tc.escape)
			}
		})
	}
}

// TestStyledCellsStripToPlain pins the color contract: styling is pure
// reinforcement, so stripping ANSI must reproduce the plain cell exactly.
func TestStyledCellsStripToPlain(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{
		{"/SBX/repo", "main", "origin/main", "2020-01-01", "ACTIVE", "-", "ch mc"},
		{"/SBX/wt", "(detached)", "-", "never", "UNUSED", "expired", "-"},
	}
	_, styled := ui.RenderCells(headers, rows, statusCell)
	for r := range rows {
		for c := range rows[r] {
			if got := ansi.Strip(styled[r][c]); got != rows[r][c] {
				t.Errorf("strip(%q) = %q, want %q", styled[r][c], got, rows[r][c])
			}
		}
	}
	for r, want := range []string{"ACTIVE", "UNUSED"} {
		if !strings.Contains(styled[r][4], "\x1b[") {
			t.Errorf("STATUS %q cell carries no ANSI: %q", want, styled[r][4])
		}
	}
	if !strings.Contains(styled[0][6], "\x1b[") {
		t.Errorf("INTEGRATIONS cell carries no ANSI: %q", styled[0][6])
	}
}

// TestStyledTableStripsToPlain is the end-to-end color contract: the styled
// table must strip back to the exact plain bytes, including alignment.
func TestStyledTableStripsToPlain(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{
		{"/SBX/repo", "main", "origin/main", "2020-01-01", "ACTIVE", "-", "ch mc"},
		{"/SBX/wt", "(detached)", "-", "never", "UNUSED", "expired", "-"},
	}
	widths := ui.NaturalWidths(headers, rows)
	aligns := ui.ColumnAligns(headers, core.ColumnRightAligns(core.StatusColumns()))
	plain := ui.RenderTable(headers, rows, widths, aligns)
	styledHeaders, styledRows := ui.RenderCells(headers, rows, statusCell)
	if got := ansi.Strip(ui.RenderTable(styledHeaders, styledRows, widths, aligns)); got != plain {
		t.Errorf("stripped styled table differs from plain:\n got: %q\nwant: %q", got, plain)
	}
}
