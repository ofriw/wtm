package main

import (
	"bufio"
	"strings"
	"testing"
	"time"
)

// pi_usage_test.go — the Pi parser contract: priced assistant messages, the
// spawn child (which lives only in the parent's tool result), and the rule that
// a child is counted exactly once.

const piFixture = `{"type":"session","version":3,"id":"s","timestamp":"2026-01-01T00:00:00Z","cwd":"/repo"}
{"type":"model_change","id":"m","timestamp":"2026-01-01T00:00:00Z","provider":"p","modelId":"m-default"}
{"type":"message","id":"a","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m-a","usage":{"cost":{"total":0.5}}}}
{"type":"message","id":"u","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","model":"m-a","usage":{"cost":{"total":0.25}}}}
{"type":"message","id":"c","timestamp":"2026-01-01T00:00:04Z","message":{"role":"assistant","model":"m-b","usage":{"cost":{"total":0.1}}}}
{"type":"message","id":"s","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-child","stats":{"cost":0.7}}}}
{"type":"message","id":"b","timestamp":"2026-01-01T00:00:06Z","message":{"role":"assistant","model":"m-a","usage":{}}}
{"type":"compaction","id":"x","timestamp":"2026-01-01T00:00:07Z","summary":"handoff"}
`

func parsePi(t *testing.T, lines ...string) []usageEvent {
	t.Helper()
	events, _, err := piUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")))
	if err != nil {
		t.Fatalf("piUsageEvents: %v", err)
	}
	return events
}

func parsePiFixture(t *testing.T) []usageEvent { return parsePi(t, piFixture) }

func assertPiEvent(t *testing.T, e usageEvent, model string, cost *float64) {
	t.Helper()
	if e.model != model {
		t.Errorf("model = %q, want %q", e.model, model)
	}
	switch {
	case cost == nil && e.cost != nil:
		t.Errorf("cost = %v, want nil", *e.cost)
	case cost != nil && e.cost == nil:
		t.Errorf("cost = nil, want %v", *cost)
	case cost != nil && *e.cost != *cost:
		t.Errorf("cost = %v, want %v", *e.cost, *cost)
	}
}

func TestPiUsageEvents(t *testing.T) {
	events := parsePiFixture(t)
	// Compaction carries no usage and must not become an event.
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-a", fptr(0.5))
	assertPiEvent(t, events[1], "m-a", fptr(0.25))
	assertPiEvent(t, events[2], "m-b", fptr(0.1))
	assertPiEvent(t, events[3], "m-child", fptr(0.7))
	// A usage record with no cost stays an event with an unknown cost.
	assertPiEvent(t, events[4], "m-a", nil)
}

// TestPiModelFallbackFromModelChange pins that an assistant usage record with no
// model is attributed to the session's tracked model, not to "unknown".
func TestPiModelFallbackFromModelChange(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","modelId":"m-switched"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","usage":{"cost":{"total":0.5}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-switched", fptr(0.5))
}

// TestPiSpawnCountsOnce pins the no-double-count invariant. Real Pi spawn
// results can carry the child cost in both the tool-result usage and
// details.stats, with equal values. The child must appear as one event.
func TestPiSpawnCountsOnce(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","usage":{"input":1,"output":2,"cacheRead":3,"cacheWrite":4,"cost":{"total":0.25}},"details":{"model":"m-child","stats":{"inputTokens":100,"outputTokens":50,"cacheReadTokens":20,"cacheWriteTokens":10,"cost":0.25}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-child", fptr(0.25))
	// The two shapes describe one child: stats wins, never the sum.
	if got, want := events[0].tokens, (promptTokens{input: 100, output: 50, cacheRead: 20, cacheWrite5m: 10}); got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
}

// TestPiSpawnModernStatsTokens pins the modern token shape when the tool-result
// usage carries no token counts.
func TestPiSpawnModernStatsTokens(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-child","stats":{"inputTokens":1000,"outputTokens":200,"cacheReadTokens":300,"cacheWriteTokens":40,"cost":0.7}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if got, want := events[0].tokens, (promptTokens{input: 1000, output: 200, cacheRead: 300, cacheWrite5m: 40}); got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
}

