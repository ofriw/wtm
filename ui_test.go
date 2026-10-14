package main

import (
	"fmt"
	"image/color"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// ui_test.go — pure width-budget and color contracts for ui.go. No TTY, no clock.

func statusHeaders() []string { return columnHeaders(statusColumns) }

func TestWidthBudget(t *testing.T) {
	cases := []struct {
		name    string
		columns string
		want    int
	}{
		{"COLUMNS wins", "80", 80},
		{"invalid COLUMNS falls back", "abc", 0},
		{"zero COLUMNS falls back", "0", 0},
		{"negative COLUMNS falls back", "-5", 0},
		{"unset COLUMNS falls back", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COLUMNS", tc.columns)
			var got int
			// captureStdout swaps in a pipe, so the TTY fallback must yield 0
			// regardless of the terminal `go test` happens to run in.
			captureStdout(t, func() { got = widthBudget() })
			if got != tc.want {
				t.Errorf("widthBudget() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestMiddleElide(t *testing.T) {
	cases := []struct {
		name string
		in   string
		w    int
		want string
	}{
		{"fits unchanged", "hello", 5, "hello"},
		{"room to spare unchanged", "hello", 10, "hello"},
		{"ascii middle cut", "hello", 4, "h…lo"},
		{"width one", "hello", 1, "…"},
		{"zero width", "hello", 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := middleElide(tc.in, tc.w); got != tc.want {
				t.Errorf("middleElide(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
			}
		})
	}
	// Wide graphemes must never let an elision blow the budget.
	wide := "日本語のとても長いパス🎉"
	for w := 1; w <= 20; w++ {
		if got := dispWidth(middleElide(wide, w)); got > w {
			t.Errorf("middleElide(%q, %d) width = %d, want <= %d", wide, w, got, w)
		}
	}
}

// statusRow builds a status row from header names, so a schema change never
// silently misaligns a fixture.
func statusRow(values map[string]string) []string {
	headers := statusHeaders()
	row := make([]string, len(headers))
	for i, h := range headers {
		row[i] = values[h]
	}
	return row
}

func headerIndex(headers []string) map[string]int {
	m := make(map[string]int, len(headers))
	for i, h := range headers {
		m[h] = i
	}
	return m
}

func TestColumnWidths(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{statusRow(map[string]string{
		"PATH":         "/a/very/long/checkout/path/that/keeps/going/on",
		"BRANCH":       "feature/a-rather-long-branch",
		"UPSTREAM":     "origin/feature/a-rather-long-branch",
		"LAST USED":    "2020-01-01",
		"AGE":          "12d",
		"ACTIVE":       "8h 20m",
		"COST":         "$41.20",
		"STATUS":       "UNUSED",
		"INTEGRATIONS": "ch mc",
	})}
	natural := naturalWidths(headers, rows)
	naturalTotal := lineWidth(natural)
	idx := headerIndex(headers)

	t.Run("budget zero is natural", func(t *testing.T) {
		assertWidths(t, columnWidths(headers, rows, 0), natural)
	})
	t.Run("generous budget is natural", func(t *testing.T) {
		assertWidths(t, columnWidths(headers, rows, naturalTotal+40), natural)
	})
	t.Run("identity columns shrink before the cost columns", func(t *testing.T) {
		budget := naturalTotal - 2
		got := columnWidths(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, headers, got, natural)
		assertNoSlivers(t, headers, got)
		if got[idx["PATH"]] >= natural[idx["PATH"]] {
			t.Errorf("PATH must give up width first: got %d, natural %d", got[idx["PATH"]], natural[idx["PATH"]])
		}
		for _, h := range []string{"AGE", "ACTIVE", "COST"} {
			if got[idx[h]] != natural[idx[h]] {
				t.Errorf("%s must not shrink while PATH has room: got %d, natural %d", h, got[idx[h]], natural[idx[h]])
			}
		}
	})
	t.Run("cost columns keep their header floor", func(t *testing.T) {
		registry := columnRegistry()
		identitySlack := 0
		for _, h := range []string{"PATH", "UPSTREAM", "BRANCH", "LAST USED"} {
			identitySlack += natural[idx[h]] - registry[h].floor
		}
		budget := naturalTotal - identitySlack - 2
		got := columnWidths(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, headers, got, natural)
		assertNoSlivers(t, headers, got)
		for _, h := range []string{"PATH", "UPSTREAM", "BRANCH"} {
			if got[idx[h]] != registry[h].floor {
				t.Errorf("%s = %d, want floor %d", h, got[idx[h]], registry[h].floor)
			}
		}
		for _, h := range []string{"AGE", "ACTIVE", "COST"} {
			if got[idx[h]] < dispWidth(h) {
				t.Errorf("%s = %d, narrower than its header %d", h, got[idx[h]], dispWidth(h))
			}
		}
	})
	t.Run("tight budget drops whole columns, never slivers", func(t *testing.T) {
		got := columnWidths(headers, rows, 45)
		assertWithin(t, got, 45)
		assertPinned(t, headers, got, natural)
		assertNoSlivers(t, headers, got)
	})
}

// TestColumnWidthsNarrowBudgets pins the shrink contract at every narrow
// terminal: identity columns yield first, cost/time headers survive whole
// while kept, lines fit, and no slivers ever render.
func TestColumnWidthsNarrowBudgets(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{statusRow(map[string]string{
		"PATH":         "/a/very/long/checkout/path/that/keeps/going/on",
		"BRANCH":       "feature/a-rather-long-branch",
		"UPSTREAM":     "origin/feature/a-rather-long-branch",
		"LAST USED":    "2020-01-01",
		"AGE":          "12d",
		"ACTIVE":       "8h 20m",
		"COST":         "$41.20",
		"STATUS":       "UNUSED",
		"INTEGRATIONS": "ch mc",
	})}
	idx := headerIndex(headers)
	natural := naturalWidths(headers, rows)
	for _, budget := range []int{80, 60, 40} {
		t.Run(fmt.Sprintf("%d", budget), func(t *testing.T) {
			got := columnWidths(headers, rows, budget)
			assertWithin(t, got, budget)
			assertPinned(t, headers, got, natural)
			assertNoSlivers(t, headers, got)
			for _, h := range []string{"AGE", "ACTIVE", "COST"} {
				if got[idx[h]] > 0 && got[idx[h]] < dispWidth(h) {
					t.Errorf("%s = %d, narrower than its %d-cell header", h, got[idx[h]], dispWidth(h))
				}
			}
		})
	}
}

func TestElideCells(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{statusRow(map[string]string{
		"PATH":         "/a/very/long/checkout/path",
		"BRANCH":       "feature/x",
		"UPSTREAM":     "origin/x",
		"LAST USED":    "2020-01-01",
		"AGE":          "12d",
		"ACTIVE":       "8h 20m",
		"COST":         "$41.20",
		"STATUS":       "UNUSED",
		"INTEGRATIONS": "ch mc",
	})}
	widths := naturalWidths(headers, rows)
	widths[0] = 10
	gotHeaders, gotRows := elideCells(headers, rows, widths)
	if got := dispWidth(gotHeaders[0]); got > 10 {
		t.Errorf("header width = %d, want <= 10", got)
	}
	path := gotRows[0][0]
	if got := dispWidth(path); got > 10 {
		t.Errorf("elided PATH width = %d, want <= 10", got)
	}
	if !strings.Contains(path, "…") {
		t.Errorf("elided PATH %q must carry a middle ellipsis", path)
	}
	if gotRows[0][1] != "feature/x" {
		t.Errorf("fitting cell changed: %q", gotRows[0][1])
	}
}

func TestResolveColorMode(t *testing.T) {
	cases := []struct {
		name  string
		mode  string
		isTTY bool
		env   map[string]string
		want  colorprofile.Profile
	}{
		{"always forces truecolor off a tty", colorAlways, false, nil, colorprofile.TrueColor},
		{"never disables on a tty", colorNever, true, nil, colorprofile.NoTTY},
		{"no_color on a tty keeps decoration", colorAuto, true, map[string]string{"NO_COLOR": "1"}, colorprofile.ASCII},
		{"no_color off a tty is plain", colorAuto, false, map[string]string{"NO_COLOR": "1"}, colorprofile.NoTTY},
		{"no_color wins over force_color", colorAuto, false, map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, colorprofile.NoTTY},
		{"force_color forces at least ansi", colorAuto, false, map[string]string{"FORCE_COLOR": "1"}, colorprofile.ANSI},
		{"cliclor zero disables", colorAuto, false, map[string]string{"CLICOLOR": "0"}, colorprofile.NoTTY},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearColorEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var got colorprofile.Profile
			// captureStdout swaps in a pipe so capability detection is deterministic.
			captureStdout(t, func() { got = resolveColorMode(tc.mode, tc.isTTY) })
			if got != tc.want {
				t.Errorf("resolveColorMode(%q, %v) = %v, want %v", tc.mode, tc.isTTY, got, tc.want)
			}
		})
	}
}

func clearColorEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{"NO_COLOR", "FORCE_COLOR", "CLICOLOR", "CLICOLOR_FORCE", "TERM", "COLORTERM"} {
		t.Setenv(k, "")
	}
}

func TestStatusCellStyle(t *testing.T) {
	cases := []struct {
		header, value string
		bold, faint   bool
		fg            color.Color
	}{
		{"STATUS", "ACTIVE", true, false, lipgloss.Green},
		{"STATUS", "UNUSED", false, false, lipgloss.Yellow},
		{"CHUNKHOUND", "yes", false, false, lipgloss.Green},
		{"CHUNKHOUND", "partial", false, false, lipgloss.Yellow},
		{"MCP", "-", false, true, nil},
		{"MCP", "n/a", false, true, nil},
		{"INTEGRATIONS", "ch mc", false, false, nil},
		{"UPSTREAM", "origin/feat", false, false, lipgloss.Green},
		{"UPSTREAM", "-", false, true, nil},
		{"BRANCH", "(detached)", false, true, nil},
		{"BRANCH", "main", false, false, nil},
		{"LAST USED", "never", false, true, nil},
		{"COST", "unknown", false, true, nil},
		{"COST", "-", false, true, nil},
		{"COST", "$4.00", false, false, nil},
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

// TestLegendStyleFollowsProfile pins the shared legend treatment: the legend is
// dim on a color terminal and byte-plain when color is off, so a legend never
// pollutes pipes or goldens.
func TestLegendStyleFollowsProfile(t *testing.T) {
	const line = "tokens: a = b"
	colored := captureStdout(t, func() { printLegend([]string{line}, colorAlways) })
	if !strings.Contains(colored, "\x1b[2m") {
		t.Fatalf("legend is not dim with --color=always: %q", colored)
	}
	if plain := captureStdout(t, func() { printLegend([]string{line}, colorNever) }); plain != line+"\n" {
		t.Fatalf("legend must be byte-plain with --color=never: %q", plain)
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
			got := statusCell(colIntegrations.header, tc.value)
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
		statusRow(map[string]string{"PATH": "/SBX/repo", "BRANCH": "main", "UPSTREAM": "origin/main", "LAST USED": "2020-01-01", "AGE": "12d", "ACTIVE": "8h 20m", "COST": "$41.20", "STATUS": "ACTIVE", "INTEGRATIONS": "ch mc"}),
		statusRow(map[string]string{"PATH": "/SBX/wt", "BRANCH": "(detached)", "UPSTREAM": "-", "LAST USED": "never", "AGE": "-", "ACTIVE": "-", "COST": "-", "STATUS": "UNUSED", "INTEGRATIONS": "-"}),
	}
	_, styled := renderCells(headers, rows, statusCell)
	for r := range rows {
		for c := range rows[r] {
			if got := ansi.Strip(styled[r][c]); got != rows[r][c] {
				t.Errorf("strip(%q) = %q, want %q", styled[r][c], got, rows[r][c])
			}
		}
	}
	idx := headerIndex(headers)
	for r, want := range []string{"ACTIVE", "UNUSED"} {
		if !strings.Contains(styled[r][idx["STATUS"]], "\x1b[") {
			t.Errorf("STATUS %q cell carries no ANSI: %q", want, styled[r][idx["STATUS"]])
		}
	}
	if !strings.Contains(styled[0][idx["INTEGRATIONS"]], "\x1b[") {
		t.Errorf("INTEGRATIONS cell carries no ANSI: %q", styled[0][idx["INTEGRATIONS"]])
	}
}

// TestStyledTableStripsToPlain is the end-to-end color contract: the styled
// table must strip back to the exact plain bytes, including alignment.
func TestStyledTableStripsToPlain(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{
		statusRow(map[string]string{"PATH": "/SBX/repo", "BRANCH": "main", "UPSTREAM": "origin/main", "LAST USED": "2020-01-01", "AGE": "12d", "ACTIVE": "8h 20m", "COST": "$41.20", "STATUS": "ACTIVE", "INTEGRATIONS": "ch mc"}),
		statusRow(map[string]string{"PATH": "/SBX/wt", "BRANCH": "(detached)", "UPSTREAM": "-", "LAST USED": "never", "AGE": "-", "ACTIVE": "-", "COST": "-", "STATUS": "UNUSED", "INTEGRATIONS": "-"}),
	}
	widths := naturalWidths(headers, rows)
	aligns := columnAligns(headers, columnRightAligns(statusColumns))
	plain := renderTable(headers, rows, widths, aligns)
	styledHeaders, styledRows := renderCells(headers, rows, statusCell)
	if got := ansi.Strip(renderTable(styledHeaders, styledRows, widths, aligns)); got != plain {
		t.Errorf("stripped styled table differs from plain:\n got: %q\nwant: %q", got, plain)
	}
}

func TestPrintTableRightAlign(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	headers := []string{"A", "BB"}
	rows := [][]string{{"x", "y"}}
	// colorNever is NoTTY and the renderer is nil, so the table renders plain.
	out := captureStdout(t, func() { printTable(headers, rows, colorNever, nil, []string{"BB"}) })
	want := "A  BB\nx   y\n"
	if out != want {
		t.Fatalf("right-aligned table = %q, want %q", out, want)
	}
}

// TestPrintKeyValues is the key/value block contract: headerless two-column
// layout on the shared two-cell gutter, no trailing padding.
func TestPrintKeyValues(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	rows := [][]string{
		{"current worktree", "/SBX/wt"},
		{"age", "6y"},
		{"active time", "45m"},
	}
	out := captureStdout(t, func() { printKeyValues(rows, 1, colorNever) })
	want := "current worktree  /SBX/wt\nage               6y\nactive time       45m\n"
	if out != want {
		t.Fatalf("key/value block = %q, want %q", out, want)
	}
}

// TestPrintKeyValuesStylesTitleAndLabels proves the title carries the title
// style, the other labels faint, and the styling strips back to the exact plain
// bytes.
func TestPrintKeyValuesStylesTitleAndLabels(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	rows := [][]string{{"current worktree", "/SBX/wt"}, {"age", "6y"}}
	styled := captureStdout(t, func() { printKeyValues(rows, 1, colorAlways) })
	if !strings.Contains(styled, "\x1b[1;36m") {
		t.Errorf("title label is not a cyan title: %q", styled)
	}
	if !strings.Contains(styled, "\x1b[2m") {
		t.Errorf("value label is not faint: %q", styled)
	}
	if got, want := ansi.Strip(styled), "current worktree  /SBX/wt\nage               6y\n"; got != want {
		t.Errorf("stripped block = %q, want %q", got, want)
	}
}

// TestPrintKeyValuesFitsWidth pins the summary block's budget: a long value is
// elided to fit, the label survives, and no line exceeds the budget.
func TestPrintKeyValuesFaintsNonValues(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	rows := [][]string{
		{"current worktree", "/SBX/wt"},
		{"active time", "-"},
		{"cost", "unknown"},
		{"age", "6y"},
	}
	styled := captureStdout(t, func() { printKeyValues(rows, 1, colorAlways) })
	// A non-value recedes with faint; a real value stays plain.
	for _, v := range []string{"-", "unknown"} {
		if !strings.Contains(styled, "\x1b[2m"+v) {
			t.Errorf("non-value %q is not faint: %q", v, styled)
		}
	}
	if strings.Contains(styled, "\x1b[2m6y") {
		t.Errorf("real value must stay plain: %q", styled)
	}
}

func TestPrintKeyValuesFitsWidth(t *testing.T) {
	rows := [][]string{
		{"current worktree", "/SBX/home/a/very/long/checkout/path/that/keeps/going/on"},
		{"cost", "$4.00"},
	}
	for _, width := range []int{80, 40, 30} {
		t.Run(fmt.Sprintf("%d", width), func(t *testing.T) {
			t.Setenv("COLUMNS", fmt.Sprintf("%d", width))
			out := captureStdout(t, func() { printKeyValues(rows, 1, colorNever) })
			for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
				if w := dispWidth(line); w > width {
					t.Errorf("line exceeds %d cells: %d\n%q", width, w, line)
				}
			}
			if !strings.Contains(out, "current worktree") {
				t.Errorf("label lost at %d cols:\n%s", width, out)
			}
		})
	}
}

func assertWidths(t *testing.T, got, want []int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("width count = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("width[%d] = %d, want %d", i, got[i], want[i])
		}
	}
}

func assertPinned(t *testing.T, headers []string, got, natural []int) {
	t.Helper()
	// STATUS is the only pinned status column (shrinkRank 0).
	for i, h := range headers {
		if h == "STATUS" && got[i] != natural[i] {
			t.Errorf("pinned column %s = %d, want %d", h, got[i], natural[i])
		}
	}
}

// assertNoSlivers pins the width contract: a column is either dropped whole or
// kept at or above its declared floor, so the width-fit pass never emits a
// one-cell sliver. A floor above the header width is the author's choice; the
// cost columns set theirs to the header width so the label always survives.
func assertNoSlivers(t *testing.T, headers []string, widths []int) {
	t.Helper()
	registry := columnRegistry()
	for i, w := range widths {
		if w == 0 {
			continue
		}
		floor := dispWidth(headers[i])
		if c, ok := registry[headers[i]]; ok && c.floor > 0 && c.floor < floor {
			floor = c.floor
		}
		if w < floor {
			t.Errorf("column %s = %d, narrower than its %d-cell floor", headers[i], w, floor)
		}
	}
}

func assertWithin(t *testing.T, widths []int, budget int) {
	t.Helper()
	if got := lineWidth(widths); got > budget {
		t.Errorf("lineWidth = %d, exceeds budget %d", got, budget)
	}
}

// TestDropZeroColumns pins that zero-width columns are removed whole: the
// returned headers, rows and widths stay index-aligned, and the gutter
// disappears too.
func TestDropZeroColumns(t *testing.T) {
	headers := []string{"A", "B", "C", "D"}
	rows := [][]string{
		{"a1", "b1", "c1", "d1"},
		{"a2", "b2", "c2", "d2"},
	}
	widths := []int{10, 0, 5, 0}
	h, r, w := dropZeroColumns(headers, rows, widths)
	if !slices.Equal(h, []string{"A", "C"}) {
		t.Errorf("headers = %v, want [A C]", h)
	}
	if !slices.Equal(r[0], []string{"a1", "c1"}) || !slices.Equal(r[1], []string{"a2", "c2"}) {
		t.Errorf("rows = %v", r)
	}
	if !slices.Equal(w, []int{10, 5}) {
		t.Errorf("widths = %v, want [10 5]", w)
	}
}

// TestDropZeroColumnsAllDropped pins that all-zero widths yield empty slices
// rather than nil slices, preserving the row structure.
func TestDropZeroColumnsAllDropped(t *testing.T) {
	headers := []string{"A", "B"}
	rows := [][]string{{"a1", "b1"}}
	widths := []int{0, 0}
	h, r, w := dropZeroColumns(headers, rows, widths)
	if len(h) != 0 || len(r[0]) != 0 || len(w) != 0 {
		t.Errorf("all-dropped = %v / %v / %v, want empty", h, r, w)
	}
}

// TestDropZeroColumnsDisclosesHiddenColumns pins the disclosure contract: any
// dropped column is named on stderr (stdout stays machine-clean), so a missing
// column reads as hidden, not absent.
func TestDropZeroColumnsDisclosesHiddenColumns(t *testing.T) {
	header := strings.ToUpper(capabilities()[0].id)
	headers := []string{"PATH", header}
	rows := [][]string{{"/x", "yes"}}
	t.Run("capability warns", func(t *testing.T) {
		errOut := captureStderr(t, func() { dropZeroColumns(headers, rows, []int{10, 0}) })
		if !strings.Contains(errOut, "terminal too narrow; hid "+header) {
			t.Fatalf("hidden capability must warn: %q", errOut)
		}
	})
	t.Run("core warns", func(t *testing.T) {
		errOut := captureStderr(t, func() { dropZeroColumns(headers, rows, []int{0, 5}) })
		if !strings.Contains(errOut, "terminal too narrow; hid PATH") {
			t.Fatalf("hidden core column must warn: %q", errOut)
		}
	})
}

// TestStatusCellStyleTrimsPaddedNonValue pins that a COST cell aligned with
// surrounding numbers — which pads an absent or unpriced value with spaces —
// still recedes as a non-value instead of reading as a number.
func TestStatusCellStyleTrimsPaddedNonValue(t *testing.T) {
	for _, v := range []string{"    -", "  unknown"} {
		if s := statusCellStyle("COST", v); !s.GetFaint() {
			t.Errorf("statusCellStyle(COST, %q) faint=false, want true", v)
		}
		if got := statusCell("COST", v); !strings.Contains(got, "\x1b[2m") {
			t.Errorf("statusCell(COST, %q) = %q, want faint ANSI", v, got)
		}
	}
	if s := statusCellStyle("COST", "$1.00"); s.GetFaint() {
		t.Errorf("statusCellStyle(COST, \"$1.00\") faint=true, want false")
	}
	if got := statusCell("COST", "$1.00"); strings.Contains(got, "\x1b[2m") {
		t.Errorf("statusCell(COST, \"$1.00\") = %q, want no faint ANSI", got)
	}
}
