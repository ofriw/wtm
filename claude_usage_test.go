package main

import (
	"bufio"
	"strings"
	"testing"
)

// claude_usage_test.go — the Claude parser contract: every assistant record
// becomes a candidate event tagged with its turn identity, cache classes stay
// apart, the synthetic model is skipped, an omitted cache-write lifetime is
// kept as unclassified so pricing can report it unknown, and the worktree-level
// dedupe keeps the largest record of a turn across session files.

const claudeFixture = `{"type":"queue-operation","operation":"dequeue","cwd":null}
{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"r1","message":{"id":"x","model":"claude-opus-4-6","usage":{"input_tokens":3,"output_tokens":1,"cache_read_input_tokens":10,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}
{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","requestId":"r1","message":{"id":"x","model":"claude-opus-4-6","usage":{"input_tokens":3,"output_tokens":400,"cache_read_input_tokens":10,"cache_creation_input_tokens":20,"cache_creation":{"ephemeral_5m_input_tokens":20,"ephemeral_1h_input_tokens":0},"service_tier":"standard","server_tool_use":{"web_search_requests":2}}}}
{"type":"assistant","timestamp":"2026-01-01T00:00:03Z","requestId":"r2","message":{"id":"y","model":"claude-opus-4-6","usage":{"input_tokens":1,"output_tokens":2,"cache_creation_input_tokens":50,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}
{"type":"assistant","timestamp":"2026-01-01T00:00:04Z","requestId":"r3","message":{"id":"z","model":"<synthetic>","usage":{"input_tokens":0,"output_tokens":0}}}
{"type":"user","timestamp":"2026-01-01T00:00:05Z","message":{"role":"user"}}
`

func parseClaudeFixture(t *testing.T) []usageEvent {
	t.Helper()
	events, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(claudeFixture)))
	if err != nil {
		t.Fatalf("claudeUsageEvents: %v", err)
	}
	return events
}

// dedupeOne runs the worktree-level dedupe the way buildUsage does and returns
// the surviving events, so the tests pin the dedupe contract, not its plumbing.
func dedupeOne(sessions ...[]usageEvent) []usageEvent {
	dedupeKeyedEvents(sessions)
	var out []usageEvent
	for _, s := range sessions {
		out = append(out, s...)
	}
	return out
}

func TestClaudeUsageDedupePrefersLargest(t *testing.T) {
	raw := parseClaudeFixture(t)
	// The two streaming records for turn x both reach the dedupe; the parser
	// itself no longer collapses them.
	if len(raw) != 3 {
		t.Fatalf("got %d candidate events, want 3 (synthetic skipped): %+v", len(raw), raw)
	}
	events := dedupeOne(raw)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (turn deduped): %+v", len(events), events)
	}
	// The two records for turn x collapse to the one with output 400 and the
	// web-search count.
	first := events[0]
	if first.tokens.output != 400 {
		t.Errorf("output = %d, want 400 (largest stream wins)", first.tokens.output)
	}
	if first.tokens.cacheWrite5m != 20 || first.tokens.cacheWrite1h != 0 {
		t.Errorf("cache classes = %+v, want 5m=20 1h=0", first.tokens)
	}
	if first.webSearch != 2 {
		t.Errorf("webSearch = %d, want 2", first.webSearch)
	}
	if first.tier != "standard" {
		t.Errorf("tier = %q, want standard", first.tier)
	}
}

// TestClaudeUsageDedupeAcrossSessions pins the invariant that survives a
// resume or fork: the same billed turn repeated in a second session file
// prices once, and the most complete record wins regardless of which file
// holds it.
func TestClaudeUsageDedupeAcrossSessions(t *testing.T) {
	event := func(id string, output int64) usageEvent {
		return usageEvent{at: at(0), key: id + ":r1", model: "m", tokens: claudeTokens{output: output}}
	}
	sessions := [][]usageEvent{
		{event("m1", 100), event("m2", 10)},
		{event("m1", 400)},
	}
	dedupeKeyedEvents(sessions)
	a, b := sessions[0], sessions[1]
	if len(a) != 1 {
		t.Fatalf("session a kept %d events, want 1 (m2 only): %+v", len(a), a)
	}
	if a[0].key != "m2:r1" {
		t.Errorf("session a kept %q, want m2 only", a[0].key)
	}
	if len(b) != 1 || b[0].tokens.output != 400 {
		t.Errorf("session b = %+v, want the largest m1 record alone", b)
	}
}

func TestClaudeUsageUnclassifiedCacheWrite(t *testing.T) {
	events := dedupeOne(parseClaudeFixture(t))
	second := events[1]
	if second.tokens.unclassifiedCacheWrite != 50 {
		t.Fatalf("unclassifiedCacheWrite = %d, want 50", second.tokens.unclassifiedCacheWrite)
	}
}

func TestClaudeUsageSkipsUntimestamped(t *testing.T) {
	events, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(
		`{"type":"assistant","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("untimestamped record produced %d events, want 0", len(events))
	}
}

// TestClaudeUsageSkipsNonAssistant pins the type filter: only an assistant
// record carries billed usage, so a user record with a usage object is not an
// event.
func TestClaudeUsageSkipsNonAssistant(t *testing.T) {
	events, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("non-assistant record produced %d events, want 0", len(events))
	}
}

// TestClaudeUsageSkipsCorruptLines pins corrupt-JSONL resilience: 'not json'
// and truncated lines interleaved between good records are skipped, and the
// good turn (deduped to its largest stream) survives intact.
func TestClaudeUsageSkipsCorruptLines(t *testing.T) {
	good := strings.Split(strings.TrimRight(claudeFixture, "\n"), "\n")
	truncated := `{"type":"assistant","timestamp":"2026-01-01T00:00:02Z","requestId":"r1","message":{"id":"x","model":"m","usage":{"input_tokens":`
	mixed := []string{
		good[0],
		"not json",
		good[1],
		truncated,
		good[2],
		"also not json",
		good[3], good[4],
		`{"type":"assistant","timestamp":"2026-01-01T00:00:0`,
	}
	events, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(mixed, "\n") + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d candidate events, want 3: %+v", len(events), events)
	}
	deduped := dedupeOne(events)
	first := deduped[0]
	if first.tokens.output != 400 || first.webSearch != 2 || first.tier != "standard" {
		t.Errorf("good turn corrupted: %+v, want output=400 webSearch=2 tier=standard", first)
	}
	if second := deduped[1]; second.tokens.unclassifiedCacheWrite != 50 {
		t.Errorf("unclassifiedCacheWrite = %d, want 50", second.tokens.unclassifiedCacheWrite)
	}
}