// TestPiSpawnStatsCostOnlyFallsBackToUsageTokens pins the single-source rule:
// when stats records no token class, the tool-result usage supplies the child's
// tokens, and the stats cost still prices it.
func TestPiSpawnStatsCostOnlyFallsBackToUsageTokens(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","usage":{"input":7,"output":3,"cacheRead":5,"cacheWrite":2,"cost":{"total":0.9}},"details":{"model":"m-child","stats":{"cost":0.4}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	// stats wins the cost; usage supplies the missing token classes.
	assertPiEvent(t, events[0], "m-child", fptr(0.4))
	if got, want := events[0].tokens, (promptTokens{input: 7, output: 3, cacheRead: 5, cacheWrite5m: 2}); got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
}

// TestPiSpawnLegacyResultsSchema pins the older Pi shape: a spawn result writes
// each child under details.results[].usage, with its own model and cost. The
// child must price on its own model, not the parent's, and must span its call.
func TestPiSpawnLegacyResultsSchema(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m-parent","content":[{"type":"toolCall","id":"c1","name":"spawn"}],"usage":{"cost":{"total":0.05}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:35Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{"mode":"spawn","results":[{"prompt":"p","usage":{"model":"m-child","cost":0.7,"input":100,"output":50,"cacheRead":20,"cacheWrite":10},"output":"o"}]}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	spawn := events[1]
	assertPiEvent(t, spawn, "m-child", fptr(0.7))
	if got, want := spawn.tokens, (promptTokens{input: 100, output: 50, cacheRead: 20, cacheWrite5m: 10}); got != want {
		t.Errorf("tokens = %+v, want %+v", got, want)
	}
	if !spawn.at.Equal(at(5)) || !spawn.end.Equal(at(35)) {
		t.Errorf("spawn span = %v..%v, want 00:00:05..00:00:35", spawn.at, spawn.end)
	}
}

// TestPiSpawnLegacyChildProvesNothing pins the fail-closed rule: an older
// result carries no route, so the child keeps an empty provider instead of
// inheriting the parent's — even under a proven parent, it must not read as
// covered.
func TestPiSpawnLegacyChildProvesNothing(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-child","cost":0.7}}]}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-child", fptr(0.7))
	if events[0].provider != "" {
		t.Errorf("provider = %q, want empty (legacy child must not inherit the parent's)", events[0].provider)
	}
}

// TestPiSpawnLegacyBatch pins a batch spawn that mixed models: one event per
// child, each on its own model, so no child is summed into another's row.
func TestPiSpawnLegacyBatch(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-a","cost":0.1,"input":100,"output":10}},{"usage":{"model":"m-b","cost":0.2,"input":200,"output":20}}]}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-a", fptr(0.1))
	assertPiEvent(t, events[1], "m-b", fptr(0.2))
	if got, want := events[0].tokens, (promptTokens{input: 100, output: 10}); got != want {
		t.Errorf("m-a tokens = %+v, want %+v", got, want)
	}
	if got, want := events[1].tokens, (promptTokens{input: 200, output: 20}); got != want {
		t.Errorf("m-b tokens = %+v, want %+v", got, want)
	}
}

// TestPiSpawnTokensFoldIntoWorktree pins the downstream contract: child tokens
// reach the model rows, the worktree token total, the cache hit rate, and the
// input:output ratio.
func TestPiSpawnTokensFoldIntoWorktree(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-a","cost":0.1,"input":100,"output":10,"cacheRead":50,"cacheWrite":5}},{"usage":{"model":"m-b","cost":0.2,"input":200,"output":20}}]}}}`)
	var u worktreeUsage
	u.addSession("pi", events, pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		}
		return m
	}))
	got := u.tokens()
	want := promptTokens{input: 300, output: 30, cacheRead: 50, cacheWrite5m: 5}
	if got != want {
		t.Errorf("worktree tokens = %+v, want %+v", got, want)
	}
	if rate, ok := got.hitRate(); !ok || formatPercent(rate) != "14.1%" {
		t.Errorf("hit rate = %v/%v, want 14.1%%", rate, ok)
	}
	if ratio, ok := got.outputRatio(got.context()); !ok || formatRatio(ratio) != "11.8:1" {
		t.Errorf("input:output ratio = %v/%v, want 11.8:1", ratio, ok)
	}
}

