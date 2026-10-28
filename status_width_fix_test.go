package main

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// status_width_fix_test.go — the measurement-preserving fit contract: AGE,
// ACTIVE, LAST USED, INTEGRATIONS and detail TIME render whole or drop whole.
// Optional columns (LIST, INTEGRATIONS) drop before identity columns shrink, so
// a narrow terminal never turns a duration into a different value.

// fitRowFor projects a header->value map onto any schema, so a fixture can
// include data-dependent columns (LIST) without assuming a statusRow height.
func fitRowFor(headers []string, values map[string]string) []string {
	row := make([]string, len(headers))
	for i, h := range headers {
		row[i] = values[h]
	}
	return row
}

// wideFitValues is the reported wide row: long identity values, a partial
// multi-month ACTIVE, and every optional column populated.
func wideFitValues() map[string]string {
	return map[string]string{
		"PATH":         "/Users/ofri/Documents/GitHub/agenticoding.github.io",
		"BRANCH":       "feature/cost-tracking-for-status",
		"UPSTREAM":     "origin/feature/cost-tracking-for-status",
		"LAST USED":    "2026-10-24",
		"AGE":          "10d 3h 27m",
		"ACTIVE":       "~2M 16d 22h 57m",
		"COST":         "$101.19",
		"LIST":         "$1,738.95",
		"STATUS":       "ACTIVE",
		"TEMP":         "-",
		"INTEGRATIONS": "chunk mcp pi claude",
	}
}

// shortFitValues is a realistic narrow row: short identity values, so after the
// optional columns drop the remaining table has nothing left to elide.
func shortFitValues() map[string]string {
	values := wideFitValues()
	values["PATH"] = "/SBX/wt"
	values["BRANCH"] = "main"
	values["UPSTREAM"] = "-"
	return values
}

// fittedTable is one status-table fit: the full schema, the widths it resolved
// to (0 = dropped), the rendered cells, and both output streams.
type fittedTable struct {
	headers []string
	natural []int
	widths  []int
	kept    []string
	cells   []string
	out     string
	stderr  string
}

// fitStatusFixture renders a status fixture at budget and returns the fit plus
// both streams. The schema is the real one (core columns, grouped
// INTEGRATIONS, plus the data-dependent LIST), so the test follows the
// production column set.
func fitStatusFixture(t *testing.T, values map[string]string, budget int) fittedTable {
	t.Helper()
	cols := withListColumn(statusColumns, true)
	headers := columnHeaders(cols)
	rows := [][]string{fitRowFor(headers, values)}
	var errOut string
	out := captureStdout(t, func() {
		errOut = captureStderr(t, func() {
			printTable(headers, rows, colorNever, statusCell, columnRightAligns(cols), budget)
		})
	})
	kept, keptRows, _, _ := sizedGrid(headers, rows, budget, columnRightAligns(cols))
	if len(kept) == 0 {
		t.Fatalf("no columns survived a %d-column budget", budget)
	}
	return fittedTable{
		headers: headers,
		natural: naturalWidths(headers, rows),
		widths:  columnWidths(headers, rows, budget),
		kept:    kept,
		cells:   keptRows[0],
		out:     out,
		stderr:  errOut,
	}
}

// dropped names the columns the fit removed, in schema order.
func (f fittedTable) dropped() []string { return hiddenHeaders(f.headers, f.widths) }

// cell returns a kept column's rendered value; "" when the column was dropped.
func (f fittedTable) cell(name string) string { return cellValue(f.kept, f.cells, name) }

// whole checks a kept column against its natural width: a kept measurement
// column must equal its natural width, which means no value was cut.
func (f fittedTable) whole(name string) bool {
	i := headerIndex(f.headers)[name]
	return f.widths[i] == 0 || f.widths[i] == f.natural[i]
}

