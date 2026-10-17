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
	events, _, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(claudeFixture)))
	if err != nil {
		t.Fatalf("claudeUsageEvents: %v", err)
	}
	return events
}

// keptBilled returns the events a billed mask marks, so a test can inspect the
// dedupe outcome the way folding does.
func keptBilled(events []usageEvent, billed []bool) []usageEvent {
	var out []usageEvent
	for i, e := range events {
		if billed[i] {
			out = append(out, e)
		}
	}
	return out
}

// dedupeOne runs the worktree-level dedupe the way buildUsage does and returns
// the surviving billed events, so the tests pin the dedupe contract, not its
// plumbing.
func dedupeOne(sessions ...[]usageEvent) []usageEvent {
	billed := dedupeKeyedEvents(sessions)
	var out []usageEvent
	for i, s := range sessions {
		out = append(out, keptBilled(s, billed[i])...)
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
		return usageEvent{at: at(0), key: id + ":r1", model: "m", tokens: promptTokens{output: output}}
	}
	sessions := [][]usageEvent{
		{event("m1", 100), event("m2", 10)},
		{event("m1", 400)},
	}
	billed := dedupeKeyedEvents(sessions)
	a, b := keptBilled(sessions[0], billed[0]), keptBilled(sessions[1], billed[1])
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

// TestClaudeDedupeWebSearchBreaksTokenTie pins D3: two streaming records of one
// turn that tie on tokens must keep the larger web-search count, whose catalog
// price is real money.
func TestClaudeDedupeWebSearchBreaksTokenTie(t *testing.T) {
	mk := func(web int64) usageEvent {
		return usageEvent{at: at(0), key: "m1:r1", model: "m", tokens: promptTokens{output: 10}, webSearch: web}
	}
	sessions := [][]usageEvent{{mk(1)}, {mk(3)}}
	billed := dedupeKeyedEvents(sessions)
	kept := keptBilled(sessions[0], billed[0])
	kept = append(kept, keptBilled(sessions[1], billed[1])...)
	if len(kept) != 1 || kept[0].webSearch != 3 {
		t.Fatalf("kept = %+v, want the webSearch=3 record alone", kept)
	}
}

// TestClaudeKeylessRecordsSurviveDedupe pins D2: an assistant record with
// neither a message id nor a request id gets an empty key, so unrelated id-less
// records both bill instead of collapsing to one winner.
func TestClaudeKeylessRecordsSurviveDedupe(t *testing.T) {
	line := `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","message":{"model":"m","usage":{"input_tokens":1}}}`
	events, _, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(line + "\n" + line + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2", len(events))
	}
	for i, e := range events {
		if e.key != "" {
			t.Errorf("events[%d].key = %q, want empty", i, e.key)
		}
	}
	if kept := dedupeOne(events); len(kept) != 2 {
		t.Fatalf("id-less records deduped to %d, want 2", len(kept))
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
	events, dropped, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(
		`{"type":"assistant","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("untimestamped record produced %d events, want 0", len(events))
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1: an untimestamped billed record must be disclosed", dropped)
	}
}

// TestClaudeUsageOversizedRecordIsDropped pins the bounded-memory loss
// contract: an oversized billed record is counted once, and the next good
// record still prices.
func TestClaudeUsageOversizedRecordIsDropped(t *testing.T) {
	big := `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"big","message":{"id":"big","model":"m","usage":{"input_tokens":999999},"pad":"` + strings.Repeat("a", maxSessionHeader) + `"}}`
	good := `{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"good","message":{"id":"good","model":"m","usage":{"input_tokens":100,"output_tokens":10}}}`
	events, dropped, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(big + "\n" + good + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1", dropped)
	}
	if len(events) != 1 {
		t.Fatalf("got %d events, want only the good record: %+v", len(events), events)
	}
	if events[0].tokens.input != 100 || events[0].tokens.output != 10 {
		t.Errorf("good record tokens = %+v, want input=100 output=10", events[0].tokens)
	}
}

// TestClaudeUsageSkipsNonAssistant pins the type filter: only an assistant
// record carries billed usage, so a user record with a usage object is not an
// event.
func TestClaudeUsageSkipsNonAssistant(t *testing.T) {
	events, dropped, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(
		`{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`,
	)))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("non-assistant record produced %d events, want 0", len(events))
	}
	if dropped != 0 {
		t.Errorf("dropped = %d, want 0: a non-assistant record is skipped, not dropped", dropped)
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
	events, dropped, err := claudeUsageEvents(bufio.NewReader(strings.NewReader(strings.Join(mixed, "\n") + "\n")))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("got %d candidate events, want 3: %+v", len(events), events)
	}
	if dropped != 1 {
		t.Errorf("dropped = %d, want 1: only the truncated usage line is unreadable", dropped)
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