// TestPiSpawnLegacyIdenticalChildrenKeepBoth pins the dedupe discriminator: two
// children with equal time, model and cost are two real events, not a repeat.
func TestPiSpawnLegacyIdenticalChildrenKeepBoth(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-a","cost":0.1}},{"usage":{"model":"m-a","cost":0.1}}]}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].key == events[1].key {
		t.Errorf("identical children share key %q; dedupe would drop one", events[0].key)
	}
}

// TestPiEventKeyIgnoresProvider pins the fork-dedupe identity: the same record
// re-attributed to another running provider still dedupes, while a spawn
// batch index keeps two equal children distinct.
func TestPiEventKeyIgnoresProvider(t *testing.T) {
	a := usageEvent{at: at(5), model: "m", provider: "p-a", cost: fptr(0.5)}
	b := usageEvent{at: at(5), model: "m", provider: "p-b", cost: fptr(0.5)}
	// Content fallback (no ids): the same record re-attributed to another
	// running provider still dedupes.
	a.key, b.key = piEventKey(a, ""), piEventKey(b, "")
	if a.key != b.key {
		t.Fatalf("same content keys differ: %q vs %q", a.key, b.key)
	}
	// An explicit id is the identity, and it is provider-independent too.
	a.key, b.key = piEventKey(a, "e1"), piEventKey(b, "e1")
	if a.key != b.key {
		t.Fatalf("same id keys differ: %q vs %q", a.key, b.key)
	}
	if piEventKeyN(a, "e1", "spawn:0") == piEventKeyN(a, "e1", "spawn:1") {
		t.Errorf("spawn batch index must keep equal children distinct")
	}
	lists := [][]usageEvent{{a}, {b}}
	masks := dedupeKeyedEvents(lists)
	if n := len(keptBilled(lists[0], masks[0])) + len(keptBilled(lists[1], masks[1])); n != 1 {
		t.Errorf("forked repeat kept %d events, want 1", n)
	}
}

// TestPiTopLevelUsageRecords pins P1: a top-level `usage` record (cache
// warming) and a compaction/branch_summary that carries usage all bill. The
// top-level usage record names its own model and provider and seeds the
// running attribution the model-less summary records inherit.
func TestPiTopLevelUsageRecords(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","id":"mc","timestamp":"2026-01-01T00:00:00Z","provider":"p-start","modelId":"m-start"}`,
		`{"type":"usage","id":"u1","timestamp":"2026-01-01T00:00:01Z","kind":"cache_warm","provider":"p-warm","model":"m-warm","usage":{"cost":{"total":0.3}}}`,
		`{"type":"compaction","id":"cmp","timestamp":"2026-01-01T00:00:02Z","summary":"s","usage":{"cost":{"total":0.2}}}`,
		`{"type":"branch_summary","id":"br","timestamp":"2026-01-01T00:00:03Z","summary":"s","usage":{"cost":{"total":0.1}}}`)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-warm", fptr(0.3))
	if events[0].provider != "p-warm" {
		t.Errorf("top-level usage provider = %q, want p-warm", events[0].provider)
	}
	assertPiEvent(t, events[1], "m-warm", fptr(0.2))
	assertPiEvent(t, events[2], "m-warm", fptr(0.1))
}

// TestPiRecordIdentityUsesID pins P2: the entry id is the identity, so two
// distinct records with equal content stay two, while the same id repeated (a
// fork copy) bills once.
func TestPiRecordIdentityUsesID(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","id":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.5}}}}`,
		`{"type":"message","id":"a2","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.5}}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	if events[0].key == events[1].key {
		t.Errorf("distinct ids share key %q; dedupe would drop one", events[0].key)
	}
	lists := [][]usageEvent{{events[0]}, {events[0]}}
	masks := dedupeKeyedEvents(lists)
	if n := len(keptBilled(lists[0], masks[0])) + len(keptBilled(lists[1], masks[1])); n != 1 {
		t.Errorf("forked record kept %d events, want 1", n)
	}
}

// TestPiMissingCostAndZeroCostDiffer pins P2's cost-presence rule: with no id
// the content fallback still separates a known zero from a missing cost, so an
// unknown total is never hidden behind a zero.
func TestPiMissingCostAndZeroCostDiffer(t *testing.T) {
	zero := parsePi(t, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0}}}}`)
	missing := parsePi(t, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{}}}`)
	if len(zero) != 1 || len(missing) != 1 {
		t.Fatalf("want one event each, got %d and %d", len(zero), len(missing))
	}
	if zero[0].key == missing[0].key {
		t.Errorf("missing cost and known zero share key %q; dedupe would hide the unknown", zero[0].key)
	}
}

// TestPiIdlessCostRevisionBillsOnce pins the fallback mirror of the id key: two
// id-less copies of one record (partial vs complete stream) share the content
// key, so exactly one bills and the larger cost wins.
func TestPiIdlessCostRevisionBillsOnce(t *testing.T) {
	partial := parsePi(t, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.01}}}}`)
	complete := parsePi(t, `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.02}}}}`)
	if len(partial) != 1 || len(complete) != 1 {
		t.Fatalf("want one event each, got %d and %d", len(partial), len(complete))
	}
	kept := dedupeOne(partial, complete)
	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1 (revised copy bills once)", len(kept))
	}
	if kept[0].cost == nil || *kept[0].cost != 0.02 {
		t.Fatalf("winner cost = %v, want 0.02", kept[0].cost)
	}
}

