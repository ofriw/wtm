package main

import (
	"bufio"
	"path/filepath"
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
	u, err := buildUsage(dir, sessionIndex{canonical(dir): {{path: session, harness: piAgent.name}}}, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got := u.active(); got != 70*time.Second {
		t.Fatalf("active = %v, want 70s", got)
	}
}