// assertLinesWithin pins the rendered line width.
func assertLinesWithin(t *testing.T, out string, budget int) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := dispWidth(line); w > budget {
			t.Errorf("line exceeds %d cells: %d\n%q", budget, w, line)
		}
	}
}

func containsName(names []string, want string) bool { return slices.Contains(names, want) }

// detailTokenWorktree is a detail fixture with the reported shape: long model
// names, every token class, and a proven list price so LIST participates in the
// fit. Its natural width is driven by real cell content, so the test derives
// the budgets instead of hard-coding the report's 116.
func detailTokenWorktree(t *testing.T, now time.Time) worktree {
	t.Helper()
	var u worktreeUsage
	u.addSession("pi", []usageEvent{
		{at: now, model: "deepseek/deepseek-v4-flash-vision-exp", tokens: promptTokens{
			input: 54_000_000, output: 20_000_000, cacheRead: 1_900_000_000, cacheWrite5m: 500_000,
		}},
		{at: now.Add(time.Hour), model: "mimo-v2.5-pro", tokens: promptTokens{
			input: 3_900_000, output: 667_100, cacheRead: 75_900_000,
		}},
	}, func(usageEvent) pricedEvent {
		var list, billed money
		list.addKnown(41.20)
		billed.addKnown(0)
		return pricedEvent{total: costPair{list: list, billed: billed, subscription: true}}
	})
	return worktree{Path: filepath.Join(t.TempDir(), "wt"), Usage: &u}
}

// detailFitWidths is the width vector the renderer uses for a fitted detail
// schema: the token group may already be gone, so the fit runs on f.cols.
func detailFitWidths(f detailFit, budget int) ([]string, []int, []int) {
	headers := columnHeaders(f.cols)
	return headers, columnWidths(headers, f.rows, budget), naturalWidths(headers, f.rows)
}

// tokenWidths reports the six token-group widths in schema order.
func tokenWidths(headers []string, widths []int) []int {
	set := tokenHeaderSet()
	var out []int
	for i, h := range headers {
		if set[h] {
			out = append(out, widths[i])
		}
	}
	return out
}

// allZero reports whether no token column carries width. A partial token group
// is the failure this policy exists to prevent.
func allZero(widths []int) bool {
	return !slices.ContainsFunc(widths, func(w int) bool { return w > 0 })
}

func TestDetailTokenGroupSurvivesOneCellOverflow(t *testing.T) {
	now := at(0)
	cur := detailTokenWorktree(t, now)
	cols, rows := detailSchema(cur.Usage)
	natural := lineWidth(naturalWidths(columnHeaders(cols), rows))
	for _, budget := range []int{natural, natural - 1, natural - 10} {
		t.Run(fmt.Sprintf("%d", budget), func(t *testing.T) {
			f := fitDetailBlock(cur, now, budget)
			if f.dropped {
				t.Fatalf("token group dropped at %d of %d cells; LIST should yield first", budget, natural)
			}
			headers, widths, whole := detailFitWidths(f, budget)
			tokens := tokenWidths(headers, widths)
			if len(tokens) != len(tokenColumnNames()) || allZero(tokens) {
				t.Fatalf("token group missing at %d: %v", budget, tokens)
			}
			idx := headerIndex(headers)
			for _, h := range tokenColumnNames() {
				if got := widths[idx[h]]; got != whole[idx[h]] {
					t.Errorf("%s elided to %d cells, natural %d", h, got, whole[idx[h]])
				}
			}
			assertWithin(t, widths, budget)
		})
	}
}

