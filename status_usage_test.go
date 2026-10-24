package main

import (
	"encoding/json"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// status_usage_test.go — status-level contracts for cost and active time: the
// JSON extension, the detail block, and the harness-tagged usage loader.

func fptr(v float64) *float64 { return &v }

// fixtureUsage is a Pi worktree: deepseek-flash for an hour ending at 1h,
// grok-4.3 at the 2h mark, one harness session spanning two hours. Costs attach
// to the first event of each model so the harness total is 4.00.
func fixtureUsage() *worktreeUsage {
	var u worktreeUsage
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	u.addSession("pi", []usageEvent{
		{at: base, model: "deepseek-flash", cost: fptr(2.90)},
		{at: base.Add(time.Hour), model: "deepseek-flash"},
		{at: base.Add(2 * time.Hour), model: "grok-4.3", cost: fptr(1.10)},
	}, pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		} else {
			m.addKnown(0)
		}
		return m
	}))
	return &u
}

// pricedTokens is a one-model Pi worktree that recorded tk and $1, so a token
// aggregate test needs no session file.
func pricedTokens(tk promptTokens) *worktreeUsage {
	var u worktreeUsage
	u.addSession("pi", []usageEvent{{at: at(0), model: "m", tokens: tk, cost: fptr(1)}}, pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(*e.cost)
		return m
	}))
	return &u
}

func TestStatusJSONUsageContract(t *testing.T) {
	with := worktree{Path: "/with", Usage: fixtureUsage()}
	without := worktree{Path: "/without"}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{with, without}, at(0), 10*time.Hour, "/with"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !got[0].Current {
		t.Error("current flag lost")
	}
	if got[0].Cost == nil || *got[0].Cost != 4.0 {
		t.Errorf("cost = %v, want 4.0", got[0].Cost)
	}
	if got[0].ActiveSeconds == nil || *got[0].ActiveSeconds != 7200 {
		t.Errorf("activeSeconds = %v, want 7200", got[0].ActiveSeconds)
	}
	pi, ok := got[0].Usage["pi"]
	if !ok {
		t.Fatalf("usage.pi missing: %s", out)
	}
	if pi.Cost == nil || *pi.Cost != 4.0 {
		t.Errorf("pi cost = %v, want 4.0", pi.Cost)
	}
	if pi.ActiveSeconds != 7200 {
		t.Errorf("pi activeSeconds = %d, want 7200", pi.ActiveSeconds)
	}
	if m := pi.Models["deepseek-flash"]; m.Cost == nil || *m.Cost != 2.9 {
		t.Errorf("deepseek cost = %v, want 2.9", m.Cost)
	}
	// A worktree with no events omits activeSeconds and usage and keeps cost null.
	if got[1].ActiveSeconds != nil || got[1].Usage != nil {
		t.Errorf("eventless worktree carried usage: %+v", got[1])
	}
	if got[1].Cost != nil {
		t.Errorf("eventless cost = %v, want null", got[1].Cost)
	}
}

// estimatedUsage is a worktree whose only cost rests on an assumption, so the
// tilde and the JSON flag have something to mark.
func estimatedUsage() *worktreeUsage {
	var u worktreeUsage
	u.addSession("claude", []usageEvent{{at: at(0), model: "claude-sonnet-4-6"}}, pairPricer(func(usageEvent) money {
		var m money
		m.addEstimated(1.23, causeCacheWrite)
		return m
	}))
	return &u
}

// TestStatusJSONEstimatedFlag pins the machine caveat: an estimated amount still
// serializes as a number, and its side carries an explicit flag.
func TestStatusJSONEstimatedFlag(t *testing.T) {
	with := worktree{Path: "/with", Usage: estimatedUsage()}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{with}, at(0), 10*time.Hour, "/with"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !got[0].CostEstimated {
		t.Errorf("costEstimated = false, want true\n%s", out)
	}
	if got[0].Cost == nil {
		t.Errorf("estimated cost lost its number\n%s", out)
	}
	if m := got[0].Usage["claude"].Models["claude-sonnet-4-6"]; !m.CostEstimated {
		t.Errorf("model costEstimated = false, want true\n%s", out)
	}
}

// TestStatusCostLegend pins the cost legend: each set reason prints its SSOT
// line, the LIST column prints its own line, and an unset one stays silent. The
// asserts reference the constants, so prose edits move both sides together.
func TestStatusCostLegend(t *testing.T) {
	cacheOnly := captureStdout(t, func() { printCostLegend(costLegend{tableReasons: estimateReasons{cache: true}}, colorNever, 0) })
	if !strings.Contains(cacheOnly, estimateLabel+" = "+estimateCacheWriteLifetime) {
		t.Fatalf("cache-only legend missing its line:\n%s", cacheOnly)
	}
	if strings.Contains(cacheOnly, estimateSubscriptionOverage) {
		t.Fatalf("cache-only legend must not explain overage:\n%s", cacheOnly)
	}
	overageOnly := captureStdout(t, func() { printCostLegend(costLegend{tableReasons: estimateReasons{overage: true}}, colorNever, 0) })
	if !strings.Contains(overageOnly, estimateLabel+" = "+estimateSubscriptionOverage) {
		t.Fatalf("overage-only legend missing its line:\n%s", overageOnly)
	}
	if strings.Contains(overageOnly, estimateCacheWriteLifetime) {
		t.Fatalf("overage-only legend must not explain the cache split:\n%s", overageOnly)
	}
	both := captureStdout(t, func() {
		printCostLegend(costLegend{tableReasons: estimateReasons{cache: true, overage: true}, list: true}, colorNever, 0)
	})
	if strings.Count(both, estimateLabel+" = ") != 2 || !strings.Contains(both, listLegend.text) {
		t.Fatalf("full legend must print 2 estimate lines and the list line, got:\n%s", both)
	}
	if got := captureStdout(t, func() { printCostLegend(costLegend{}, colorNever, 0) }); got != "" {
		t.Fatalf("no-caveat legend must be empty, got:\n%q", got)
	}
	// The LIST line prints under the table even when the breakdown renders the
	// column too, so the table scanner never meets an unexplained second price.
	if got := captureStdout(t, func() { printCostLegend(costLegend{list: true}, colorNever, 0) }); !strings.Contains(got, listLegend.text) {
		t.Fatalf("a table-kept list line must print under the table, got:\n%q", got)
	}
	detail := captureStdout(t, func() { printDetailLegend(estimateReasons{}, true, false, true, colorNever, 0) })
	// The token group is a titled block: the heading prints alone, its clauses
	// indent under it, and every "=" still shares one column with the LIST line.
	wantDetail := "\nlist   = " + listLegend.text + "\n" +
		"tokens:\n" +
		"  in   = " + tokenLegend[0].text + "\n" +
		"  miss = " + tokenLegend[1].text + "\n" +
		"  hit% = " + tokenLegend[2].text + "\n"
	if detail != wantDetail {
		t.Fatalf("breakdown legend = \n%s\nwant:\n%s", detail, wantDetail)
	}
	if got := captureStdout(t, func() { printDetailLegend(estimateReasons{}, false, false, false, colorNever, 0) }); got != "" {
		t.Fatalf("an empty breakdown legend must print nothing, got:\n%q", got)
	}
	partial := captureStdout(t, func() { printCostLegend(costLegend{partial: true}, colorNever, 0) })
	if !strings.Contains(partial, partialLabel+" = "+partialLegend) {
		t.Fatalf("partial table legend missing its line:\n%s", partial)
	}
	if strings.Contains(partial, estimateLabel+" = ") {
		t.Fatalf("partial-only legend must not explain an estimate:\n%s", partial)
	}
	detailPartial := captureStdout(t, func() { printDetailLegend(estimateReasons{}, false, true, false, colorNever, 0) })
	if !strings.Contains(detailPartial, partialLabel+" = "+partialLegend) {
		t.Fatalf("partial detail legend missing its line:\n%s", detailPartial)
	}
}

