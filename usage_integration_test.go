package main

import (
	"encoding/json"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// usage_integration_test.go — loader→pricer→cell contracts the unit tables do
// not cover end to end: the unclassified cache write, the pricing modifiers,
// and a partial worktree whose unreadable harness must taint the total.

// claudeUsageLine builds one assistant record with the given usage object, so a
// test drives the real parser rather than a hand-built event.
func claudeUsageLine(usage string) string {
	return `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"m1","usage":` + usage + `}}`
}

// usageIndex writes one session owned by the given harness and returns an index
// that maps it to dir, the way the session walk would.
func usageIndex(t *testing.T, dir, name, harness, line string) sessionIndex {
	t.Helper()
	p := filepath.Join(dir, name)
	writeFile(t, p, line+"\n", 0o644)
	return sessionIndex{canonical(dir): {{path: p, harness: harness}}}
}

// TestBuildUsageClaudeUnclassifiedCacheWriteIsEstimated pins the end-to-end
// estimate: tokens with no 5m/1h lifetime are priced at the 5-minute rate, the
// cell carries the tilde, and the JSON keeps the number and sets the flag.
func TestBuildUsageClaudeUnclassifiedCacheWriteIsEstimated(t *testing.T) {
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name,
		claudeUsageLine(`{"input_tokens":1000,"cache_creation_input_tokens":100}`))
	u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := worktree{Path: dir, Usage: &u}
	if got := cellCost(w, columnContext{}); got != "~$0.00" {
		t.Errorf("cost = %q, want ~$0.00", got)
	}
	cost := w.cost()
	if v := cost.billed.jsonValue(); v == nil {
		t.Errorf("json cost = nil, want a number")
	}
	if !cost.billed.isEstimated() {
		t.Errorf("json cost missing the estimated flag: %+v", cost.billed)
	}
}

func TestBuildUsageClaudePricingModifiers(t *testing.T) {
	cases := map[string]struct {
		usage string
		want  float64
	}{
		"standard":     {`{"input_tokens":100000}`, 0.1},
		"batch tier":   {`{"input_tokens":100000,"service_tier":"batch"}`, 0.05},
		"long context": {`{"input_tokens":250000}`, 0.5},
		"web search":   {`{"input_tokens":0,"server_tool_use":{"web_search_requests":2}}`, 0.02},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(tc.usage))
			u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
			if err != nil {
				t.Fatal(err)
			}
			cost := u.cost()
			if !cost.known() {
				t.Fatalf("cost unknown, want %v", tc.want)
			}
			if diff := cost.billed.amount - tc.want; diff > 1e-9 || diff < -1e-9 {
				t.Errorf("cost = %v, want %v", cost.billed.amount, tc.want)
			}
		})
	}
}

// TestBuildUsageCrossHarnessPartialIsUnknown pins the invariant that decides
// whether a partial sum can masquerade as complete: an unreadable harness must
// taint the total even when it contributes no readable spans, while a readable
// harness keeps its own priced total.
func TestBuildUsageCrossHarnessPartialIsUnknown(t *testing.T) {
	dir := t.TempDir()
	readable := filepath.Join(dir, "c.jsonl")
	writeFile(t, readable, claudeUsageLine(`{"input_tokens":1000000}`)+"\n", 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: piAgent.name},
		{path: readable, harness: claudeAgent.name},
	}}
	u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
	if err == nil {
		t.Fatal("buildUsage must report the unreadable session")
	}
	if got := cellCost(worktree{Path: dir, Usage: &u}, columnContext{}); got != "unknown" {
		t.Errorf("total cost = %q, want unknown: a partial sum must not look complete", got)
	}
	h, ok := u.harnesses[claudeAgent.name]
	if !ok {
		t.Fatal("the readable harness was dropped")
	}
	if got := h.cost.cell(); got != "$2.00" {
		t.Errorf("claude cost = %q, want $2.00", got)
	}
	if h.partial {
		t.Error("a readable harness with no lost records must stay exact")
	}
	for name, m := range h.models {
		if m.partial {
			t.Errorf("model %q must stay exact when its harness lost nothing", name)
		}
	}
	var j jsonWorktree
	applyUsageJSON(&j, &u)
	jh := j.Usage[claudeAgent.name]
	if jh.Partial {
		t.Error("readable harness JSON must omit the partial flag")
	}
	for name, m := range jh.Models {
		if m.Partial {
			t.Errorf("readable model JSON %q must omit the partial flag", name)
		}
	}
}