func TestDetailTokenGroupDropsWholeOnlyAtModelFloor(t *testing.T) {
	now := at(0)
	cur := detailTokenWorktree(t, now)
	cols, rows := detailSchema(cur.Usage)
	headers := columnHeaders(cols)
	natural := naturalWidths(headers, rows)
	full := lineWidth(natural)
	idx := headerIndex(headers)
	// The tightest budget that still holds the token group: LIST is gone and
	// MODEL is at its 12-cell floor.
	tightest := full - (natural[idx[colList.header]] + 2) - (natural[idx[colModel.header]] - colModel.floor)
	for _, tc := range []struct {
		budget int
		kept   bool
	}{{tightest, true}, {tightest - 1, false}} {
		t.Run(fmt.Sprintf("%d", tc.budget), func(t *testing.T) {
			f := fitDetailBlock(cur, now, tc.budget)
			if f.dropped == tc.kept {
				t.Fatalf("dropped=%v at %d, want kept=%v", f.dropped, tc.budget, tc.kept)
			}
			headers, widths, _ := detailFitWidths(f, tc.budget)
			tokens := tokenWidths(headers, widths)
			if tc.kept {
				if allZero(tokens) {
					t.Fatalf("token group empty at %d", tc.budget)
				}
				if got := widths[headerIndex(headers)[colModel.header]]; got != colModel.floor {
					t.Errorf("MODEL width = %d at the tightest token budget, want floor %d", got, colModel.floor)
				}
			} else if !allZero(tokens) {
				t.Fatalf("partial token group at %d: %v", tc.budget, tokens)
			}
			var errOut string
			out := captureStdout(t, func() {
				errOut = captureStderr(t, func() {
					printStatusDetail([]worktree{cur}, cur.Path, now, colorNever, tc.budget)
				})
			})
			header := detailHeaderLine(out)
			for _, h := range tokenColumnNames() {
				if tc.kept && (!strings.Contains(header, h) || strings.Contains(errOut, h)) {
					t.Errorf("kept token %q missing from the header or disclosed as hidden:\n%s\n%s", h, header, errOut)
				}
				if !tc.kept && !strings.Contains(errOut, h) {
					t.Errorf("dropped token %q is not disclosed:\n%s", h, errOut)
				}
			}
			assertWithin(t, widths, tc.budget)
		})
	}
}

// TestFitKeepsMeasurementsWhole pins the reported bug: at 110 and 120 columns
// the optional columns drop whole and disclose themselves, while AGE, ACTIVE,
// LAST USED and COST keep their exact values.
func TestFitKeepsMeasurementsWhole(t *testing.T) {
	values := wideFitValues()
	for _, budget := range []int{110, 120} {
		t.Run(fmt.Sprintf("%d", budget), func(t *testing.T) {
			f := fitStatusFixture(t, values, budget)
			assertLinesWithin(t, f.out, budget)
			for _, h := range []string{"LAST USED", "AGE", "ACTIVE", "COST"} {
				if !f.whole(h) {
					t.Errorf("%s was cut at %d columns: width %d, natural %d", h, budget, f.widths[headerIndex(f.headers)[h]], f.natural[headerIndex(f.headers)[h]])
				}
				if got := f.cell(h); got != values[h] {
					t.Errorf("%s = %q at %d columns, want %q", h, got, budget, values[h])
				}
			}
			if !f.whole("INTEGRATIONS") {
				t.Errorf("INTEGRATIONS must be whole or dropped, width %d", f.widths[headerIndex(f.headers)["INTEGRATIONS"]])
			}
			if got := f.cell("INTEGRATIONS"); got != "" && got != values["INTEGRATIONS"] {
				t.Errorf("INTEGRATIONS = %q, want whole %q or dropped", got, values["INTEGRATIONS"])
			}
			dropped := f.dropped()
			if !containsName(dropped, "LIST") && !containsName(dropped, "INTEGRATIONS") {
				t.Errorf("an optional column must drop at %d columns, dropped %v", budget, dropped)
			}
			for _, h := range dropped {
				if !strings.Contains(f.stderr, h) {
					t.Errorf("dropped %s must be named on stderr: %q", h, f.stderr)
				}
				switch h {
				case "LAST USED", "AGE", "ACTIVE", "COST":
					t.Errorf("measurement column %s must never drop before the optional columns", h)
				}
			}
		})
	}
}

