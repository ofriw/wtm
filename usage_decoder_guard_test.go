package main

import (
	"bufio"
	"reflect"
	"strings"
	"testing"
)

// TestDecoderTypeSetGuard pins the record-type sets each harness consumes, so a
// new billed type cannot land silently: every historically handled type must
// still be consumed (an event or turn marker) without a drop, and any readable
// type outside the handled set that carries usage must taint the total instead
// of vanishing. It mirrors TestDecoderTagGuard: both sides of the contract are
// asserted from the same table of inputs.
func TestDecoderTypeSetGuard(t *testing.T) {
	roundTrip := func(name string, parse func(*bufio.Reader) (sessionParse, error), line string) sessionParse {
		t.Helper()
		p, err := parse(bufio.NewReader(strings.NewReader(line + "\n")))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return p
	}
	// Every historically handled type must still be consumed (an event or a
	// turn marker) without a drop; any readable type outside the handled set
	// that carries usage must taint the total instead of vanishing.
	for _, tc := range []struct {
		name        string
		line        string
		harness     string
		wantEvents  int
		wantDropped int
		wantUnsup   string
	}{
		{"pi message", `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":1}}}}`, "pi", 1, 0, ""},
		{"pi usage", `{"type":"usage","timestamp":"2026-01-01T00:00:00Z","usage":{"cost":{"total":1}}}`, "pi", 1, 0, ""},
		{"pi compaction", `{"type":"compaction","timestamp":"2026-01-01T00:00:00Z","usage":{"cost":{"total":1}}}`, "pi", 1, 0, ""},
		{"pi branch_summary", `{"type":"branch_summary","timestamp":"2026-01-01T00:00:00Z","usage":{"cost":{"total":1}}}`, "pi", 1, 0, ""},
		{"pi unknown", `{"type":"turn_batch","timestamp":"2026-01-01T00:00:00Z","usage":{"input":9000,"output":900,"cost":{"total":4.9}}}`, "pi", 0, 1, "turn_batch"},
		{"pi unknown nested message", `{"type":"turn_batch","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"input":9000,"output":900,"cost":{"total":4.9}}}}`, "pi", 0, 1, "turn_batch"},
		{"claude assistant", `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r1","message":{"id":"m","model":"x","usage":{"input_tokens":1}}}`, "claude", 1, 0, ""},
		{"claude user", `{"type":"user","timestamp":"2026-01-01T00:00:00Z","message":{"content":[{"type":"text","text":"hi"}]}}`, "claude", 0, 0, ""},
		{"claude unknown", `{"type":"assistant_turn_v2","timestamp":"2026-01-01T00:00:00Z","requestId":"r2","message":{"id":"m2","model":"x","usage":{"input_tokens":50000,"output_tokens":5000}}}`, "claude", 0, 1, "assistant_turn_v2"},
		{"claude unreadable usage", `{"timestamp":"2026-01-01T00:00:00Z","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`, "claude", 0, 1, ""},
		{"claude unknown oversized", `{"type":"future_billing","timestamp":"2026-01-01T00:00:00Z","message":{"id":"x","model":"m","usage":{"input_tokens":1},"pad":"` + strings.Repeat("a", maxSessionHeader) + `"}}`, "claude", 0, 1, ""},
		{"pi model_change usage", `{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","usage":{"cost":{"total":1}}}`, "pi", 0, 1, "model_change"},
		{"claude user usage", `{"type":"user","timestamp":"2026-01-01T00:00:00Z","usage":{"input_tokens":1}}`, "claude", 0, 1, "user"},
		{"claude malformed assistant", `{"type":"assistant","timestamp":"bad"quote","message":{"id":"x","model":"m","usage":{"input_tokens":1}}}`, "claude", 0, 1, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fn := piUsageEvents
			if tc.harness == "claude" {
				fn = claudeUsageEvents
			}
			p := roundTrip(tc.name, fn, tc.line)
			if len(p.events) != tc.wantEvents || p.dropped != tc.wantDropped {
				t.Errorf("events = %d (want %d), dropped = %d (want %d)", len(p.events), tc.wantEvents, p.dropped, tc.wantDropped)
			}
			if tc.wantUnsup != "" && p.unsupported[tc.wantUnsup] != 1 {
				t.Errorf("unsupported[%s] = %d, want 1", tc.wantUnsup, p.unsupported[tc.wantUnsup])
			}
		})
	}
}

// TestDecoderTagGuard pins every hand-written UnmarshalJSON method to its
// struct's json tags. Each decoder re-lists its keys as string literals instead
// of reading encoding/json tags, so a new billed field added later would decode
// to zero silently. Reflecting over the tags and checking each against the keys
// the decoder handles turns that silence into a loud test failure.
func TestDecoderTagGuard(t *testing.T) {
	type guard struct {
		name    string
		typ     reflect.Type
		handled map[string]bool
	}
	keys := func(k ...string) map[string]bool {
		m := make(map[string]bool, len(k))
		for _, s := range k {
			m[s] = true
		}
		return m
	}
	for _, g := range []guard{
		{"piUsage", reflect.TypeOf(piUsage{}), keys("input", "output", "cacheRead", "cacheWrite", "cost")},
		{"piCost", reflect.TypeOf(piCost{}), keys("input", "output", "cacheRead", "cacheWrite", "total")},
		{"piSpawnStats", reflect.TypeOf(piSpawnStats{}), keys("cost", "inputTokens", "outputTokens", "cacheReadTokens", "cacheWriteTokens")},
		{"piSpawnUsage", reflect.TypeOf(piSpawnUsage{}), keys("model", "cost", "input", "output", "cacheRead", "cacheWrite")},
		{"claudeUsage", reflect.TypeOf(claudeUsage{}), keys("input_tokens", "output_tokens", "cache_read_input_tokens", "cache_creation_input_tokens", "cache_creation", "service_tier", "server_tool_use")},
		{"claudeCacheCreation", reflect.TypeOf(claudeCacheCreation{}), keys("ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")},
		{"claudeServerToolUse", reflect.TypeOf(claudeServerToolUse{}), keys("web_search_requests")},
	} {
		for i := 0; i < g.typ.NumField(); i++ {
			f := g.typ.Field(i)
			tag := strings.Split(f.Tag.Get("json"), ",")[0]
			if tag == "" || tag == "-" {
				continue
			}
			if !g.handled[tag] {
				t.Errorf("%s: json tag %q is not decoded by its hand-written UnmarshalJSON; add a decoder entry", g.name, tag)
			}
		}
	}
}
