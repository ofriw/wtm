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
	events, err := piUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(lines, "\n") + "\n")))
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
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","usage":{"cost":{"total":0.25}},"details":{"model":"m-child","stats":{"cost":0.25}}}}`)
	if len(events) != 1 {
		t.Fatalf("got %d events, want 1: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-child", fptr(0.25))
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
	// A record without a usable timestamp contributes nothing.
	if got := parsePi(t,
		`{"type":"message","timestamp":"nope","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}`); len(got) != 0 {
		t.Fatalf("bad timestamp produced %d events, want 0", len(got))
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
	events := parsePi(t, lines...)
	if len(events) != 5 {
		t.Fatalf("got %d events, want 5: %+v", len(events), events)
	}
	assertPiEvent(t, events[0], "m-a", fptr(0.5))
	assertPiEvent(t, events[1], "m-a", fptr(0.25))
	assertPiEvent(t, events[2], "m-b", fptr(0.1))
	assertPiEvent(t, events[3], "m-child", fptr(0.7))
	assertPiEvent(t, events[4], "m-a", nil)
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