// TestEstimateLegendNeededFollowsRenderedCells pins the anti-dangle rule: the
// legend is needed only when a "~" cell actually renders, so a dropped COST
// column suppresses it. The estimated worktree is not the current one, so only
// the table can show the mark.
func TestCostLegendNeededFollowsRenderedCells(t *testing.T) {
	with := worktree{Path: "/with", Usage: estimatedUsage()}
	cols := statusSchema(false, []worktree{with})
	if r := costLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour, 0); !r.tableReasons.any() {
		t.Fatal("an estimated table total must ask for the legend")
	}
	if r := costLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour, 5); r.tableReasons.any() {
		t.Fatal("a dropped COST column must not ask for the legend")
	}
}

// TestCostLegendSelectivity pins the reason-aware legend: a cache-only
// total prints one line, an overage-only total prints the other, both print
// two, and an exact total prints none.
func TestCostLegendSelectivity(t *testing.T) {
	cacheEst := func() money { var m money; m.addEstimated(1, causeCacheWrite); return m }()
	overageEst := func() costPair {
		var p costPair
		p.list.addKnown(1)
		p.billed = estimatedZero()
		p.subscription = true
		return p
	}()
	pairUsage := func(p costPair) *worktreeUsage {
		u := &worktreeUsage{harnesses: map[string]*harnessUsage{
			"pi": {cost: p, models: map[string]*modelUsage{"m": {cost: p}}},
		}}
		return u
	}
	cols := statusColumnsFor(false)
	cases := []struct {
		name      string
		pair      costPair
		wantCache bool
		wantOver  bool
	}{
		{"cache-only", costPair{list: cacheEst, billed: cacheEst}, true, false},
		// A proven subscription may carry the same cache-write guess; it must
		// still ask for the cache line only, never the allowance caveat.
		{"covered-cache", costPair{list: cacheEst, billed: cacheEst, subscription: true}, true, false},
		{"overage-only", overageEst, false, true},
		{"exact", costPair{list: func() money { var m money; m.addKnown(1); return m }(), billed: func() money { var m money; m.addKnown(0); return m }(), subscription: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := worktree{Path: "/with", Usage: pairUsage(tc.pair)}
			r := costLegendNeeded(cols, []worktree{w}, "/elsewhere", at(0), 10*time.Hour, 0).tableReasons
			if r.cache != tc.wantCache || r.overage != tc.wantOver {
				t.Fatalf("reasons = %+v, want cache=%v overage=%v", r, tc.wantCache, tc.wantOver)
			}
			out := captureStdout(t, func() { printCostLegend(costLegend{tableReasons: r}, colorNever, 0) })
			wantLines := 0
			if tc.wantCache {
				wantLines++
			}
			if tc.wantOver {
				wantLines++
			}
			if got := strings.Count(out, estimateLabel+" = "); got != wantLines {
				t.Fatalf("legend lines = %d, want %d:\n%s", got, wantLines, out)
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		both := overageEst
		both.list = cacheEst
		w := worktree{Path: "/with", Usage: pairUsage(both)}
		r := costLegendNeeded(cols, []worktree{w}, "/elsewhere", at(0), 10*time.Hour, 0).tableReasons
		if !r.cache || !r.overage {
			t.Fatalf("reasons = %+v, want both", r)
		}
		if out := captureStdout(t, func() { printCostLegend(costLegend{tableReasons: r}, colorNever, 0) }); strings.Count(out, estimateLabel+" = ") != 2 {
			t.Fatalf("both-reasons legend must print 2 lines:\n%s", out)
		}
	})
}

func TestListCostJSONOmitsProvenUnknown(t *testing.T) {
	both := costPair{subscription: true}
	both.billed.addUnknown()
	both.list.addUnknown()
	if got := listCostJSON(both); got != nil {
		t.Errorf("both-unknown listCost = %+v, want nil (omit the key)", got)
	}
	unproven := costPair{}
	unproven.billed.addKnown(1)
	unproven.list.addKnown(2)
	if got := listCostJSON(unproven); got != nil {
		t.Errorf("unproven listCost = %+v, want nil", got)
	}
	equal := costPair{subscription: true}
	equal.billed.addKnown(3)
	equal.list.addKnown(3)
	if got := listCostJSON(equal); got != nil {
		t.Errorf("proven-equal listCost = %+v, want nil", got)
	}
	mixed := costPair{subscription: true, billed: knownZero()}
	mixed.list.addUnknown()
	lc := listCostJSON(mixed)
	if lc == nil {
		t.Fatalf("known-billed/unknown-list listCost = nil, want explicit null")
	}
	raw, err := json.Marshal(lc)
	if err != nil {
		t.Fatalf("marshal listCost: %v", err)
	}
	if string(raw) != "null" {
		t.Errorf("listCost = %s, want null", raw)
	}
}

func TestStatusDetailBlock(t *testing.T) {
	with := worktree{Path: "/with", Usage: fixtureUsage()}
	empty := worktree{Path: "/empty"}
	other := worktree{Path: "/other", Usage: fixtureUsage()}

	// No harness anywhere has events: the block is omitted.
	if out := captureStdout(t, func() { printStatusDetail([]worktree{empty}, "/empty", at(0), colorNever, 0) }); out != "" {
		t.Fatalf("block must be omitted when the repo has no usage:\n%s", out)
	}

	// The current worktree has events: a separated, titled block, then rows.
	out := captureStdout(t, func() { printStatusDetail([]worktree{with, other}, "/with", at(0), colorNever, 0) })
	if !strings.HasPrefix(out, "\ncurrent worktree") {
		t.Errorf("block must open with a blank line, then the identity row:\n%q", out)
	}
	for _, want := range []string{"current worktree", "active time", "MODEL", "↑ in", "↓ out", "↩ hit", "↪ write", "× miss", "● hit%", "deepseek-flash", "grok-4.3", "$4.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail block missing %q:\n%s", want, out)
		}
	}
	// The summary labels keep their order above the breakdown grid.
	prev := 0
	for _, label := range []string{"\ncurrent worktree", "\nage", "\nactive time", "\ncost", "\nMODEL"} {
		i := strings.Index(out, label)
		if i < prev {
			t.Errorf("detail label %q out of order:\n%s", label, out)
		}
		prev = i
	}
	if strings.Contains(out, "\x1b") {
		t.Errorf("NoTTY detail block carried ANSI:\n%q", out)
	}

	// The repo has usage, but not in the current worktree: the note replaces rows.
	out = captureStdout(t, func() { printStatusDetail([]worktree{empty, other}, "/empty", at(0), colorNever, 0) })
	if !strings.Contains(out, "no agent usage recorded") {
		t.Errorf("empty current worktree must say so:\n%s", out)
	}
	if strings.Contains(out, "MODEL") {
		t.Errorf("empty current worktree must not print rows:\n%s", out)
	}
}