// piUsageLine builds one self-priced Pi record with the given cost total, so a
// test drives the real parser rather than a hand-built event.
func piUsageLine(total float64) string {
	return fmt.Sprintf(`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":%v}}}}`, total)
}

// TestBuildUsageActivityOrderIndependent pins D1: activity time comes from the
// original events, so deduplicating a shared transcript never shrinks a session
// span. Both session orders yield the same active time and the same total.
func TestBuildUsageActivityOrderIndependent(t *testing.T) {
	piLine := func(id, ts string, cost float64) string {
		return fmt.Sprintf(`{"type":"message","id":%q,"timestamp":%q,"message":{"role":"assistant","model":"m","usage":{"cost":{"total":%v}}}}`, id, ts, cost)
	}
	bodyA := piLine("e0", "2026-01-01T00:00:00Z", 1) + "\n" + piLine("e1", "2026-01-01T00:00:10Z", 1) + "\n"
	bodyB := bodyA + piLine("e2", "2026-01-01T00:00:20Z", 1) + "\n"
	for _, order := range []string{"ab", "ba"} {
		t.Run(order, func(t *testing.T) {
			dir := t.TempDir()
			a := filepath.Join(dir, "a.jsonl")
			b := filepath.Join(dir, "b.jsonl")
			writeFile(t, a, bodyA, 0o644)
			writeFile(t, b, bodyB, 0o644)
			refs := []sessionRef{{path: a, harness: piAgent.name}, {path: b, harness: piAgent.name}}
			if order == "ba" {
				refs[0], refs[1] = refs[1], refs[0]
			}
			u, err := buildUsage(dir, sessionIndex{canonical(dir): refs}, nil, subscriptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := seconds(u.active()); got != 20 {
				t.Errorf("active = %ds, want 20s regardless of session order", got)
			}
			if h := u.harnesses[piAgent.name]; h == nil || seconds(h.active()) != 20 {
				t.Errorf("harness active = %v, want 20s", h)
			}
			if got := u.cost().billed.amount; math.Abs(got-3) > 1e-9 {
				t.Errorf("cost = %v, want 3 (each record billed once)", got)
			}
		})
	}
}

// TestBuildUsageDedupesPiForkAcrossSessionFiles pins that a resumed or forked
// Pi transcript prices once: identical content across files shares one key, so
// the second copy drops instead of doubling the total.
func TestBuildUsageDedupesPiForkAcrossSessionFiles(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.jsonl")
	pathB := filepath.Join(dir, "b.jsonl")
	writeFile(t, pathA, piUsageLine(1.5)+"\n", 0o644)
	writeFile(t, pathB, piUsageLine(1.5)+"\n", 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: pathA, harness: piAgent.name},
		{path: pathB, harness: piAgent.name},
	}}
	// Pi prices itself; no catalog is involved.
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.cost().cell(); got != "$1.50" {
		t.Errorf("total cost = %q, want $1.50: the repeated Pi record must price once", got)
	}
}

// TestBuildUsageAllUnreadableIsUnknown pins that total loss reads as unknown:
// with no readable spans the worktree keeps no harness, but the partial flag
// must still taint the total instead of reading as no usage.
func TestBuildUsageAllUnreadableIsUnknown(t *testing.T) {
	dir := t.TempDir()
	idx := sessionIndex{canonical(dir): {
		{path: filepath.Join(dir, "missing.jsonl"), harness: piAgent.name},
	}}
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err == nil {
		t.Fatal("buildUsage must report the unreadable session")
	}
	if u.hasEvents() {
		t.Fatal("an all-unreadable worktree must keep no harness")
	}
	if !u.partial {
		t.Error("partial flag lost: total loss must taint the total")
	}
	if got := u.cost().cell(); got != "unknown" {
		t.Errorf("total cost = %q, want unknown, never -", got)
	}
}

// a resume or fork: a worktree whose two session files repeat the same billed
// turn prices it once, at the most complete record.
func TestBuildUsageDedupesTurnAcrossSessionFiles(t *testing.T) {
	dir := t.TempDir()
	pathA := filepath.Join(dir, "a.jsonl")
	pathB := filepath.Join(dir, "b.jsonl")
	writeFile(t, pathA, claudeUsageLine(`{"input_tokens":100000}`)+"\n", 0o644)
	writeFile(t, pathB, claudeUsageLine(`{"input_tokens":100000}`)+"\n", 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: pathA, harness: claudeAgent.name},
		{path: pathB, harness: claudeAgent.name},
	}}
	u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	cost := u.cost()
	if !cost.known() {
		t.Fatalf("cost unknown, want a single turn's price")
	}
	if got := cost.cell(); got != "$0.10" {
		t.Errorf("total cost = %q, want $0.10: the repeated turn must price once", got)
	}
}