// TestPiSpawnLegacyResultWithoutCost pins that an older child with no cost stays
// an unknown-cost event, attributed to the parent's running model.
func TestPiSpawnLegacyResultWithoutCost(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m-parent","usage":{"cost":{"total":0.05}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{}}]}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	assertPiEvent(t, events[1], "m-parent", nil)
	if events[1].tokens.total() != 0 {
		t.Errorf("cost-only legacy child tokens = %+v, want none", events[1].tokens)
	}
}

// TestPiSpawnRoutedProvider pins the child's billing provider: Pi records the
// provider that served a routed child on the spawn result, and that provider —
// not the parent's — must price the child.
func TestPiSpawnRoutedProvider(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-child","route":{"status":"routed","provider":"vercel-ai-gateway"},"stats":{"cost":0.6}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-child", fptr(0.6))
	if events[0].provider != "vercel-ai-gateway" {
		t.Errorf("provider = %q, want the routed child's vercel-ai-gateway", events[0].provider)
	}
}

// TestPiSpawnInheritedProvider pins the fallback: an inherited child records no
// provider, so it bills on the parent's.
func TestPiSpawnInheritedProvider(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-child","route":{"status":"inherited"},"stats":{"cost":0.6}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	if events[0].provider != "openai-codex" {
		t.Errorf("provider = %q, want the parent's openai-codex", events[0].provider)
	}
}

// TestPiSpawnCostFromUsage covers a child that reports its cost only in the
// tool-result usage: the cost must not be lost or turned unknown.
func TestPiSpawnCostFromUsage(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","usage":{"cost":{"total":0.4}},"details":{"model":"m-child"}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-child", fptr(0.4))
}

// TestPiSpawnWithoutCost pins that a non-error child with no reported cost is an
// unknown-cost event, not a silent omission.
func TestPiSpawnWithoutCost(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:07Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-costless"}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-costless", nil)
}

// TestPiFailedSpawnIsKnownZero pins the accounting fix: a failed spawn that
// reports neither a child nor a cost is a known zero, so it cannot poison the
// worktree total with unknown.
func TestPiFailedSpawnIsKnownZero(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:07Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","isError":true,"details":{}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "unknown", fptr(0))
}

// TestPiFailedSpawnBareRecordIsKnownZero pins the pre-filter contract: a failed
// spawn that carries only isError (no usage, stats or details) must still reach
// the parser, so the known-zero event is never silently dropped.
func TestPiFailedSpawnBareRecordIsKnownZero(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:07Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","isError":true}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "unknown", fptr(0))
}

// TestPiSpawnRunningTime pins that a spawn spans from its tool call to its
// result, so a long child is attributed its running time rather than a point.
func TestPiSpawnRunningTime(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m-parent","content":[{"type":"toolCall","id":"c1","name":"spawn"}],"usage":{"cost":{"total":0.05}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:35Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","isError":true,"details":{}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	spawn := events[1]
	assertPiEvent(t, spawn, "m-parent", fptr(0))
	if !spawn.at.Equal(at(5)) || !spawn.end.Equal(at(35)) {
		t.Errorf("spawn span = %v..%v, want 00:00:05..00:00:35", spawn.at, spawn.end)
	}
}

