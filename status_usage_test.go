package main

import (
	"encoding/json"
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
		m.addEstimated(1.23)
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

// TestStatusEstimateLegend pins the reason-aware legend: each set reason prints
// its SSOT line, an unset reason stays silent, and no reason prints nothing.
// The asserts reference the constants, so prose edits move both sides together.
func TestStatusEstimateLegend(t *testing.T) {
	cacheOnly := captureStdout(t, func() { printEstimateLegend(estimateReasons{cache: true}, colorNever) })
	if !strings.Contains(cacheOnly, "~ estimated: "+estimateCacheWriteLifetime) {
		t.Fatalf("cache-only legend missing its line:\n%s", cacheOnly)
	}
	if strings.Contains(cacheOnly, estimateSubscriptionOverage) {
		t.Fatalf("cache-only legend must not explain overage:\n%s", cacheOnly)
	}
	overageOnly := captureStdout(t, func() { printEstimateLegend(estimateReasons{overage: true}, colorNever) })
	if !strings.Contains(overageOnly, "~ estimated: "+estimateSubscriptionOverage) {
		t.Fatalf("overage-only legend missing its line:\n%s", overageOnly)
	}
	if strings.Contains(overageOnly, estimateCacheWriteLifetime) {
		t.Fatalf("overage-only legend must not explain the cache split:\n%s", overageOnly)
	}
	both := captureStdout(t, func() { printEstimateLegend(estimateReasons{cache: true, overage: true}, colorNever) })
	if strings.Count(both, "~ estimated: ") != 2 {
		t.Fatalf("both-reasons legend must print 2 lines, got:\n%s", both)
	}
	if got := captureStdout(t, func() { printEstimateLegend(estimateReasons{}, colorNever) }); got != "" {
		t.Fatalf("no-estimate legend must be empty, got:\n%q", got)
	}
}

// TestEstimateLegendNeededFollowsRenderedCells pins the anti-dangle rule: the
// legend is needed only when a "~" cell actually renders, so a dropped COST
// column suppresses it. The estimated worktree is not the current one, so only
// the table can show the mark.
func TestEstimateLegendNeededFollowsRenderedCells(t *testing.T) {
	with := worktree{Path: "/with", Usage: estimatedUsage()}
	cols := statusColumnsFor(false)
	if r := estimateLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour); !r.any() {
		t.Fatal("an estimated table total must ask for the legend")
	}
	t.Setenv("COLUMNS", "5")
	if r := estimateLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour); r.any() {
		t.Fatal("a dropped COST column must not ask for the legend")
	}
}