// TestStatusDetailTokenColumns pins the breakdown contract: one column per token
// metric, each headed by its glyph and label from detailTokenMeta, with every
// row's value in the matching column.
func TestStatusDetailTokenColumns(t *testing.T) {
	headers := columnHeaders(detailColumns())
	for i, d := range detailTokenMeta {
		if got, want := headers[3+i], d.glyph+" "+d.label; got != want {
			t.Errorf("token column %d header = %q, want %q", i, got, want)
		}
	}
	var u worktreeUsage
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "m", tokens: promptTokens{input: 5_300_000, output: 732, cacheWrite5m: 714_800}, cost: fptr(1)},
	}, pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(*e.cost)
		return m
	}))
	// in is the total prompt size (5.3M + 714.8k = 6.0M), not the uncached slice.
	if got, want := detailRows(&u, false)[0], []string{"m", "0s", "$1.00", "6.0M", "732", "-", "714.8k", "6.0M", "0.0%"}; !slices.Equal(got, want) {
		t.Errorf("detail row = %q, want %q", got, want)
	}
}

// TestStatusDetailTokenGlyphs pins the token group's glyph invariant: one
// display cell, text presentation (no U+FE0F). A 2-cell or color emoji overflows
// its box and clips the neighbouring label, and a fallback-font dingbat renders
// thin beside the bold header, so both classes are refused here, not at the
// terminal.
func TestStatusDetailTokenGlyphs(t *testing.T) {
	for _, d := range detailTokenMeta {
		if got := dispWidth(d.glyph); got != 1 {
			t.Errorf("%q glyph %q = %d cells, want 1", d.label, d.glyph, got)
		}
		if strings.ContainsRune(d.glyph, '\uFE0F') {
			t.Errorf("%q glyph %q carries U+FE0F; emoji presentation is not allowed", d.label, d.glyph)
		}
	}
}

