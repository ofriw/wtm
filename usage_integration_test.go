package main

import (
	"fmt"
	"math"
	"path/filepath"
	"testing"
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

func TestBuildUsageClaudeUnclassifiedCacheWriteIsUnknown(t *testing.T) {
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name,
		claudeUsageLine(`{"input_tokens":1000,"cache_creation_input_tokens":100}`))
	u, err := buildUsage(dir, idx, testRates(t), subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := worktree{Path: dir, Usage: &u}
	if got := cellCost(w, columnContext{}); got != "unknown" {
		t.Errorf("cost = %q, want unknown", got)
	}
	if v := w.cost().billed.jsonValue(); v != nil {
		t.Errorf("json cost = %v, want null", v)
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
}

// piUsageLine builds one self-priced Pi record with the given cost total, so a
// test drives the real parser rather than a hand-built event.
func piUsageLine(total float64) string {
	return fmt.Sprintf(`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":%v}}}}`, total)
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