// TestEstimateLegendSelectivity pins the reason-aware legend: a cache-only
// total prints one line, an overage-only total prints the other, both print
// two, and an exact total prints none.
func TestEstimateLegendSelectivity(t *testing.T) {
	cacheEst := func() money { var m money; m.addEstimated(1); return m }()
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
		{"overage-only", overageEst, false, true},
		{"exact", costPair{list: func() money { var m money; m.addKnown(1); return m }(), billed: func() money { var m money; m.addKnown(0); return m }(), subscription: true}, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := worktree{Path: "/with", Usage: pairUsage(tc.pair)}
			r := estimateLegendNeeded(cols, []worktree{w}, "/elsewhere", at(0), 10*time.Hour)
			if r.cache != tc.wantCache || r.overage != tc.wantOver {
				t.Fatalf("reasons = %+v, want cache=%v overage=%v", r, tc.wantCache, tc.wantOver)
			}
			out := captureStdout(t, func() { printEstimateLegend(r, colorNever) })
			wantLines := 0
			if tc.wantCache {
				wantLines++
			}
			if tc.wantOver {
				wantLines++
			}
			if got := strings.Count(out, "~ estimated: "); got != wantLines {
				t.Fatalf("legend lines = %d, want %d:\n%s", got, wantLines, out)
			}
		})
	}
	t.Run("both", func(t *testing.T) {
		both := overageEst
		both.list = cacheEst
		w := worktree{Path: "/with", Usage: pairUsage(both)}
		r := estimateLegendNeeded(cols, []worktree{w}, "/elsewhere", at(0), 10*time.Hour)
		if !r.cache || !r.overage {
			t.Fatalf("reasons = %+v, want both", r)
		}
		if out := captureStdout(t, func() { printEstimateLegend(r, colorNever) }); strings.Count(out, "~ estimated: ") != 2 {
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
	t.Setenv("COLUMNS", "0")
	with := worktree{Path: "/with", Usage: fixtureUsage()}
	empty := worktree{Path: "/empty"}
	other := worktree{Path: "/other", Usage: fixtureUsage()}

	// No harness anywhere has events: the block is omitted.
	if out := captureStdout(t, func() { printStatusDetail([]worktree{empty}, "/empty", at(0), colorNever) }); out != "" {
		t.Fatalf("block must be omitted when the repo has no usage:\n%s", out)
	}

	// The current worktree has events: a separated, titled block, then rows.
	out := captureStdout(t, func() { printStatusDetail([]worktree{with, other}, "/with", at(0), colorNever) })
	if !strings.HasPrefix(out, "\ncurrent worktree") {
		t.Errorf("block must open with a blank line, then the identity row:\n%q", out)
	}
	for _, want := range []string{"current worktree", "active time", "MODEL", "↑ in", "↓ out", "🎯 hit", "✍️ write", "✗ miss", "◉ hit%", "deepseek-flash", "grok-4.3", "$4.00"} {
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
	out = captureStdout(t, func() { printStatusDetail([]worktree{empty, other}, "/empty", at(0), colorNever) })
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
	if got, want := detailRows(&u)[0], []string{"m", "0s", "$1.00", "6.0M", "732", "-", "714.8k", "6.0M", "0.0%"}; !slices.Equal(got, want) {
		t.Errorf("detail row = %q, want %q", got, want)
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
}

// TestLoadUsageKeepsAllUnreadable pins the disclosure fix: a worktree whose
// every session is unreadable keeps its usage so the loss reads as unknown
// with the partial flag, never as no usage.
func TestLoadUsageKeepsAllUnreadable(t *testing.T) {
	dir := t.TempDir()
	ws := workspace{wts: []worktree{{Path: dir}}, idx: sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: "pi"},
	}}}
	captureStderr(t, func() { ws.loadUsage() })
	u := ws.wts[0].Usage
	if u == nil {
		t.Fatal("partial usage dropped: total loss reads as no usage")
	}
	if got := cellCost(ws.wts[0], columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
	var j jsonWorktree
	applyUsageJSON(&j, u)
	if !j.Partial {
		t.Error("partial flag lost on total loss")
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
	out := captureStdout(t, func() { printStatusDetail([]worktree{w}, "/w", at(0), colorNever) })
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

// TestAlignCostSubFieldsEstimateUnknown pins that alignment never mid-cuts
// pairs: estimated and unknown cells align with exact ones in one column.
func TestAlignCostSubFieldsEstimateUnknown(t *testing.T) {
	headers := []string{"MODEL", colCost.header}
	rows := [][]string{
		{"pair", "$0.00/$41.20"},
		{"est", "~$0.00/$41.20"},
		{"solo", "$12.34"},
		{"unk", "unknown"},
	}
	out := alignCostSubFields(headers, rows)
	if len(out) != 4 {
		t.Fatalf("aligned rows = %d, want 4", len(out))
	}
	if out[3][1] == "" {
		t.Error("unknown cell must survive alignment")
	}
	if out[1][1] == "" || !strings.Contains(out[1][1], "~$0.00") {
		t.Errorf("estimated cell lost its mark: %q", out[1][1])
	}
	if !strings.Contains(out[0][1], "/") || !strings.Contains(out[1][1], "/") {
		t.Errorf("pairs must keep separator: %q %q", out[0][1], out[1][1])
	}
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
	t.Setenv("COLUMNS", "0")
	costOnly := worktreeUsage{harnesses: map[string]*harnessUsage{
		"pi": {models: map[string]*modelUsage{"m": {cost: costPair{billed: knownZero()}}}},
	}}
	withTokens := worktreeUsage{harnesses: map[string]*harnessUsage{
		"pi": {models: map[string]*modelUsage{"m": {cost: costPair{billed: knownZero()}, tokens: promptTokens{input: 100}}}},
	}}
	costOut := captureStdout(t, func() {
		printStatusDetail([]worktree{{Path: "/w", Usage: &costOnly}}, "/w", at(0), colorNever)
	})
	if strings.Contains(costOut, "tokens: in") {
		t.Errorf("cost-only detail must not print the token legend:\n%s", costOut)
	}
	tokOut := captureStdout(t, func() {
		printStatusDetail([]worktree{{Path: "/w", Usage: &withTokens}}, "/w", at(0), colorNever)
	})
	if !strings.Contains(tokOut, "tokens: in") {
		t.Errorf("token-bearing detail must print the token legend:\n%s", tokOut)
	}
}