// TestStatusDetailHitRateGuard pins the cache-hit invariant: a cache read always
// follows a cache build (uncached input or a write), so a model whose window
// records hits but no build is refused instead of read as a false 100%. The
// guard also keeps rounding from reaching 100%.
func TestStatusDetailHitRateGuard(t *testing.T) {
	cases := []struct {
		name   string
		tokens promptTokens
		want   string
	}{
		{"hits without a build", promptTokens{cacheRead: 3_800_000}, "unknown"},
		{"hits with a build", promptTokens{input: 1, cacheRead: 10_000_000}, "99.9%"},
		{"only output", promptTokens{output: 732}, "-"},
		{"no hits", promptTokens{input: 1000}, "0.0%"},
	}
	for _, c := range cases {
		if got := hitRateCell(&modelUsage{tokens: c.tokens}); got != c.want {
			t.Errorf("%s: hitRateCell = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestStatusDetailTokenRatios pins the summary's input:output ratios: every
// share divides the recorded output, so the group is additive (total = cached +
// uncached), and a worktree with no output shows no ratio rows at all — a ratio
// without a denominator is undefined.
func TestStatusDetailTokenRatios(t *testing.T) {
	// context = 5.3M + 4.0M + 0.7M = 10.0M; output = 0.5M.
	u := pricedTokens(promptTokens{input: 5_300_000, cacheRead: 4_000_000, cacheWrite5m: 700_000, output: 500_000})
	rows := detailSummaryRows(worktree{Path: "/wt", Usage: u}, at(0))
	want := [][]string{
		{"total in:out", "20.0:1"},
		{"cached in:out", "8.0:1"},
		{"uncached in:out", "12.0:1"},
	}
	got := rows[len(rows)-len(want):]
	for i := range want {
		if !slices.Equal(got[i], want[i]) {
			t.Errorf("ratio row %d = %q, want %q", i, got[i], want[i])
		}
	}
	if rows := detailSummaryRows(worktree{Path: "/wt", Usage: pricedTokens(promptTokens{input: 1000})}, at(0)); len(rows) != 4 {
		t.Errorf("rows without output = %d, want 4 (ratio group hidden):\n%q", len(rows), rows)
	}
	if rows := tokenRatioRows(promptTokens{output: 100}, false); len(rows) != 0 {
		t.Errorf("output-only rows = %q, want hidden (no input context)", rows)
	}
}

// TestStatusInputOutputRatioJSON pins the machine contract: the key carries all
// three shares, and a worktree that recorded no output omits the key entirely.
func TestStatusInputOutputRatioJSON(t *testing.T) {
	tokens := promptTokens{input: 5_300_000, cacheRead: 4_000_000, cacheWrite5m: 700_000, output: 500_000}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{
			{Path: "/ratio", Usage: pricedTokens(tokens)},
			{Path: "/no-output", Usage: pricedTokens(promptTokens{input: 1000})},
		}, at(0), 10*time.Hour, "/ratio"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	r := got[0].InputOutputRatio
	if r == nil || r.Total != 20 || r.Cached != 8 || r.Uncached != 12 {
		t.Errorf("inputOutputRatio = %+v, want total=20 cached=8 uncached=12", r)
	}
	if got[1].InputOutputRatio != nil {
		t.Errorf("no-output worktree must omit inputOutputRatio, got %+v", got[1].InputOutputRatio)
	}
	if inputOutputRatioJSON(pricedTokens(promptTokens{output: 100})) != nil {
		t.Errorf("output-only worktree must omit inputOutputRatio")
	}
	if !strings.Contains(out, `"inputOutputRatio"`) {
		t.Errorf("JSON must name the key:\n%s", out)
	}
}

// TestInputOutputRatioRoundingDrift pins the documented bound (see
// jsonInputOutputRatio): the three shares divide one output, so
// total = cached + uncached holds before rounding, and after each field's
// independent 1e-6 trim the rounded values can differ by at most 1e-6.
// Repeating decimals are the adversarial mixes: they maximize trim error.
func TestInputOutputRatioRoundingDrift(t *testing.T) {
	for _, tc := range []struct {
		name string
		tk   promptTokens
	}{
		{"thirds", promptTokens{input: 2, cacheRead: 1, output: 3}},
		{"sevenths", promptTokens{input: 5, cacheRead: 2, cacheWrite5m: 3, output: 7}},
		{"large", promptTokens{input: 1_000_003, cacheRead: 1_000_001, output: 3_000_007}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStdout(t, func() {
				if err := printStatusJSON([]worktree{
					{Path: "/ratio", Usage: pricedTokens(tc.tk)},
				}, at(0), 10*time.Hour, "/ratio"); err != nil {
					t.Errorf("printStatusJSON: %v", err)
				}
			})
			var got []jsonWorktree
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("invalid JSON: %v\n%s", err, out)
			}
			r := got[0].InputOutputRatio
			if r == nil {
				t.Fatal("ratios undefined, want defined")
			}
			// Compare in integer micro-units: the bound is exact in decimals,
			// and float64 representation would blur it by ~1e-16 relative.
			mu := func(v float64) int64 { return int64(math.Round(v * 1e6)) }
			drift := mu(r.Total) - (mu(r.Cached) + mu(r.Uncached))
			if drift < 0 {
				drift = -drift
			}
			if drift > 1 {
				t.Errorf("rounded drift %d micro-units exceeds 1e-6: %+v", drift, r)
			}
		})
	}
}

// TestSummaryFittedWidthEmpty pins the empty-rows guard: no rows means no
// value width, never a panic.
func TestSummaryFittedWidthEmpty(t *testing.T) {
	if got := summaryFittedWidth(nil, 80); got != 0 {
		t.Errorf("summaryFittedWidth(nil) = %d, want 0", got)
	}
	if got := summaryHiddenGroups(nil, 80); len(got) != 0 {
		t.Errorf("summaryHiddenGroups(nil) = %q, want empty", got)
	}
}

// TestCostLegendSplitsTableAndDetail pins the per-block ownership: table marks
// print under the table, detail marks under the breakdown, never crossed.
// TestCostLegendSplitsTableAndDetail pins the per-block ownership: table
// marks print under the table, detail marks under the breakdown, never crossed.
// When the current worktree is absent ("/elsewhere"), printStatusDetail is a
// no-op, so no detail legend is produced — the table legend stands alone.
func TestCostLegendSplitsTableAndDetail(t *testing.T) {
	now := at(0)
	env := sandbox(t)
	tableWT := worktree{Path: filepath.Join(env.Home, "wt-table"), Usage: estimatedUsage()}
	cols := statusSchema(false, []worktree{tableWT})
	l := costLegendNeeded(cols, []worktree{tableWT}, "/elsewhere", now, 10*time.Hour, 0)
	if !l.tableReasons.any() {
		t.Fatal("estimated table total must set tableReasons")
	}
	// The current worktree is not in the list, so printStatusDetail is a no-op.
	if out := captureStdout(t, func() { printStatusDetail([]worktree{tableWT}, "/elsewhere", now, colorNever, 0) }); out != "" {
		t.Fatalf("absent current worktree must print no detail block, got:\n%q", out)
	}
}

// TestStatusPartialJSONFlag pins the machine disclosure for partial totals:
// a tainted total carries partial:true, while a whole total omits the key.
func TestStatusPartialJSONFlag(t *testing.T) {
	var u worktreeUsage
	u.addSession("pi", []usageEvent{{at: at(0), model: "m", cost: fptr(1)}}, pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		}
		return m
	}))
	markPartial(&u, map[string]bool{"pi": true})
	partial := worktree{Path: "/partial", Usage: &u}
	whole := worktree{Path: "/whole", Usage: fixtureUsage()}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{partial, whole}, at(0), 10*time.Hour, "/whole"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if !got[0].Partial {
		t.Error("partial total lost its flag")
	}
	if got[1].Partial {
		t.Error("whole total must not carry partial")
	}
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if raw[0]["partial"] != true {
		t.Errorf("partial key = %v, want true", raw[0]["partial"])
	}
	if _, ok := raw[1]["partial"]; ok {
		t.Error("whole total must omit the partial key")
	}
	// The loss reaches the harness and model levels too, not just the worktree.
	pi := got[0].Usage["pi"]
	if !pi.Partial {
		t.Error("harness partial flag lost")
	}
	if m := pi.Models["m"]; !m.Partial {
		t.Error("model partial flag lost")
	}
	if m := pi.Models["m"]; m.Cost != nil {
		t.Errorf("affected model cost = %v, want null (unknown)", *m.Cost)
	}
}

// TestStatusPartialDerivedValuesAreMarked pins the mixed-loss contract: readable
// values are lower-bound estimates, not exact, and every affected level says so.
func TestStatusPartialDerivedValuesAreMarked(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	writeFile(t, good, `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"input":100,"output":10,"cacheRead":20,"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
`, 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: piAgent.name},
		{path: good, harness: piAgent.name},
	}}
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err == nil {
		t.Fatal("buildUsage must report the unreadable session")
	}
	wt := worktree{Path: dir, Usage: &u}
	if got := cellActive(wt, columnContext{}); got != "~10s" {
		t.Errorf("active = %q, want ~10s", got)
	}
	if got := cellCost(wt, columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
	wantRow := []string{"m", "~10s", "unknown", "~120", "~10", "~20", "unknown", "~100", "~16.7%"}
	if got := detailRows(&u, false)[0]; !slices.Equal(got, wantRow) {
		t.Errorf("detail row = %q, want %q", got, wantRow)
	}
	rows := detailSummaryRows(wt, at(0))
	wantRatios := [][]string{{"total in:out", "~12.0:1"}, {"cached in:out", "~2.0:1"}, {"uncached in:out", "~10.0:1"}}
	gotRatios := rows[len(rows)-len(wantRatios):]
	for i := range wantRatios {
		if !slices.Equal(gotRatios[i], wantRatios[i]) {
			t.Errorf("ratio row %d = %q, want %q", i, gotRatios[i], wantRatios[i])
		}
	}
	var j jsonWorktree
	j.InputOutputRatio = inputOutputRatioJSON(&u)
	applyUsageJSON(&j, &u)
	if !j.Partial || j.ActiveSeconds == nil || *j.ActiveSeconds != 10 {
		t.Fatalf("worktree JSON = %+v, want partial with activeSeconds 10", j)
	}
	pi := j.Usage[piAgent.name]
	if !pi.Partial {
		t.Error("harness partial flag lost")
	}
	m := pi.Models["m"]
	if !m.Partial || m.Cost != nil || m.CacheHitRate == nil || m.CacheMissTokens == nil {
		t.Fatalf("model JSON = %+v, want partial, null cost, cache metrics", m)
	}
	if j.InputOutputRatio == nil || !j.InputOutputRatio.Partial {
		t.Fatalf("ratio JSON = %+v, want partial shares", j.InputOutputRatio)
	}
}

