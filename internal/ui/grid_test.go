package ui

import (
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	testutil "wtm/internal/testutil"
)

// grid_test.go — the grid engine's own contracts (width, elision, drop
// disclosure, and ids pruned in lockstep with columns), synthetic fixtures only.

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
			// CaptureStdout swaps in a pipe, so the TTY fallback must yield 0
			// regardless of the terminal `go test` happens to run in.
			testutil.CaptureStdout(t, func() { got = WidthBudget() })
			if got != tc.want {
				t.Errorf("WidthBudget() = %d, want %d", got, tc.want)
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
			if got := MiddleElide(tc.in, tc.w); got != tc.want {
				t.Errorf("MiddleElide(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
			}
		})
	}
	// Wide graphemes must never let an elision blow the budget.
	wide := "日本語のとても長いパス🎉"
	for w := 1; w <= 20; w++ {
		if got := DispWidth(MiddleElide(wide, w)); got > w {
			t.Errorf("MiddleElide(%q, %d) width = %d, want <= %d", wide, w, got, w)
		}
	}
}

func TestElideCells(t *testing.T) {
	headers := []string{"/a/very/long/checkout/path", "feature/x", "origin/x", "2020-01-01", "UNUSED", "ch mc"}
	rows := [][]string{headers}
	widths := NaturalWidths(headers, rows)
	widths[0] = 10
	gotHeaders, gotRows := ElideCells(headers, rows, widths)
	if got := DispWidth(gotHeaders[0]); got > 10 {
		t.Errorf("header width = %d, want <= 10", got)
	}
	path := gotRows[0][0]
	if got := DispWidth(path); got > 10 {
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
		{"always forces truecolor off a tty", ColorAlways, false, nil, colorprofile.TrueColor},
		{"never disables on a tty", ColorNever, true, nil, colorprofile.NoTTY},
		{"no_color on a tty keeps decoration", ColorAuto, true, map[string]string{"NO_COLOR": "1"}, colorprofile.ASCII},
		{"no_color off a tty is plain", ColorAuto, false, map[string]string{"NO_COLOR": "1"}, colorprofile.NoTTY},
		{"no_color wins over force_color", ColorAuto, false, map[string]string{"NO_COLOR": "1", "FORCE_COLOR": "1"}, colorprofile.NoTTY},
		{"force_color forces at least ansi", ColorAuto, false, map[string]string{"FORCE_COLOR": "1"}, colorprofile.ANSI},
		{"cliclor zero disables", ColorAuto, false, map[string]string{"CLICOLOR": "0"}, colorprofile.NoTTY},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clearColorEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			var got colorprofile.Profile
			testutil.CaptureStdout(t, func() { got = ResolveColorMode(tc.mode, tc.isTTY) })
			if got != tc.want {
				t.Errorf("ResolveColorMode(%q, %v) = %v, want %v", tc.mode, tc.isTTY, got, tc.want)
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

func TestPrintTableRightAlign(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	headers := []string{"A", "BB"}
	rows := [][]string{{"x", "y"}}
	out := testutil.CaptureStdout(t, func() { PrintTable(headers, nil, rows, ColorNever, nil, []string{"BB"}, Policy{}) })
	want := "A  BB\nx   y\n"
	if out != want {
		t.Fatalf("right-aligned table = %q, want %q", out, want)
	}
}

func TestDropZeroColumns(t *testing.T) {
	headers := []string{"A", "B", "C", "D"}
	rows := [][]string{
		{"a1", "b1", "c1", "d1"},
		{"a2", "b2", "c2", "d2"},
	}
	widths := []int{10, 0, 5, 0}
	ids := []string{"a", "b", "c", "d"}
	h, idOut, r, w := DropZeroColumns(headers, ids, rows, widths, nil)
	if !slices.Equal(h, []string{"A", "C"}) {
		t.Errorf("headers = %v, want [A C]", h)
	}
	if !slices.Equal(idOut, []string{"a", "c"}) {
		t.Errorf("ids = %v, want [a c]", idOut)
	}
	if !slices.Equal(r[0], []string{"a1", "c1"}) || !slices.Equal(r[1], []string{"a2", "c2"}) {
		t.Errorf("rows = %v", r)
	}
	if !slices.Equal(w, []int{10, 5}) {
		t.Errorf("widths = %v, want [10 5]", w)
	}
}

func TestDropZeroColumnsAllDropped(t *testing.T) {
	headers := []string{"A", "B"}
	rows := [][]string{{"a1", "b1"}}
	widths := []int{0, 0}
	h, idOut, r, w := DropZeroColumns(headers, nil, rows, widths, nil)
	if len(h) != 0 || len(r[0]) != 0 || len(w) != 0 || idOut != nil {
		t.Errorf("all-dropped = %v / %v / %v / %v, want empty and nil ids", h, idOut, r, w)
	}
}

// TestDropZeroColumnsWarnsForHiddenCapability pins the disclosure contract: a
// dropped capability column is named on stderr (stdout stays machine-clean),
// while a dropped core column stays silent. Only capability columns drop whole.
func TestDropZeroColumnsWarnsForHiddenCapability(t *testing.T) {
	capHeader := func(h string) bool { return h == "CAPX" }
	headers := []string{"PATH", "CAPX"}
	rows := [][]string{{"/x", "yes"}}
	t.Run("capability warns", func(t *testing.T) {
		errOut := testutil.CaptureStderr(t, func() { DropZeroColumns(headers, nil, rows, []int{10, 0}, capHeader) })
		if !strings.Contains(errOut, "terminal too narrow; hid CAPX") {
			t.Fatalf("hidden capability must warn: %q", errOut)
		}
	})
	t.Run("core stays silent", func(t *testing.T) {
		errOut := testutil.CaptureStderr(t, func() { DropZeroColumns(headers, nil, rows, []int{0, 5}, capHeader) })
		if errOut != "" {
			t.Fatalf("hidden core column must not warn: %q", errOut)
		}
	})
}

// TestSizedGridPrunesIDs pins that ids drop in lockstep with headers/rows, so
// a dropped column leaves no orphan id behind.

func TestSizedGridPrunesIDs(t *testing.T) {
	headers := []string{"A", "B"}
	ids := []string{"id-a", "id-b"}
	rows := [][]string{{"x", "y"}}
	p := Policy{Priority: []string{"B"}, Floors: map[string]int{"B": 0}}
	h, idOut, r, _, _ := SizedGrid(headers, ids, rows, 3, nil, p)
	if !slices.Equal(h, []string{"A"}) || !slices.Equal(idOut, []string{"id-a"}) {
		t.Fatalf("SizedGrid = headers %v ids %v, want [A] [id-a]", h, idOut)
	}
	if !slices.Equal(r[0], []string{"x"}) {
		t.Fatalf("rows = %v, want [[x]]", r)
	}
}

// TestPrintTableRendersPrunedIDs pins the end-to-end invariant: a dropped
// column leaves the renderer with only the kept ids.
func TestPrintTableRendersPrunedIDs(t *testing.T) {
	t.Setenv("COLUMNS", "3")
	headers := []string{"A", "B"}
	ids := []string{"id-a", "id-b"}
	rows := [][]string{{"x", "y"}}
	p := Policy{Priority: []string{"B"}, Floors: map[string]int{"B": 0}}
	var seen []string
	render := func(id, val string) string { seen = append(seen, id); return id }
	out := testutil.CaptureStdout(t, func() { PrintTable(headers, ids, rows, ColorAlways, render, nil, p) })
	if !slices.Equal(seen, []string{"id-a"}) {
		t.Fatalf("render saw ids %v, want [id-a]", seen)
	}
	if strings.Contains(out, "id-b") {
		t.Fatalf("output must not contain dropped id: %q", out)
	}
}
