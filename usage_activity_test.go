package main

import (
	"bufio"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// usage_activity_test.go — the causal ACTIVE contract: turns, not gaps. A
// transcript gap is either work (tool execution) or idle (between turns); only
// turn boundaries can tell them apart.

func atMin(base time.Time, minutes int) time.Time {
	return base.Add(time.Duration(minutes) * time.Minute)
}

// sessionBlocks is the span-only view of a session's causal turns, used by
// activity tests that need no model attribution.
func sessionBlocks(events []activityEvent) []span { return turnSpans(buildTurns(events)) }

func TestSessionBlocksExcludeIdleBetweenTurns(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blocks := sessionBlocks([]activityEvent{
		{at: base, kind: activityHuman},
		{at: atMin(base, 1), kind: activityWork},
		{at: atMin(base, 2), kind: activityEnd},
		{at: atMin(base, 120), kind: activityHuman},
		{at: atMin(base, 125), kind: activityEnd},
	})
	want := 2*time.Minute + 5*time.Minute
	if got := unionDuration(blocks); got != want {
		t.Fatalf("active = %v, want %v", got, want)
	}
}

func TestSessionBlocksMissingEndClosesAtLastWork(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blocks := sessionBlocks([]activityEvent{
		{at: base, kind: activityHuman},
		{at: atMin(base, 10), kind: activityWork},
		{at: atMin(base, 120), kind: activityHuman},
	})
	if got := unionDuration(blocks); got != 10*time.Minute {
		t.Fatalf("active = %v, want 10m: a missing end_turn must not count idle", got)
	}
}

func TestSessionBlocksCountLongToolRun(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	blocks := sessionBlocks([]activityEvent{
		{at: base, kind: activityHuman},
		{at: atMin(base, 1), kind: activityWork},
		{at: atMin(base, 60), kind: activityWork},
		{at: atMin(base, 65), kind: activityEnd},
	})
	if got := unionDuration(blocks); got != 65*time.Minute {
		t.Fatalf("active = %v, want 65m: tool execution is work", got)
	}
}

func TestClipSpansKeepsOnlyTurnTime(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	raw := []span{{start: atMin(base, 1), end: atMin(base, 120)}}
	blocks := []span{{start: base, end: atMin(base, 5)}, {start: atMin(base, 100), end: atMin(base, 105)}}
	clipped := clipSpans(raw, blocks)
	if got := unionDuration(clipped); got != 9*time.Minute {
		t.Fatalf("clipped = %v, want 9m", got)
	}
}

// TestPiActivityCausalTurns pins the parser-to-ACTIVE path: a human prompt and
// an assistant turn stay active through a tool run, the next turn starts two
// hours later, and the two-hour idle is excluded.
func TestPiActivityCausalTurns(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	lines := []string{
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash"}],"usage":{"cost":{"total":1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"ok"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","content":[{"type":"text","text":"done"}],"usage":{"cost":{"total":1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T02:00:00Z","message":{"role":"user","content":[{"type":"text","text":"again"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T02:00:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}`,
	}
	parsed, err := piUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	blocks := sessionBlocks(parsed.activity)
	if got := unionDuration(blocks); got != 70*time.Second {
		t.Fatalf("parsed active = %v, want 70s", got)
	}
	if len(blocks) != 2 {
		t.Fatalf("blocks = %v, want two turns", blocks)
	}
	if !blocks[0].start.Equal(base) || !blocks[1].start.Equal(atMin(base, 120)) {
		t.Fatalf("turn boundaries = %v, want %v and %v", blocks, base, atMin(base, 120))
	}
}

// TestBuildUsageActiveUsesCausalTurns proves the fold consumes parser activity,
// not a first-to-last usage span.
func TestBuildUsageActiveUsesCausalTurns(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "s.jsonl")
	body := strings.Join([]string{
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","stopReason":"toolUse","usage":{"cost":{"total":1}},"content":[{"type":"toolCall","id":"c1","name":"bash"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":"ok"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T02:00:00Z","message":{"role":"user","content":"again"}}`,
		`{"type":"message","timestamp":"2026-01-01T02:00:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}`,
	}, "\n") + "\n"
	writeFile(t, session, body, 0o644)
	u, err := buildUsageForTest(dir, sessionIndex{canonical(dir): {{path: session, harness: piAgent.name}}}, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.active(); got != 70*time.Second {
		t.Fatalf("active = %v, want 70s", got)
	}
}

// TestPiActivityAcceptsSpacedMessageKey pins the encoder-independence of the
// cheap prefilter: the colon is not part of the key, so "message" : must still
// reach the activity decoder. Under-matching drops billable time silently.
func TestPiActivityAcceptsSpacedMessageKey(t *testing.T) {
	lines := []string{
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message" : {"role":"user","content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}`,
	}
	parsed, err := piUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if got := unionDuration(sessionBlocks(parsed.activity)); got != time.Minute {
		t.Fatalf("active = %v, want 1m", got)
	}
}

// TestPiActivityIgnoresMessageTextOutsideTheKey pins the other side of the
// prefilter: a non-message record whose value happens to spell message must not
// create a turn. The decoder's top-level type check owns that decision.
func TestPiActivityIgnoresMessageTextOutsideTheKey(t *testing.T) {
	line := `{"type":"session","timestamp":"2026-01-01T00:00:00Z","note":"message"}`
	parsed, err := piUsageEvents(bufio.NewReader(strings.NewReader(line + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.activity) != 0 {
		t.Fatalf("activity = %v, want none", parsed.activity)
	}
}

// TestClampSpansBoundsFutureTurns pins the clamp boundary: a straddling turn
// ends at the report clock, and a wholly future turn is dropped.
func TestClampSpansBoundsFutureTurns(t *testing.T) {
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	straddling := clampSpans(sessionBlocks([]activityEvent{
		{at: now.Add(-10 * time.Minute), kind: activityHuman},
		{at: now.Add(10 * time.Minute), kind: activityEnd},
	}), now)
	if got := unionDuration(straddling); got != 10*time.Minute {
		t.Fatalf("straddling active = %v, want 10m", got)
	}
	future := clampSpans(sessionBlocks([]activityEvent{
		{at: now.Add(time.Minute), kind: activityHuman},
		{at: now.Add(2 * time.Minute), kind: activityEnd},
	}), now)
	if len(future) != 0 {
		t.Fatalf("future spans = %v, want none", future)
	}
}

// TestAttributeTurnsClampsClippedModelSpans pins the clipped-span clamp at the fold boundary: a
// model span stamped after the report instant must never let a model row exceed
// the harness total in the same run. It drives addBilledSession, the real fold
// entry, instead of re-enacting its composition.
func TestAttributeTurnsClampsClippedModelSpans(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 10, 0, 0, time.UTC)
	var u worktreeUsage
	u.addBilledSession("pi",
		[]usageEvent{{at: now.Add(-5 * time.Minute), end: now.Add(15 * time.Minute), model: "m"}},
		[]activityEvent{
			{at: now.Add(-10 * time.Minute), kind: activityHuman},
			{at: now.Add(-5 * time.Minute), kind: activityWork},
			{at: now.Add(20 * time.Minute), kind: activityEnd},
		},
		nil, func(usageEvent) pricedEvent { return unpriced() }, now)
	h := u.harnesses["pi"]
	// The clipped span runs to now+15m, so pre-fix it stayed unclamped at 20m
	// and a model row could claim more time than the worktree total. The fix
	// clamps it to the report clock: 5m of real usage before now.
	if got := h.model("m").active(); got != 5*time.Minute {
		t.Fatalf("model active = %v, want 5m (clipped span clamped to the report clock)", got)
	}
	if h.active() < h.model("m").active() {
		t.Fatalf("model active %v exceeds harness active %v", h.model("m").active(), h.active())
	}
}

// TestTurnOpeningsCreditTheAnswerer pins the attribution contract: a turn's
// opening belongs to the model of its assistant answer, never to a warmup or
// summary usage record; the boundary record stays with the turn it closed; and
// clipped model time is never re-attributed.
func TestTurnOpeningsCreditTheAnswerer(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sec := func(n int) time.Time { return base.Add(time.Duration(n) * time.Second) }
	warmup := []usageEvent{{at: sec(5), model: "warmup"}}
	child := []usageEvent{{at: sec(5), end: sec(60), model: "child"}, {at: sec(65), model: "parent"}}
	cases := []struct {
		name    string
		turns   []turn
		events  []usageEvent
		covered []span
		want    map[string][]span
	}{
		{
			name:   "warmup is not an answer",
			turns:  []turn{{span: span{sec(0), sec(30)}, answer: "answer", answerAt: sec(30)}},
			events: warmup,
			want:   map[string][]span{"answer": {{sec(0), sec(30)}}},
		},
		{
			name:   "opening stops at the answer's first record",
			turns:  []turn{{span: span{sec(0), sec(65)}, answer: "parent", answerAt: sec(5)}},
			events: child,
			want:   map[string][]span{"parent": {{sec(0), sec(5)}}},
		},
		{
			name:    "covered opening is not re-attributed",
			turns:   []turn{{span: span{sec(0), sec(65)}, answer: "parent", answerAt: sec(5)}},
			events:  child,
			covered: []span{{sec(2), sec(4)}},
			want:    map[string][]span{"parent": {{sec(0), sec(2)}, {sec(4), sec(5)}}},
		},
		{
			name:   "boundary record stays with the turn it closed",
			turns:  []turn{{span: span{sec(0), sec(3)}, answer: "a", answerAt: sec(3)}, {span: span{sec(3), sec(10)}, answer: "b", answerAt: sec(10)}},
			events: []usageEvent{{at: sec(3), model: "a"}, {at: sec(10), model: "b"}},
			want:   map[string][]span{"a": {{sec(0), sec(3)}}, "b": {{sec(3), sec(10)}}},
		},
		{
			name:  "no assistant answer falls back to a point event",
			turns: []turn{{span: span{sec(0), sec(60)}, answerAt: sec(0)}},
			events: []usageEvent{
				{at: sec(0), model: "boundary"},
				{at: sec(30), model: "late"},
				{at: sec(10), model: "early"},
			},
			want: map[string][]span{"early": {{sec(0), sec(10)}}},
		},
	}
	for _, c := range cases {
		got := turnOpenings(c.turns, c.events, c.covered)
		if len(got) != len(c.want) {
			t.Errorf("%s: openings = %v, want %v", c.name, got, c.want)
			continue
		}
		for name, want := range c.want {
			if !slices.Equal(got[name], want) {
				t.Errorf("%s: %s opening = %v, want %v", c.name, name, got[name], want)
			}
		}
	}
}