// TestLoadUsageKeepsAllUnreadable pins the disclosure fix: a worktree whose
// every session is unreadable keeps its usage so the loss reads as unknown
// with the partial flag, never as no usage.
func TestLoadUsageKeepsAllUnreadable(t *testing.T) {
	dir := t.TempDir()
	ws := workspace{wts: []worktree{{Path: dir}}, idx: sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: "pi"},
	}}}
	captureStderr(t, func() { ws.loadUsage(nil) })
	u := ws.wts[0].Usage
	if u == nil {
		t.Fatal("partial usage dropped: total loss reads as no usage")
	}
	if got := cellCost(ws.wts[0], columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
	if got := cellActive(ws.wts[0], columnContext{}); got != "unknown" {
		t.Errorf("active = %q, want unknown (not a measured 0s)", got)
	}
	var j jsonWorktree
	applyUsageJSON(&j, u)
	if !j.Partial {
		t.Error("partial flag lost on total loss")
	}
	if j.ActiveSeconds != nil {
		t.Errorf("activeSeconds = %v, want omitted on total loss", *j.ActiveSeconds)
	}
	if j.Usage != nil {
		t.Errorf("usage = %+v, want omitted on total loss", j.Usage)
	}
}

// routes each through the right pricer: Pi's own cost and Claude's tokens.
func TestBuildUsagePricesHarnesses(t *testing.T) {
	dir := t.TempDir()
	piFile := filepath.Join(dir, "pi.jsonl")
	writeFile(t, piFile, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1.5}}}}
`, 0o644)
	claudeFile := filepath.Join(dir, "claude.jsonl")
	writeFile(t, claudeFile, `{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","requestId":"r","message":{"id":"x","model":"m1","usage":{"input_tokens":100000,"output_tokens":0,"service_tier":"standard"}}}
`, 0o644)
	cat := testRates(t)
	wt := dir
	idx := sessionIndex{canonical(wt): {
		{path: piFile, harness: "pi"},
		{path: claudeFile, harness: "claude"},
	}}
	u, err := buildUsage(wt, idx, cat, subscriptions{})
	if err != nil {
		t.Fatalf("buildUsage: %v", err)
	}
	if got := u.harnesses["pi"].cost.cell(); got != "$1.50" {
		t.Errorf("pi cost = %q, want $1.50", got)
	}
	// 100k input tokens at m1's standard input rate (1e-6).
	if got := u.harnesses["claude"].cost.cell(); got != "$0.10" {
		t.Errorf("claude cost = %q, want $0.10", got)
	}
}

// TestBuildUsageMissingCatalogIsUnknown pins graceful degradation: a Claude
// session priced without a catalog is unknown, never silently zero.
func TestBuildUsageMissingCatalogIsUnknown(t *testing.T) {
	dir := t.TempDir()
	claudeFile := filepath.Join(dir, "claude.jsonl")
	writeFile(t, claudeFile, `{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","requestId":"r","message":{"id":"x","model":"m1","usage":{"input_tokens":100,"service_tier":"standard"}}}
`, 0o644)
	idx := sessionIndex{canonical(dir): {{path: claudeFile, harness: "claude"}}}
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.harnesses["claude"].cost.cell(); got != "unknown" {
		t.Errorf("claude cost = %q, want unknown", got)
	}
}

func TestWorktreeCostMoney(t *testing.T) {
	if got := (worktree{}).cost().cell(); got != "-" {
		t.Errorf("eventless cost = %q, want -", got)
	}
	w := worktree{Usage: fixtureUsage()}
	if got := w.cost().cell(); got != "$4.00" {
		t.Errorf("cost = %q, want $4.00", got)
	}
}

// TestWorktreeZeroCostIsKnown pins the design rule: a reported zero is a known
// $0.00, never unknown.
func TestWorktreeZeroCostIsKnown(t *testing.T) {
	var u worktreeUsage
	u.addSession("pi", []usageEvent{{at: at(0), model: "m"}}, pairPricer(func(usageEvent) money {
		var m money
		m.addKnown(0)
		return m
	}))
	w := worktree{Usage: &u}
	if got := w.cost().cell(); got != "$0.00" {
		t.Errorf("zero cost = %q, want $0.00", got)
	}
	if v := w.cost().billed.jsonValue(); v == nil || *v != 0 {
		t.Errorf("zero cost json = %v, want 0", v)
	}
}

// TestDetailBlockUnknownCost pins the unpriced breakdown row: a harness with
// events but no price reads "unknown", not "-" and not a number.
func TestDetailBlockUnknownCost(t *testing.T) {
	var u worktreeUsage
	u.addSession("claude", []usageEvent{{at: at(0), model: "claude-sonnet-4-6"}}, pairPricer(func(usageEvent) money {
		var m money
		m.addUnknown()
		return m
	}))
	w := worktree{Path: "/w", Usage: &u}
	out := captureStdout(t, func() { printStatusDetail([]worktree{w}, "/w", at(0), colorNever, 0) })
	if !strings.Contains(out, "unknown") {
		t.Errorf("unpriced detail row must read unknown:\n%s", out)
	}
	if !strings.Contains(out, "claude-sonnet-4-6") {
		t.Errorf("model row missing:\n%s", out)
	}
}

// TestCellActiveZeroSpanIsZeroSeconds pins the design rule: "-" is only for a
// worktree with no events. A point event spans zero time, which is a measured
// "0s", so real activity must not hide behind "-".
func TestCellActiveZeroSpanIsZeroSeconds(t *testing.T) {
	var u worktreeUsage
	u.addSession("pi", []usageEvent{{at: at(0), model: "m"}}, pairPricer(func(usageEvent) money {
		var m money
		m.addKnown(0)
		return m
	}))
	if got := cellActive(worktree{Path: "/w", Usage: &u}, columnContext{}); got != "0s" {
		t.Errorf("zero-span active time = %q, want 0s", got)
	}
	if got := cellActive(worktree{Path: "/w"}, columnContext{}); got != "-" {
		t.Errorf("no-event active time = %q, want -", got)
	}
}

// TestCellAgeUnknownMark pins the absent-creation-mark contract: AGE reads "-"
// and the JSON age is null, never a real zero.
func TestCellAgeUnknownMark(t *testing.T) {
	now := at(0).Add(time.Hour)
	if got := cellAge(worktree{}, columnContext{now: now}); got != "-" {
		t.Errorf("unknown creation mark age = %q, want -", got)
	}
	if got := ageSeconds(worktree{}, now); got != nil {
		t.Errorf("unknown age seconds = %v, want nil", got)
	}
}

// TestBuildUsageUnionsOverlappingSessions pins the harness-level union across
// sessions: two concurrent sessions count one wall-clock span, not their sum.
func TestBuildUsageUnionsOverlappingSessions(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.jsonl")
	writeFile(t, a, `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
`, 0o644)
	b := filepath.Join(dir, "b.jsonl")
	writeFile(t, b, `{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-01T00:00:20Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
`, 0o644)
	idx := sessionIndex{canonical(dir): {{path: a, harness: "pi"}, {path: b, harness: "pi"}}}
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.active(); got != 20*time.Second {
		t.Errorf("union active = %v, want 20s", got)
	}
}

// TestBuildUsageUnreadableSessionIsUnknown pins that an unreadable session
// makes the harness cost unknown without dropping the sessions that did load.
func TestBuildUsageUnreadableSessionIsUnknown(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good.jsonl")
	writeFile(t, good, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1.5}}}}
`, 0o644)
	spanned := filepath.Join(dir, "spanned.jsonl")
	writeFile(t, spanned, `{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-01T00:00:20Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
`, 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: "pi"},
		{path: good, harness: "pi"},
		{path: spanned, harness: "pi"},
	}}
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err == nil {
		t.Fatal("buildUsage must report the unreadable session")
	}
	if got := u.cost().cell(); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
	if got := u.active(); got != 10*time.Second {
		t.Errorf("active = %v, want 10s: a later session must survive the error", got)
	}
}

