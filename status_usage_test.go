package main

import (
	"encoding/json"
	"path/filepath"
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

// TestStatusEstimateLegend pins the one explanation: the legend states the SSOT
// reason and stays silent when every displayed price is exact. The assert
// references the constant, so prose edits move both sides together.
func TestStatusEstimateLegend(t *testing.T) {
	out := captureStdout(t, func() { printEstimateLegend(true) })
	if !strings.Contains(out, "~ estimated: "+estimateCacheWriteLifetime) {
		t.Fatalf("legend missing its mark or its reason:\n%s", out)
	}
	if got := captureStdout(t, func() { printEstimateLegend(false) }); got != "" {
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
	if !estimateLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour) {
		t.Fatal("an estimated table total must ask for the legend")
	}
	t.Setenv("COLUMNS", "5")
	if estimateLegendNeeded(cols, []worktree{with}, "/elsewhere", at(0), 10*time.Hour) {
		t.Fatal("a dropped COST column must not ask for the legend")
	}
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
	for _, want := range []string{"current worktree", "active time", "HARNESS", "deepseek-flash", "grok-4.3", "$4.00"} {
		if !strings.Contains(out, want) {
			t.Errorf("detail block missing %q:\n%s", want, out)
		}
	}
	// The summary labels keep their order above the breakdown grid.
	prev := 0
	for _, label := range []string{"\ncurrent worktree", "\nage", "\nactive time", "\ncost", "\nHARNESS"} {
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
	if strings.Contains(out, "HARNESS") {
		t.Errorf("empty current worktree must not print rows:\n%s", out)
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