// TestPiSpawnErrorKeepsReportedCost pins that a failed child's partial cost is
// kept, and still spans its running time.
func TestPiSpawnErrorKeepsReportedCost(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m-parent","content":[{"type":"toolCall","id":"c1","name":"spawn"}],"usage":{"cost":{"total":0.01}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:20Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","isError":true,"usage":{"cost":{"total":0.4}},"details":{"model":"m-child"}}}`)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2: %+v", len(events), events)
	}
	spawn := events[1]
	assertPiEvent(t, spawn, "m-child", fptr(0.4))
	if !spawn.at.Equal(at(5)) || !spawn.end.Equal(at(20)) {
		t.Errorf("spawn span = %v..%v, want 00:00:05..00:00:20", spawn.at, spawn.end)
	}
}

// TestPiSpawnCallWithoutUsageStillSpans pins the pre-filter rule: an assistant
// record carrying only a spawn toolCall (no usage) must still register the
// call, so the child's running time survives as the result's span.
func TestPiSpawnCallWithoutUsageStillSpans(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m-parent","content":[{"type":"toolCall","id":"c1","name":"spawn"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:35Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{}}}`)
	// A usage-less assistant record is no cost event; the spawn result is one.
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	spawn := events[0]
	if !spawn.at.Equal(at(5)) || !spawn.end.Equal(at(35)) {
		t.Errorf("spawn span = %v..%v, want 00:00:05..00:00:35", spawn.at, spawn.end)
	}
}

func TestPiUsageTimestamps(t *testing.T) {
	events := parsePiFixture(t)
	want := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	if !events[0].at.Equal(want) {
		t.Errorf("first event at = %v, want %v", events[0].at, want)
	}
	// A record without a usable timestamp contributes nothing and is counted.
	events, dropped, err := piUsageEvents(bufio.NewReader(strings.NewReader(
		`{"type":"message","timestamp":"nope","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}` + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("bad timestamp produced %d events, want 0", len(events))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1: a bad timestamp must be disclosed", dropped)
	}
}

// TestPiUsageSkipsCorruptLines pins corrupt-JSONL resilience: 'not json' and
// truncated lines interleaved between good events are skipped, and the good
// costs survive intact. The truncated assistant line carries the usage key so
// it passes the pre-filter and exercises the decode-failure path.
func TestPiUsageSkipsCorruptLines(t *testing.T) {
	good := strings.Split(strings.TrimRight(piFixture, "\n"), "\n")
	truncated := `{"type":"message","id":"a","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m-a","usage":{"cost":{"total":`
	lines := []string{
		good[0], good[1], good[2],
		"not json",
		truncated,
		good[3], good[4],
		`{"type":"message","timestamp":"2026-01-01T00:00:0`,
		good[5], good[6], good[7],
	}
	events, dropped, err := piUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1: only the truncated usage line is unreadable", dropped)
	}
	assertPiEvent(t, events[0], "m-a", fptr(0.5))
	assertPiEvent(t, events[1], "m-a", fptr(0.25))
	assertPiEvent(t, events[2], "m-b", fptr(0.1))
	assertPiEvent(t, events[3], "m-child", fptr(0.7))
	assertPiEvent(t, events[4], "m-a", nil)
}

// TestPiUsageOversizedRecordIsDropped pins the bounded-memory loss contract: an
// oversized record is counted once, and the next good record still prices.
func TestPiUsageOversizedRecordIsDropped(t *testing.T) {
	big := `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":9}},"pad":"` + strings.Repeat("a", maxSessionHeader) + `"}}`
	events, dropped, err := piUsageEvents(bufio.NewReader(strings.NewReader(big + "\n" + piUsageLine(1.5) + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want only the good record: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m", fptr(1.5))
}

// TestPiProviderToolResultDoesNotUpdate pins that a toolResult with a provider
// field does not update the running provider. Only assistant messages carry the
// session's provider; a toolResult's provider belongs to a child agent and must
// not leak into subsequent parent events.
func TestPiProviderToolResultDoesNotUpdate(t *testing.T) {
	events := parsePi(t,
		`{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m-parent","provider":"anthropic","usage":{"cost":{"total":0.1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:02Z","message":{"role":"toolResult","provider":"openai","toolName":"bash","usage":{"cost":{"total":0.05}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"assistant","model":"m-parent","usage":{"cost":{"total":0.2}}}}`)
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3: %+v", len(events), events)
	}
	// The toolResult at events[1] must not have updated the running provider.
	// The next assistant message at events[2] must still carry "anthropic".
	if events[2].provider != "anthropic" {
		t.Errorf("events[2].provider = %q, want anthropic (toolResult provider must not leak)", events[2].provider)
	}
}

// TestPiCacheMissBillsOnce pins the design rule that Pi reports the cost after
// it re-bills a cache miss: cache fields on a record are informational, and the
// command adds cost.total once — never a separate cache-miss price.
func TestPiCacheMissBillsOnce(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.5},"cache_read":1000,"cache_write":500}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1", len(events))
	}
	assertPiEvent(t, events[0], "m", fptr(0.5))
}