// TestPartialProvenHarnessCell pins the unknown/unknown collapse: a proven
// subscription harness marked partial renders as "unknown", not "unknown/unknown".
func TestPartialProvenHarnessCell(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("openai-codex", 10))
	markPartial(&u, map[string]bool{"pi": true})
	wt := worktree{Path: "/test", Usage: &u}
	if got := cellCost(wt, columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
}

// TestStatusPartialZeroStaysUnknown pins unknown != zero: when a lost total has
// no readable token or span count, the cell must not read an approximate zero.
func TestStatusPartialZeroStaysUnknown(t *testing.T) {
	m := &modelUsage{partial: true, cost: costPair{billed: knownZero()}}
	if got := totalInputCell(m); got != "unknown" {
		t.Errorf("in = %q, want unknown", got)
	}
	if got := tokenCount(m, tierOutput); got != "unknown" {
		t.Errorf("out = %q, want unknown", got)
	}
	if got := missCount(m); got != "unknown" {
		t.Errorf("miss = %q, want unknown", got)
	}
	if got := hitRateCell(m); got != "unknown" {
		t.Errorf("hit%% = %q, want unknown", got)
	}
	rows := detailRows(&worktreeUsage{harnesses: map[string]*harnessUsage{
		"pi": {models: map[string]*modelUsage{"m": m}},
	}}, false)
	if got := rows[0][1]; got != "unknown" {
		t.Errorf("model time = %q, want unknown", got)
	}
	if rows := tokenRatioRows(promptTokens{}, true); len(rows) != len(detailRatioMeta) || rows[0][1] != "unknown" {
		t.Errorf("partial undefined ratios = %q, want unknown rows", rows)
	}
}

// TestStatusPartialLegendNeedsRenderedMarker pins the anti-dangle rule: a total
// loss renders "unknown", not "~", so no legend explains a missing marker.
func TestStatusPartialLegendNeedsRenderedMarker(t *testing.T) {
	lost := worktree{Path: "/lost", Usage: &worktreeUsage{partial: true}}
	if anyPartialActive([]worktree{lost}) {
		t.Error("a total loss must not claim a partial ACTIVE marker")
	}
	lostWts := []worktree{lost}
	lostCols := statusSchema(false, lostWts)
	lostLegend := costLegendNeeded(lostCols, lostWts, "", at(0), 0, 0)
	if lostLegend.partial {
		t.Error("table legend must not print a partial clause without a marker")
	}
	if got := captureStdout(t, func() { printCostLegend(lostLegend, colorNever, 0) }); strings.Contains(got, partialLabel) {
		t.Errorf("table legend printed a dangling partial clause:\n%s", got)
	}
	lostFit := fitDetailBlock(lost, at(0), 0)
	if lostFit.partial {
		t.Error("detail legend must not print a partial clause without a marker")
	}
	if got := captureStdout(t, func() { printDetailLegend(estimateReasons{}, false, lostFit.partial, false, colorNever, 0) }); strings.Contains(got, partialLabel) {
		t.Errorf("detail legend printed a dangling partial clause:\n%s", got)
	}

	mixed := worktree{Path: "/mixed", Usage: &worktreeUsage{
		partial: true,
		harnesses: map[string]*harnessUsage{"pi": {
			partial: true,
			spans:   []span{{start: at(0), end: at(10)}},
			models: map[string]*modelUsage{"m": {
				partial: true,
				spans:   []span{{start: at(0), end: at(10)}},
			}},
		}},
	}}
	mixedWts := []worktree{mixed}
	if !anyPartialActive(mixedWts) {
		t.Error("a readable partial span must claim the ACTIVE marker")
	}
	mixedCols := statusSchema(false, mixedWts)
	mixedLegend := costLegendNeeded(mixedCols, mixedWts, "", at(0), 0, 0)
	if !mixedLegend.partial {
		t.Error("table legend must explain the rendered partial marker")
	}
	if got := captureStdout(t, func() { printCostLegend(mixedLegend, colorNever, 0) }); !strings.Contains(got, partialLabel) {
		t.Errorf("table legend missing the partial clause:\n%s", got)
	}
	mixedFit := fitDetailBlock(mixed, at(0), 0)
	if !mixedFit.partial {
		t.Error("detail legend must explain the rendered partial marker")
	}
	if got := captureStdout(t, func() { printDetailLegend(estimateReasons{}, false, mixedFit.partial, false, colorNever, 0) }); !strings.Contains(got, partialLabel) {
		t.Errorf("detail legend missing the partial clause:\n%s", got)
	}
}