// TestBuildUsageDedupesProviderVariantTurns pins the provider-independent
// dedupe key: one turn repeated across two sessions with different provider
// evidence bills once in either session order, so the order never changes the
// total.
func TestBuildUsageDedupesProviderVariantTurns(t *testing.T) {
	for _, order := range []string{"ab", "ba"} {
		t.Run(order, func(t *testing.T) {
			dir := t.TempDir()
			body := func(provider string) string {
				return fmt.Sprintf(`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","provider":%q,"model":"m","usage":{"cost":{"total":7}}}}`+"\n", provider)
			}
			a := filepath.Join(dir, "a.jsonl")
			b := filepath.Join(dir, "b.jsonl")
			writeFile(t, a, body("p-a"), 0o644)
			writeFile(t, b, body("p-b"), 0o644)
			refs := []sessionRef{{path: a, harness: piAgent.name}, {path: b, harness: piAgent.name}}
			if order == "ba" {
				refs[0], refs[1] = refs[1], refs[0]
			}
			u, err := buildUsage(dir, sessionIndex{canonical(dir): refs}, nil, subscriptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got := u.cost().billed.amount; math.Abs(got-7) > 1e-9 {
				t.Errorf("cost = %v, want 7: a provider-variant turn must bill once", got)
			}
		})
	}
}

// TestBuildUsageBatchTierWith1hWriteIsUnknown pins 3B: the catalog has no
// batch 1-hour-write rate, so a batch-tier turn with 1h cache writes reads
// unknown rather than silently mispriced.
func TestBuildUsageBatchTierWith1hWriteIsUnknown(t *testing.T) {
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name,
		claudeUsageLine(`{"input_tokens":100,"service_tier":"batch","cache_creation":{"ephemeral_1h_input_tokens":50}}`))
	u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.cost().cell(); got != "unknown" {
		t.Fatalf("cost = %q, want unknown (no batch 1h-write rate)", got)
	}
	if v := u.cost().billed.jsonValue(); v != nil {
		t.Fatalf("json cost = %v, want null", *v)
	}
}

// TestBuildUsageActiveTimeSurvivesDedupe pins 3D: dedupe decides which copy of
// a billed turn prices, but activity time folds every event, so the billed mask
// never changes the active total while the cost drops the repeat.
func TestBuildUsageActiveTimeSurvivesDedupe(t *testing.T) {
	a := usageEvent{at: at(0), end: at(10), model: "m", key: "k", cost: fptr(1)}
	b := usageEvent{at: at(20), end: at(30), model: "m", key: "k", cost: fptr(1)}
	price := pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		} else {
			m.addUnknown()
		}
		return m
	})
	var whole, deduped worktreeUsage
	whole.addBilledSession("pi", []usageEvent{a, b}, nil, nil, price)
	masks := dedupeKeyedEvents([][]usageEvent{{a, b}})
	deduped.addBilledSession("pi", []usageEvent{a, b}, nil, masks[0], price)
	if whole.active() != deduped.active() {
		t.Fatalf("active = %v after dedupe, want %v (dedupe never changes time)", deduped.active(), whole.active())
	}
	if got := whole.cost().cell(); got != "$2.00" {
		t.Fatalf("whole cost = %q, want $2.00", got)
	}
	if got := deduped.cost().cell(); got != "$1.00" {
		t.Fatalf("deduped cost = %q, want $1.00", got)
	}
}

// TestBuildUsageBadTimestampWarnsAndPartials pins the never-silent contract: a
// billed record with a bad timestamp still drops, but the run warns once and
// the total reads partial instead of a smaller exact number.
func TestBuildUsageBadTimestampWarnsAndPartials(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	bad := `{"type":"message","timestamp":"nope","message":{"role":"assistant","model":"m","usage":{"cost":{"total":9}}}}`
	writeFile(t, session, piUsageLine(1.5)+"\n"+bad+"\n", 0o644)
	ws := workspace{
		wts: []worktree{{Path: dir}},
		idx: sessionIndex{canonical(dir): {{path: session, harness: piAgent.name}}},
	}
	errOut := captureStderr(t, func() { ws.loadUsage(nil) })
	if got := strings.Count(errOut, warnPrefix); got != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", got, errOut)
	}
	u := ws.wts[0].Usage
	if u == nil || !u.partial {
		t.Fatalf("dropped records must taint the total: %+v", u)
	}
	if got := cellCost(ws.wts[0], columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
}