// TestFitAtEightyHasOnlyWholeValues pins the last narrow step: once the optional
// columns drop, everything still shown fits its natural width, so the table
// carries no ellipsis at all.
func TestFitAtEightyHasOnlyWholeValues(t *testing.T) {
	values := shortFitValues()
	for _, budget := range []int{80, 100} {
		t.Run(fmt.Sprintf("%d", budget), func(t *testing.T) {
			f := fitStatusFixture(t, values, budget)
			assertLinesWithin(t, f.out, budget)
			if strings.Contains(f.out, "…") {
				t.Errorf("the table at %d columns must carry only whole values:\n%s", budget, f.out)
			}
			for _, i := range []int{headerIndex(f.headers)["LIST"], headerIndex(f.headers)["INTEGRATIONS"]} {
				if f.widths[i] != 0 {
					t.Errorf("%s must drop whole at %d columns", f.headers[i], budget)
				}
			}
			if budget == 100 && f.widths[headerIndex(f.headers)["PATH"]] == 0 {
				t.Errorf("PATH must survive while INTEGRATIONS can absorb the overflow: %q", f.stderr)
			}
			if budget == 80 && f.widths[headerIndex(f.headers)["PATH"]] != 0 {
				t.Errorf("PATH must drop whole at 80 columns, stderr %q", f.stderr)
			}
			for i, h := range f.headers {
				if f.widths[i] != 0 && f.widths[i] != f.natural[i] {
					t.Errorf("%s = %d, want whole %d or dropped at %d columns", h, f.widths[i], f.natural[i], budget)
				}
			}
			for _, h := range f.dropped() {
				if !strings.Contains(f.stderr, h) {
					t.Errorf("dropped %s must be named on stderr: %q", h, f.stderr)
				}
			}
		})
	}
}

// TestFitKeepsDetailTimeWhole pins the detail block's TIME contract: TIME keeps
// its whole value while it fits and drops whole (never middle-elided) when it
// does not.
func TestFitKeepsDetailTimeWhole(t *testing.T) {
	cols := detailColumns()
	headers := columnHeaders(cols)
	row := make([]string, len(headers))
	for i, h := range headers {
		switch h {
		case "MODEL":
			row[i] = "claude-sonnet-4-5-20250929"
		case "TIME":
			row[i] = "1h 40m"
		case "COST":
			row[i] = "$5.15"
		default:
			row[i] = "1234"
		}
	}
	rows := [][]string{row}
	natural := naturalWidths(headers, rows)
	timeIdx := headerIndex(headers)["TIME"]
	for _, budget := range []int{40, 60} {
		t.Run(fmt.Sprintf("%d", budget), func(t *testing.T) {
			kept, keptRows, _, _ := sizedGrid(headers, rows, budget, columnRightAligns(cols))
			i, ok := headerIndex(kept)["TIME"]
			if !ok || keptRows[0][i] != "1h 40m" {
				t.Errorf("TIME must render whole at %d columns, got %q", budget, cellValue(kept, keptRows[0], "TIME"))
			}
			if strings.Contains(cellValue(kept, keptRows[0], "TIME"), "…") {
				t.Errorf("TIME must never middle-elide: %q", cellValue(kept, keptRows[0], "TIME"))
			}
		})
	}
	t.Run("dropped whole", func(t *testing.T) {
		widths := columnWidths(headers, rows, 8)
		if w := widths[timeIdx]; w != 0 && w != natural[timeIdx] {
			t.Errorf("TIME width = %d, want whole %d or dropped", w, natural[timeIdx])
		}
	})
}

// cellValue returns a named column's value from one row; "" when the column is
// absent (dropped by the fit).
func cellValue(headers, row []string, name string) string {
	i, ok := headerIndex(headers)[name]
	if !ok {
		return ""
	}
	return row[i]
}