// TestStatusPartialDetailLegendCoversTokenAndRatioMarkers pins the other detail
// marker sources: a partial token cell and a partial ratio row each need the
// legend even when no readable span exists.
func TestStatusPartialDetailLegendCoversTokenAndRatioMarkers(t *testing.T) {
	cases := map[string]promptTokens{
		"token": {input: 1},
		"ratio": {input: 1, output: 1},
	}
	for name, tokens := range cases {
		t.Run(name, func(t *testing.T) {
			u := worktreeUsage{partial: true, harnesses: map[string]*harnessUsage{"pi": {
				partial: true,
				models: map[string]*modelUsage{"m": {
					partial: true,
					tokens:  tokens,
				}},
			}}}
			w := worktree{Path: "/wt", Usage: &u}
			if f := fitDetailBlock(w, at(0), 0); !f.partial {
				t.Errorf("detail legend missing a rendered %s marker", name)
			}
		})
	}
}

// TestStatusPartialZeroSecondsJSONStaysQualified pins the machine contract for a
// partial harness or model whose readable activity is zero: activeSeconds:0 is
// kept, and partial:true marks it as a lower bound.
func TestStatusPartialZeroSecondsJSONStaysQualified(t *testing.T) {
	u := worktreeUsage{partial: true, harnesses: map[string]*harnessUsage{"pi": {
		partial: true,
		models: map[string]*modelUsage{"m": {
			partial: true,
			cost:    costPair{billed: knownZero()},
		}},
	}}}
	var j jsonWorktree
	applyUsageJSON(&j, &u)
	if !j.Partial || j.ActiveSeconds == nil || *j.ActiveSeconds != 0 {
		t.Fatalf("worktree JSON = %+v, want partial with activeSeconds 0", j)
	}
	h := j.Usage["pi"]
	if !h.Partial || h.ActiveSeconds != 0 {
		t.Errorf("harness JSON = %+v, want partial with activeSeconds 0", h)
	}
	m := h.Models["m"]
	if !m.Partial || m.ActiveSeconds != 0 {
		t.Errorf("model JSON = %+v, want partial with activeSeconds 0", m)
	}
}

// TestStatusJSONTiers pins the machine contract for the per-tier breakdown: a
// model with recorded tokens carries a tiers map with the exact counts and the
// billed/list cost keys the table shows, so a consumer can read the same
// breakdown without parsing the table.
func TestStatusJSONTiers(t *testing.T) {
	var u worktreeUsage
	price := usagePricer(agent{name: "pi"}, nil, subscriptions{})
	u.addSession("pi", []usageEvent{{
		at: at(0), model: "m", cost: fptr(0.30),
		tokens:   promptTokens{input: 1000, output: 500, cacheRead: 2000, cacheWrite5m: 100},
		tierCost: &tokenCosts{input: fptr(0.10), output: fptr(0.15), cacheRead: fptr(0.04), cacheWrite: fptr(0.01)},
	}}, price)
	w := worktree{Path: "/w", Usage: &u}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{w}, at(0), 10*time.Hour, "/w"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	tiers := got[0].Usage["pi"].Models["m"].Tiers
	if len(tiers) != int(numTiers) {
		t.Fatalf("tiers = %d keys, want %d\n%s", len(tiers), numTiers, out)
	}
	if in := tiers["input"]; in.Tokens != 1000 || in.Cost == nil || *in.Cost != 0.10 {
		t.Errorf("input tier = %+v, want 1000 tokens @ 0.10", in)
	}
	if cw := tiers["cacheWrite"]; cw.Tokens != 100 {
		t.Errorf("cacheWrite tier = %+v, want 100 tokens", cw)
	}
	if got := tiers["cacheRead"].Tokens; got != 2000 {
		t.Errorf("cacheRead tokens = %d, want 2000", got)
	}
	m := got[0].Usage["pi"].Models["m"]
	if m.CacheMissTokens == nil || *m.CacheMissTokens != 1100 {
		t.Errorf("cacheMissTokens = %v, want 1100", m.CacheMissTokens)
	}
	if m.CacheHitRate == nil || *m.CacheHitRate != 0.645161 {
		t.Errorf("cacheHitRate = %v, want 0.645161", m.CacheHitRate)
	}
}

// TestStatusJSONCacheMetricsOmitted pins when the derived cache keys are absent:
// a model with no prompt tokens omits both, and a model whose hits have no
// recorded build omits only the rate, so the contract never carries a 100%.
func TestStatusJSONCacheMetricsOmitted(t *testing.T) {
	var u worktreeUsage
	price := usagePricer(agent{name: "pi"}, nil, subscriptions{})
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "notokens", cost: fptr(0.10)},
		{at: at(0), model: "outputonly", cost: fptr(0.10), tokens: promptTokens{output: 500}},
		{at: at(0), model: "nobuild", cost: fptr(0.10), tokens: promptTokens{cacheRead: 5000}},
	}, price)
	w := worktree{Path: "/w", Usage: &u}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{w}, at(0), 10*time.Hour, "/w"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	models := got[0].Usage["pi"].Models
	for _, name := range []string{"notokens", "outputonly"} {
		if m := models[name]; m.CacheMissTokens != nil || m.CacheHitRate != nil {
			t.Errorf("%s: cache metrics present without prompt tokens: %+v", name, m)
		}
	}
	if m := models["nobuild"]; m.CacheMissTokens == nil || *m.CacheMissTokens != 0 || m.CacheHitRate != nil {
		t.Errorf("nobuild = %+v, want miss 0 and no rate", m)
	}
}

// TestStatusJSONCacheKeysConsumer pins the raw machine keys a consumer reads:
// a token-bearing model carries tiers/cacheMissTokens/cacheHitRate, a model
// with no tokens omits all three, and hits without a build keep the miss
// count but omit the rate, so no consumer ever reads a false 100%.
func TestStatusJSONCacheKeysConsumer(t *testing.T) {
	var u worktreeUsage
	price := usagePricer(agent{name: "pi"}, nil, subscriptions{})
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "withtokens", cost: fptr(0.30), tokens: promptTokens{input: 1000, cacheRead: 2000}},
		{at: at(0), model: "notokens", cost: fptr(0.10)},
		{at: at(0), model: "nobuild", cost: fptr(0.10), tokens: promptTokens{cacheRead: 5000}},
	}, price)
	w := worktree{Path: "/w", Usage: &u}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{w}, at(0), 10*time.Hour, "/w"); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	models := raw[0]["usage"].(map[string]any)["pi"].(map[string]any)["models"].(map[string]any)
	// Tier keys are the SSOT names, so a rename moves both sides together.
	wantTiers := map[string]bool{}
	for _, k := range tierMeta {
		wantTiers[k] = true
	}
	with := models["withtokens"].(map[string]any)
	if tiers, ok := with["tiers"].(map[string]any); !ok {
		t.Errorf("withtokens tiers missing:\n%s", out)
	} else {
		for k := range tiers {
			if !wantTiers[k] {
				t.Errorf("tiers key %q not in tierMeta", k)
			}
		}
		for k := range wantTiers {
			if _, ok := tiers[k]; !ok {
				t.Errorf("tiers key %q missing", k)
			}
		}
	}
	for _, k := range []string{"cacheMissTokens", "cacheHitRate"} {
		if _, ok := with[k]; !ok {
			t.Errorf("withtokens %s missing", k)
		}
	}
	bare := models["notokens"].(map[string]any)
	for _, k := range []string{"tiers", "cacheMissTokens", "cacheHitRate"} {
		if _, ok := bare[k]; ok {
			t.Errorf("notokens must omit %s", k)
		}
	}
	nobuild := models["nobuild"].(map[string]any)
	if _, ok := nobuild["cacheMissTokens"]; !ok {
		t.Error("nobuild must keep cacheMissTokens")
	}
	if _, ok := nobuild["cacheHitRate"]; ok {
		t.Error("nobuild must omit cacheHitRate (hits with no build)")
	}
}