// TestBuildUsageOversizedRecordWarnsAndPartials pins the disclosure end to end:
// an oversized billed record is counted, the total is partial, and the warning
// names the loss while a later good record still folds.
func TestBuildUsageOversizedRecordWarnsAndPartials(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	big := `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":9}},"pad":"` + strings.Repeat("a", maxSessionHeader) + `"}}`
	writeFile(t, session, big+"\n"+piUsageLine(1.5)+"\n", 0o644)
	ws := workspace{
		wts: []worktree{{Path: dir}},
		idx: sessionIndex{canonical(dir): {{path: session, harness: piAgent.name}}},
	}
	errOut := captureStderr(t, func() { ws.loadUsage(nil) })
	if !strings.Contains(errOut, "oversized") {
		t.Fatalf("warning must name the oversized loss:\n%s", errOut)
	}
	u := ws.wts[0].Usage
	if u == nil || !u.partial {
		t.Fatalf("oversized record must taint the total: %+v", u)
	}
	if h := u.harnesses[piAgent.name]; h == nil || len(h.models) == 0 {
		t.Fatalf("the good record after the loss must still fold: %+v", u)
	}
}

// TestBuildUsageOversizedUnbilledRecordStillDiscloses pins the conservative
// policy: after the discard the parser cannot know whether the record carried
// billed data, so an oversized user record still marks the total partial.
func TestBuildUsageOversizedUnbilledRecordStillDiscloses(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	big := `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"` + strings.Repeat("a", maxSessionHeader) + `"}}`
	writeFile(t, session, big+"\n"+piUsageLine(1.5)+"\n", 0o644)
	ws := workspace{
		wts: []worktree{{Path: dir}},
		idx: sessionIndex{canonical(dir): {{path: session, harness: piAgent.name}}},
	}
	errOut := captureStderr(t, func() { ws.loadUsage(nil) })
	if !strings.Contains(errOut, "oversized") {
		t.Fatalf("warning must name the oversized loss:\n%s", errOut)
	}
	u := ws.wts[0].Usage
	if u == nil || !u.partial {
		t.Fatalf("oversized record must taint the total: %+v", u)
	}
	if h := u.harnesses[piAgent.name]; h == nil || len(h.models) == 0 {
		t.Fatalf("the good record after the loss must still fold: %+v", u)
	}
}