// TestPiSameIDCostRevisionBillsOnce pins the id-key rule: two copies of one
// record (partial vs complete stream) share the id, so exactly one bills and
// the larger cost wins.
func TestPiSameIDCostRevisionBillsOnce(t *testing.T) {
	a := usageEvent{at: at(1), model: "m", cost: fptr(0.01)}
	b := usageEvent{at: at(1), model: "m", cost: fptr(0.02)}
	a.key = piEventKeyN(a, "e1", "")
	b.key = piEventKeyN(b, "e1", "")
	if a.key != b.key {
		t.Fatalf("same id keys differ: %q vs %q", a.key, b.key)
	}
	lists := [][]usageEvent{{a}, {b}}
	masks := dedupeKeyedEvents(lists)
	kept := append(keptBilled(lists[0], masks[0]), keptBilled(lists[1], masks[1])...)
	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1", len(kept))
	}
	if kept[0].cost == nil || *kept[0].cost != 0.02 {
		t.Fatalf("winner cost = %v, want 0.02", kept[0].cost)
	}
}

// TestPiSameIDNilVsZeroBillsOnce pins the presence rule under an id key: a
// nil-cost copy and a known-zero copy share the id, so exactly one bills and
// the known zero wins over the unknown.
func TestPiSameIDNilVsZeroBillsOnce(t *testing.T) {
	a := usageEvent{at: at(1), model: "m"}
	b := usageEvent{at: at(1), model: "m", cost: fptr(0)}
	a.key = piEventKeyN(a, "e1", "")
	b.key = piEventKeyN(b, "e1", "")
	if a.key != b.key {
		t.Fatalf("same id keys differ: %q vs %q", a.key, b.key)
	}
	lists := [][]usageEvent{{a}, {b}}
	masks := dedupeKeyedEvents(lists)
	kept := append(keptBilled(lists[0], masks[0]), keptBilled(lists[1], masks[1])...)
	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1", len(kept))
	}
	if kept[0].cost == nil || *kept[0].cost != 0 {
		t.Fatalf("winner cost = %v, want the known 0", kept[0].cost)
	}
}

// TestPiSameIDDifferentTimestampBillsTwice pins the time binding on the id key:
// two unrelated sessions can collide on the short random id, but not on the
// exact timestamp, so both turns must bill.
func TestPiSameIDDifferentTimestampBillsTwice(t *testing.T) {
	a := usageEvent{at: at(1), model: "m", cost: fptr(0.5)}
	b := usageEvent{at: at(2), model: "m", cost: fptr(0.5)}
	a.key = piEventKeyN(a, "e1", "")
	b.key = piEventKeyN(b, "e1", "")
	if a.key == b.key {
		t.Fatalf("same id but different time share key %q; one turn would drop", a.key)
	}
	lists := [][]usageEvent{{a}, {b}}
	masks := dedupeKeyedEvents(lists)
	kept := append(keptBilled(lists[0], masks[0]), keptBilled(lists[1], masks[1])...)
	if len(kept) != 2 {
		t.Fatalf("kept %d events, want 2 (distinct times bill separately)", len(kept))
	}
}