// TestListColumnFollowsTheData pins the data-dependent schema: the LIST column
// exists only when a row carries a list price, each row's cell is that row's own
// list price or a dash, and the billed COST cell never packs the pair.
func TestListColumnFollowsTheData(t *testing.T) {
	proven := worktree{Path: "/sub", Usage: subscriptionUsage(at(0), 41.20)}
	metered := worktree{Path: "/pay", Usage: fixtureUsage()}
	with, without := statusSchema(false, []worktree{proven, metered}), statusSchema(false, []worktree{metered})
	if !slices.Contains(columnHeaders(with), colList.header) {
		t.Errorf("a proven subscription must add the LIST column: %v", columnHeaders(with))
	}
	if slices.Contains(columnHeaders(without), colList.header) {
		t.Errorf("a table of single prices must not carry a LIST column: %v", columnHeaders(without))
	}
	if cols, _ := detailSchema(proven.Usage); !slices.Contains(columnHeaders(cols), colList.header) {
		t.Errorf("a model list price must add the breakdown's LIST column: %v", columnHeaders(cols))
	}
	if got := cellCost(proven, columnContext{}); got != "$0.00" {
		t.Errorf("proven billed cell = %q, want $0.00", got)
	}
	if got := cellList(proven, columnContext{}); got != "$41.20" {
		t.Errorf("proven list cell = %q, want $41.20", got)
	}
	if got := cellList(metered, columnContext{}); got != "-" {
		t.Errorf("metered list cell = %q, want -", got)
	}
}

// subscriptionUsage is one priced worktree whose pair proves a subscription:
// billed $0 metered $list, so the LIST column has a value to carry.
func subscriptionUsage(now time.Time, list float64) *worktreeUsage {
	var u worktreeUsage
	u.addSession("pi", []usageEvent{{at: now, model: "m"}}, func(usageEvent) pricedEvent {
		var l, billed money
		l.addKnown(list)
		billed.addKnown(0)
		return pricedEvent{total: costPair{list: l, billed: billed, subscription: true}}
	})
	return &u
}

// TestTiersJSONSubscriptionListCost pins the tier-level list contract: a proven
// subscription tier carries both its priced $0 billed cost and its list price,
// while an unproven tier omits listCost even when its list side is priced.
func TestTiersJSONSubscriptionListCost(t *testing.T) {
	var m modelUsage
	m.tokens = promptTokens{input: 100}
	m.tiers[tierInput].billed = knownZero()
	m.tiers[tierInput].list.addKnown(0.30)
	m.tiers[tierInput].subscription = true
	m.tiers[tierOutput].list.addKnown(0.50)

	out := tiersJSON(m.tokens, m.tiers)
	in, ok := out["input"]
	if !ok {
		t.Fatalf("input tier missing: %+v", out)
	}
	if in.Cost == nil || *in.Cost != 0 {
		t.Errorf("input cost = %v, want the proven 0", in.Cost)
	}
	if in.ListCost == nil || in.ListCost.v == nil || *in.ListCost.v != 0.30 {
		t.Errorf("input listCost = %+v, want 0.30", in.ListCost)
	}
	if o, ok := out["output"]; !ok {
		t.Fatalf("output tier missing: %+v", out)
	} else if o.ListCost != nil {
		t.Errorf("unproven output listCost = %+v, want nil", o.ListCost)
	}
}

// TestHitRateCappedBelowOne pins the no-false-100% cap: a window that rounds to
// a full cache share is clipped at maxHitRate, so its percent and its machine
// rate stay below 1.
func TestHitRateCappedBelowOne(t *testing.T) {
	tokens := promptTokens{cacheRead: 2_000_000, input: 1}
	rate, ok := tokens.hitRate()
	if !ok {
		t.Fatal("hitRate undefined for a window with a build")
	}
	if rate > maxHitRate {
		t.Errorf("hitRate = %v, want <= %v", rate, maxHitRate)
	}
	if got := formatPercent(rate); got != "99.9%" {
		t.Errorf("formatPercent(%v) = %q, want 99.9%%", rate, got)
	}
	_, ratePtr := cacheJSON(tokens)
	if ratePtr == nil {
		t.Fatal("cacheJSON rate missing")
	}
	if *ratePtr >= 1 {
		t.Errorf("cacheJSON rate = %v, want < 1", *ratePtr)
	}
}

// TestStatusDetailTokenLegendOnlyWithTokens pins the anti-dangle rule at the
// detail block: the token legend prints only when a model actually recorded
// tokens, never beside a breakdown whose token cells are all dashes.
func TestStatusDetailTokenLegendOnlyWithTokens(t *testing.T) {
	costOnly := worktreeUsage{harnesses: map[string]*harnessUsage{
		"pi": {models: map[string]*modelUsage{"m": {cost: costPair{billed: knownZero()}}}},
	}}
	withTokens := worktreeUsage{harnesses: map[string]*harnessUsage{
		"pi": {models: map[string]*modelUsage{"m": {cost: costPair{billed: knownZero()}, tokens: promptTokens{input: 100}}}},
	}}
	costOut := captureStdout(t, func() {
		printStatusDetail([]worktree{{Path: "/w", Usage: &costOnly}}, "/w", at(0), colorNever, 0)
	})
	if strings.Contains(costOut, tokenLegendTitle) {
		t.Errorf("cost-only detail must not print the token legend:\n%s", costOut)
	}
	tokOut := captureStdout(t, func() {
		printStatusDetail([]worktree{{Path: "/w", Usage: &withTokens}}, "/w", at(0), colorNever, 0)
	})
	if !strings.Contains(tokOut, tokenLegendTitle) {
		t.Errorf("token-bearing detail must print the token legend:\n%s", tokOut)
	}
}
