package main

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// ui_test.go — pure width-budget and color contracts for ui.go. No TTY, no clock.

func statusHeaders() []string { return statusTableHeaders }

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

func TestColumnWidths(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{{
		"/a/very/long/checkout/path/that/keeps/going/on",
		"feature/a-rather-long-branch",
		"origin/feature/a-rather-long-branch",
		"2020-01-01", "UNUSED", "yes", "yes", "yes",
	}}
	natural := naturalWidths(headers, rows)
	naturalTotal := lineWidth(natural)

	t.Run("budget zero is natural", func(t *testing.T) {
		assertWidths(t, columnWidths(headers, rows, 0), natural)
	})
	t.Run("generous budget is natural", func(t *testing.T) {
		assertWidths(t, columnWidths(headers, rows, naturalTotal+40), natural)
	})
	t.Run("path shrinks before branch", func(t *testing.T) {
		budget := naturalTotal - 4
		got := columnWidths(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, got, natural)
		if got[0] >= natural[0] {
			t.Errorf("PATH should shrink first: got %d, natural %d", got[0], natural[0])
		}
		if got[1] != natural[1] || got[2] != natural[2] {
			t.Errorf("BRANCH/UPSTREAM must not shrink while PATH has room: got %d/%d, natural %d/%d", got[1], got[2], natural[1], natural[2])
		}
	})
	t.Run("upstream shrinks after path floor", func(t *testing.T) {
		budget := naturalTotal - (natural[0] - shrinkFloor["PATH"]) - 3
		got := columnWidths(headers, rows, budget)
		assertWithin(t, got, budget)
		assertPinned(t, got, natural)
		if got[0] != shrinkFloor["PATH"] {
			t.Errorf("PATH = %d, want floor %d", got[0], shrinkFloor["PATH"])
		}
		if got[2] != natural[2]-3 {
			t.Errorf("UPSTREAM = %d, want %d", got[2], natural[2]-3)
		}
	})
	t.Run("tight budget still fits and keeps pinned columns", func(t *testing.T) {
		got := columnWidths(headers, rows, 45)
		assertWithin(t, got, 45)
		assertPinned(t, got, natural)
	})
}

func TestElideCells(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{{
		"/a/very/long/checkout/path",
		"feature/x",
		"origin/x",
		"2020-01-01", "UNUSED", "yes", "yes", "yes",
	}}
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
		{"CONFIG", "yes", false, false, lipgloss.Green},
		{"DB", "yes", false, false, lipgloss.Green},
		{"MCP", "-", false, true, nil},
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

// TestStyledCellsStripToPlain pins the color contract: styling is pure
// reinforcement, so stripping ANSI must reproduce the plain cell exactly.
func TestStyledCellsStripToPlain(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{
		{"/SBX/repo", "main", "origin/main", "2020-01-01", "ACTIVE", "yes", "-", "yes"},
		{"/SBX/wt", "(detached)", "-", "never", "UNUSED", "-", "yes", "-"},
	}
	_, styled := styleCells(headers, rows, statusCellStyle)
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
}

// TestStyledTableStripsToPlain is the end-to-end color contract: the styled
// table must strip back to the exact plain bytes, including alignment.
func TestStyledTableStripsToPlain(t *testing.T) {
	headers := statusHeaders()
	rows := [][]string{
		{"/SBX/repo", "main", "origin/main", "2020-01-01", "ACTIVE", "yes", "-", "yes"},
		{"/SBX/wt", "(detached)", "-", "never", "UNUSED", "-", "yes", "-"},
	}
	widths := naturalWidths(headers, rows)
	aligns := columnAligns(headers, statusRightAlign)
	plain := renderTable(headers, rows, widths, aligns)
	styledHeaders, styledRows := styleCells(headers, rows, statusCellStyle)
	if got := ansi.Strip(renderTable(styledHeaders, styledRows, widths, aligns)); got != plain {
		t.Errorf("stripped styled table differs from plain:\n got: %q\nwant: %q", got, plain)
	}
}

func TestPrintTableRightAlign(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	headers := []string{"A", "BB"}
	rows := [][]string{{"x", "y"}}
	out := captureStdout(t, func() { printTable(headers, rows, colorAuto, nil, []string{"BB"}) })
	want := "A  BB\nx   y\n"
	if out != want {
		t.Fatalf("right-aligned table = %q, want %q", out, want)
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

func assertPinned(t *testing.T, got, natural []int) {
	t.Helper()
	for _, i := range []int{4, 5, 6, 7} {
		if got[i] != natural[i] {
			t.Errorf("pinned column %d = %d, want %d", i, got[i], natural[i])
		}
	}
}

func assertWithin(t *testing.T, widths []int, budget int) {
	t.Helper()
	if got := lineWidth(widths); got > budget {
		t.Errorf("lineWidth = %d, exceeds budget %d", got, budget)
	}
}