// TestPiIdlessSameTimestampCollisionBillsOnce pins 3C: id-less records with
// equal time, model and cost share one fallback key, so the same record seen in
// two sessions bills once. Both sessions still count toward active time.
func TestPiIdlessSameTimestampCollisionBillsOnce(t *testing.T) {
	line := `{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.5}}}}`
	a := parsePi(t, line)
	b := parsePi(t, line)
	if len(a) != 1 || len(b) != 1 {
		t.Fatalf("want one event each, got %d and %d", len(a), len(b))
	}
	if a[0].key != b[0].key {
		t.Fatalf("equal id-less records keys differ: %q vs %q", a[0].key, b[0].key)
	}
	kept := dedupeOne(a, b)
	if len(kept) != 1 {
		t.Fatalf("kept %d events, want 1 (same-timestamp collision bills once)", len(kept))
	}
}

// TestPiSpawnWithoutIDCrossSessionBillsOnce pins 3C: a spawn batch without a
// record id still dedupes across sessions — the content fallback plus the child
// index is the identity — so a repeated transcript bills each child once.
func TestPiSpawnWithoutIDCrossSessionBillsOnce(t *testing.T) {
	line := `{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-a","cost":0.1,"input":100,"output":10,"cacheRead":5,"cacheWrite":2}},{"usage":{"model":"m-b","cost":0.2,"input":200,"output":20}}]}}}`
	a := parsePi(t, line)
	b := parsePi(t, line)
	if len(a) != 2 || len(b) != 2 {
		t.Fatalf("want two children each, got %d and %d", len(a), len(b))
	}
	kept := dedupeOne(a, b)
	if len(kept) != 2 {
		t.Fatalf("kept %d children, want 2 (each child bills once)", len(kept))
	}
	var u worktreeUsage
	price := pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		} else {
			m.addUnknown()
		}
		return m
	})
	billed := dedupeKeyedEvents([][]usageEvent{a, b})
	u.addBilledSession("pi", a, billed[0], price)
	u.addBilledSession("pi", b, billed[1], price)
	if got := u.cost().cell(); got != "$0.30" {
		t.Fatalf("cost = %q, want $0.30 (cross-session spawn bills once)", got)
	}
	wantTokens := promptTokens{input: 300, output: 30, cacheRead: 5, cacheWrite5m: 2}
	if got := u.tokens(); got != wantTokens {
		t.Fatalf("tokens = %+v, want %+v (each child folds once)", got, wantTokens)
	}
}

// TestPiUsageTokensAndTierCost pins the parser's token contract: a modern Pi
// record fills the shared prompt-token classes and the per-class price split,
// while a legacy total-only record leaves both token and tier data absent.
func TestPiUsageTokensAndTierCost(t *testing.T) {
	events := parsePi(t,
		`{"type":"message","id":"a1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"input":100,"output":50,"cacheRead":200,"cacheWrite":30,"cost":{"input":0.1,"output":0.2,"cacheRead":0.03,"cacheWrite":0.01,"total":0.34}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	e := events[0]
	if e.tokens.input != 100 || e.tokens.output != 50 || e.tokens.cacheRead != 200 || e.tokens.cacheWrite5m != 30 {
		t.Errorf("tokens = %+v, want input=100 output=50 cacheRead=200 cacheWrite5m=30", e.tokens)
	}
	assertPiEvent(t, e, "m", fptr(0.34))
	if e.tierCost == nil {
		t.Fatal("tierCost = nil, want the record's per-class prices")
	}
	for _, tc := range []struct {
		name string
		got  *float64
		want float64
	}{
		{"input", e.tierCost.input, 0.1},
		{"output", e.tierCost.output, 0.2},
		{"cacheRead", e.tierCost.cacheRead, 0.03},
		{"cacheWrite", e.tierCost.cacheWrite, 0.01},
	} {
		if tc.got == nil {
			t.Errorf("tierCost.%s = nil, want %v", tc.name, tc.want)
			continue
		}
		if *tc.got != tc.want {
			t.Errorf("tierCost.%s = %v, want %v", tc.name, *tc.got, tc.want)
		}
	}

	// A legacy record prices only the total: no tokens, no per-class split.
	legacy := parsePi(t,
		`{"type":"message","id":"a2","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.5}}}}`)
	if len(legacy) != 1 {
		t.Fatalf("got %d legacy events, want 1", len(legacy))
	}
	if legacy[0].tierCost != nil {
		t.Errorf("legacy tierCost = %+v, want nil (only total priced)", legacy[0].tierCost)
	}
	if legacy[0].tokens.total() != 0 {
		t.Errorf("legacy tokens.total() = %d, want 0", legacy[0].tokens.total())
	}
}