// TestBuildUsageProvenPiPlusEstimatedClaudeAsksNoOverageLegend pins B1 end to
// end: an exact proven Pi charge beside an unproven Claude cache-write estimate
// carries the cache cause only, so the allowance caveat must not print.
func TestBuildUsageProvenPiPlusEstimatedClaudeAsksNoOverageLegend(t *testing.T) {
	sandbox(t)
	dir := t.TempDir()
	piPath := filepath.Join(dir, "p.jsonl")
	claudePath := filepath.Join(dir, "c.jsonl")
	writeFile(t, piPath, piProviderLine("openai-codex", 1)+"\n", 0o644)
	writeFile(t, claudePath, claudeUsageLine(`{"input_tokens":1000,"cache_creation_input_tokens":100}`)+"\n", 0o644)
	idx := sessionIndex{canonical(dir): {
		{path: piPath, harness: piAgent.name},
		{path: claudePath, harness: claudeAgent.name},
	}}
	u, err := buildUsage(dir, idx, testRates(t), loadSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	r := u.cost().reasons()
	if !r.cache {
		t.Errorf("reasons = %+v, want the cache-write caveat", r)
	}
	if r.overage {
		t.Errorf("reasons = %+v, want no allowance caveat: the estimate is a token split", r)
	}
}

// TestLoadUsagePiOnlyNeedsNoRatesCatalog pins T1: a repo with only Pi sessions
// resolves cost from the harness record without loading the catalog, so a
// missing WTM_RATES and an offline host change nothing.
func TestLoadUsagePiOnlyNeedsNoRatesCatalog(t *testing.T) {
	sandbox(t)
	// A missing pinned catalog makes any catalog load fail and warn, so this test
	// proves the loader is never reached instead of depending on network state.
	t.Setenv(ratesEnv, filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("WTM_NOW", "2026-01-01T01:00:00Z")
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	writeFile(t, path, piProviderLine("", 1.5)+"\n", 0o644)
	ws := workspace{
		wts: []worktree{{Path: dir}},
		idx: sessionIndex{canonical(dir): {{path: path, harness: piAgent.name}}},
	}
	errOut := captureStderr(t, func() { ws.loadUsage(nil) })
	if strings.Contains(errOut, "claude cost unavailable") {
		t.Fatalf("a Pi-only repo must not load the catalog:\n%s", errOut)
	}
	u := ws.wts[0].Usage
	if u == nil || !u.cost().billed.known() {
		t.Fatalf("Pi cost = %+v, want a resolved price", u)
	}
	if got := u.cost().cell(); got != "$1.50" {
		t.Errorf("cost = %q, want $1.50", got)
	}
}

// TestStatusClaudeCatalogOutageWarnsAndReportsUnknown pins the user-visible
// outage path: a missing catalog keeps status alive, warns, and reports unknown
// cost in both the table and the JSON contract.
func TestStatusClaudeCatalogOutageWarnsAndReportsUnknown(t *testing.T) {
	sandbox(t)
	t.Setenv(ratesEnv, filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("WTM_NOW", "2026-01-01T01:00:00Z")
	repo := nativeRepo(t)
	path := mkClaudeSession(t, repo, "outage", time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC))
	writeFile(t, path, claudeTranscript(repo)+claudeUsageLine(`{"input_tokens":1000}`)+"\n", 0o644)

	out, errOut, rc := runWTM(t, repo, []string{"COLUMNS=200"}, "status", "--color=never")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, errOut)
	}
	if !strings.Contains(errOut, "claude cost unavailable") {
		t.Errorf("stderr must disclose the missing catalog:\n%s", errOut)
	}
	if !strings.Contains(out, "unknown") {
		t.Errorf("table must report unknown cost:\n%s", out)
	}

	jsonOut, jsonErr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status --json exit %d: %s", rc, jsonErr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonOut)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if v, present := rows[0]["cost"]; !present || v != nil {
		t.Errorf("cost = %v (present %v), want null", v, present)
	}
}

// TestStatusDisclosesPartialPiUsage pins T5 through the real binary: one
// dropped record taints the total, so the table marks ACTIVE with "~", COST
// reads unknown, the partial legend prints, and JSON carries the flag.
func TestStatusDisclosesPartialPiUsage(t *testing.T) {
	sandbox(t)
	t.Setenv(ratesEnv, "")
	t.Setenv("WTM_NOW", "2026-01-01T01:00:00Z")
	repo := nativeRepo(t)
	path := mkSession(t, repo, "partial", time.Date(2026, 1, 1, 0, 0, 10, 0, time.UTC))
	body := fmt.Sprintf("{\"type\":\"session\",\"cwd\":%q}\n", repo)
	body += `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}
`
	body += `{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0}}}}
`
	body += `{"type":"message","timestamp":"bad","message":{"role":"assistant","model":"m","usage":` + "\n"
	writeFile(t, path, body, 0o644)

	out, errOut, rc := runWTM(t, repo, []string{"COLUMNS=200"}, "status", "--color=never")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, errOut)
	}
	for _, want := range []string{"~10s", "unknown", partialLegend} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q:\n%s", want, out)
		}
	}
	if !strings.Contains(errOut, "session record") {
		t.Errorf("stderr must disclose the dropped record:\n%s", errOut)
	}

	jsonOut, jsonErr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status --json exit %d: %s", rc, jsonErr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &rows); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, jsonOut)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0]["partial"] != true {
		t.Errorf("partial = %v, want true", rows[0]["partial"])
	}
	if v, present := rows[0]["cost"]; !present || v != nil {
		t.Errorf("cost = %v (present %v), want null", v, present)
	}
}

// TestBuildUsageGoodSessionStaysExact pins the other half: a fully readable
// session stays exact, with no warning and no partial flag.
func TestBuildUsageGoodSessionStaysExact(t *testing.T) {
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", piAgent.name, piUsageLine(1.5))
	u, err := buildUsage(dir, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if u.partial {
		t.Error("a fully readable session must not be partial")
	}
	if got := u.cost().cell(); got != "$1.50" {
		t.Errorf("cost = %q, want $1.50", got)
	}
}
