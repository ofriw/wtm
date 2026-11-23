package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func seedLegacySpawnFork(t *testing.T, repo, name string) {
	t.Helper()
	path := mkSession(t, repo, name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf("{\"type\":\"session\",\"cwd\":%q}\n", repo)
	body += `{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"results":[{"usage":{"model":"m","cost":0.1}},{"usage":{"model":"m","cost":0.1}}]}}}` + "\n"
	writeFile(t, path, body, 0o644)
}

// Identical siblings are separate work; a fork is not additional work.
func TestPiBinaryLegacyBatchKeepsSiblingsAcrossFork(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	seedLegacySpawnFork(t, repo, "original")
	seedLegacySpawnFork(t, repo, "fork")
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	h := subscriptionObject(t, subscriptionObject(t, rows[0]["usage"])["pi"])
	m := subscriptionObject(t, subscriptionObject(t, h["models"])["m"])
	for level, object := range map[string]map[string]any{"worktree": rows[0], "harness": h, "model": m} {
		if object["cost"] != 0.2 {
			t.Errorf("%s cost = %v, want 0.2: each sibling bills once across forks", level, object["cost"])
		}
	}
}

// TestPiCostGuard pins the money guard end to end: a reported total outside the
// sane range never renders as dollars and warns with its field and cause; a sane
// total stays exact, even when its token count is rejected.
type piCostCase struct {
	name, total, cause, tokens string
	corrupt                    bool
	want                       any
}

func TestPiCostGuard(t *testing.T) {
	for _, tc := range []piCostCase{
		{"negative", "-100", "negative", "", true, nil},
		{"overflow", "1e300", "above 1e+06 USD", "", true, nil},
		{"valid", "0.5", "", "", false, 0.5},
		{"corrupt-token", "0.5", "", `"input":-1000`, false, 0.5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			repo := nativeRepo(t)
			path := mkSession(t, repo, "cost-"+tc.name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			writeFile(t, path, piCostBody(repo, tc.total, tc.tokens), 0o644)
			out, stderr, rc := runWTM(t, repo, nil, "status")
			if rc != 0 {
				t.Fatalf("status exit %d: %s", rc, stderr)
			}
			assertPiCostTable(t, out, stderr, tc)
			assertPiCostJSON(t, repo, tc)
		})
	}
}

// assertPiCostTable pins the table and its disclosure: bad money reads unknown
// with a warning naming its field and cause, good money renders exactly and
// silently. The cause text is the warning identity, so no session path rides it.
func assertPiCostTable(t *testing.T, out, stderr string, tc piCostCase) {
	t.Helper()
	switch {
	case tc.corrupt:
		if !strings.Contains(out, "unknown") || strings.Contains(out, "$") {
			t.Fatalf("table must read unknown with no dollars:\n%s", out)
		}
		for _, want := range []string{"cost.total", tc.cause} {
			if !strings.Contains(stderr, want) {
				t.Errorf("stderr misses %q:\n%s", want, stderr)
			}
		}
	case !strings.Contains(out, "$0.50") || strings.Contains(stderr, "cost.total"):
		t.Fatalf("sane cost must render exactly and silently:\n%s%s", out, stderr)
	}
	if tc.tokens != "" {
		if strings.Contains(out, "-1000") || !strings.Contains(out, "unknown") ||
			!strings.Contains(stderr, "token input: negative") {
			t.Fatalf("corrupt count must read unknown and disclose its cause:\n%s%s", out, stderr)
		}
	}
}

// assertPiCostJSON pins the same verdict at every JSON level, including a null
// token count for a rejected class.
func assertPiCostJSON(t *testing.T, repo string, tc piCostCase) {
	t.Helper()
	rows := subscriptionStatus(t, repo)
	pi := subscriptionObject(t, subscriptionObject(t, rows[0]["usage"])["pi"])
	model := subscriptionObject(t, subscriptionObject(t, pi["models"])["m"])
	for level, object := range map[string]map[string]any{"worktree": rows[0], "harness": pi, "model": model} {
		if cost, present := object["cost"]; !present || cost != tc.want {
			t.Errorf("%s cost = %#v (present %v), want %v", level, cost, present, tc.want)
		}
	}
	if tc.tokens != "" {
		tiers := subscriptionObject(t, model["tiers"])
		in := subscriptionObject(t, tiers["input"])
		// Presence plus value: an omitted rejected key must not read as a null.
		if v, present := in["tokens"]; !present || v != nil {
			t.Errorf("input tokens = %#v (present %v), want explicit null", v, present)
		}
	}
}

// piUsageBody writes a human prompt that opens the turn, then one assistant
// record with the given usage object, so cost, counts, and ACTIVE are all
// observable from one transcript.
func piUsageBody(repo, usage string) string {
	return fmt.Sprintf("{\"type\":\"session\",\"cwd\":%q}\n", repo) +
		"{\"type\":\"message\",\"timestamp\":\"2026-01-01T00:00:00Z\",\"message\":{\"role\":\"user\",\"content\":\"go\"}}\n" +
		fmt.Sprintf("{\"type\":\"message\",\"timestamp\":\"2026-01-01T00:00:01Z\",\"message\":{\"role\":\"assistant\",\"model\":\"m\",\"usage\":%s}}\n", usage)
}

// piCostBody writes the record TestPiCostGuard prices: a stated total plus any
// extra token figures.
func piCostBody(repo, total, tokens string) string {
	if tokens != "" {
		tokens += ","
	}
	return piUsageBody(repo, fmt.Sprintf(`{%s"cost":{"total":%s}}`, tokens, total))
}

// TestPiCorruptTierWithoutTotalStaysUnknown pins that a harness-reported class
// price is checked even when the record states no total. The corrupt tier warns
// by field name and reads unknown, its valid sibling keeps its price, and the
// total the record never stated stays unknown instead of borrowing the split.
func TestPiCorruptTierWithoutTotalStaysUnknown(t *testing.T) {
	sandbox(t)
	pinClock(t)
	repo := nativeRepo(t)
	writeFile(t, mkSession(t, repo, "tier-only", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		piUsageBody(repo, `{"input":2,"cost":{"input":-1,"output":0.2}}`), 0o644)
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if n := strings.Count(stderr, "tierCost.input: negative"); n != 1 {
		t.Errorf("tier warnings = %d, want 1:\n%s", n, stderr)
	}
	row := statusRows(t, stdout)[0]
	model := statusModel(t, row, piAgent.name, "m")
	for level, cost := range map[string]any{"worktree": row["cost"], "model": model["cost"]} {
		if cost != nil {
			t.Errorf("%s cost = %v, want null: no stated total, no total", level, cost)
		}
	}
	if in := statusTier(t, model, "input"); in["cost"] != nil || in["tokens"] != float64(2) {
		t.Errorf("input tier = %+v, want 2 tokens and an unknown cost", in)
	}
	if out := statusTier(t, model, "output"); out["cost"] != 0.2 {
		t.Errorf("output tier = %+v, want the valid sibling to keep its 0.2", out)
	}
}

// TestArchitectureProbeDuplicate pins the pre-election price disclosure: a bad
// price in a forked transcript (a duplicate of the winning record) must warn
// once per cause without mutating the event, so the valid sibling still wins
// dedupe and prices unchanged.
func TestArchitectureProbeDuplicate(t *testing.T) {
	sandbox(t)
	pinClock(t)
	repo := nativeRepo(t)
	body := fmt.Sprintf("{\"type\":\"session\",\"cwd\":%q}\n", repo)
	body += `{"type":"message","id":"e1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":-1}}}}` + "\n"
	body += `{"type":"message","id":"e1","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":0.19}}}}` + "\n"
	writeFile(t, mkSession(t, repo, "probe-dup", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)), body, 0o644)
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if n := strings.Count(stderr, "cost.total: negative"); n != 1 {
		t.Errorf("warnings for the rejected -1 = %d, want exactly 1:\n%s", n, stderr)
	}
	row := statusRows(t, stdout)[0]
	if row["cost"] != 0.19 {
		t.Errorf("worktree cost = %v, want 0.19 (the valid sibling must win unchanged)", row["cost"])
	}
	if model := statusModel(t, row, piAgent.name, "m"); model["cost"] != 0.19 {
		t.Errorf("model cost = %v, want 0.19", model["cost"])
	}
}

// TestPiNegativeZeroAmountRejected pins f5 at the Pi transcript boundary: a
// negative underflow (-1e-999) rounds to negative zero and must read unknown
// with a warning, never as an exact free figure; an explicit numeric zero stays
// a valid, exact free class.
func TestPiNegativeZeroAmountRejected(t *testing.T) {
	for _, tc := range []struct {
		name, total, warn string
		want              any
	}{
		{"underflow", "-1e-999", "cost.total: not a number", nil},
		{"explicit-zero", "0", "", 0.0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			pinClock(t)
			repo := nativeRepo(t)
			writeFile(t, mkSession(t, repo, "negzero-"+tc.name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
				piUsageBody(repo, fmt.Sprintf(`{"cost":{"total":%s}}`, tc.total)), 0o644)
			stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
			if rc != 0 {
				t.Fatalf("status exit %d: %s", rc, stderr)
			}
			row := statusRows(t, stdout)[0]
			if tc.want == nil {
				if row["cost"] != nil {
					t.Errorf("worktree cost = %v, want unknown", row["cost"])
				}
				if n := strings.Count(stderr, tc.warn); n != 1 {
					t.Errorf("warnings for %q = %d, want exactly 1:\n%s", tc.warn, n, stderr)
				}
			} else {
				if row["cost"] != tc.want {
					t.Errorf("worktree cost = %v, want the exact free %v", row["cost"], tc.want)
				}
				if strings.Contains(stderr, "cost.total") {
					t.Errorf("explicit zero must not warn:\n%s", stderr)
				}
			}
		})
	}
}

// TestPiUnreadableFigureKeepsSiblings pins that a number this build cannot read
// voids only its own field. The charge, the surviving counts, and the turn's
// active time all stay billed, and one warning names the rejected figure.
func TestPiUnreadableFigureKeepsSiblings(t *testing.T) {
	for _, tc := range []struct {
		name, usage, cause string
		cost               any
		input              any
	}{
		{"int-overflow", `{"input":9223372036854775808,"output":7,"cost":{"total":0.5}}`, "token input: unreadable", 0.5, nil},
		{"int-fraction", `{"input":1.5,"output":7,"cost":{"total":0.5}}`, "token input: unreadable", 0.5, nil},
		{"total-out-of-range", `{"input":9,"output":7,"cost":{"total":1e999}}`, "cost.total: number out of range", nil, float64(9)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			pinClock(t)
			repo := nativeRepo(t)
			writeFile(t, mkSession(t, repo, "unreadable-"+tc.name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
				piUsageBody(repo, tc.usage), 0o644)
			stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
			if rc != 0 {
				t.Fatalf("status exit %d: %s", rc, stderr)
			}
			if n := strings.Count(stderr, tc.cause); n != 1 {
				t.Errorf("warnings for %q = %d, want 1:\n%s", tc.cause, n, stderr)
			}
			row := statusRows(t, stdout)[0]
			if row["cost"] != tc.cost {
				t.Errorf("worktree cost = %v, want %v", row["cost"], tc.cost)
			}
			model := statusModel(t, row, piAgent.name, "m")
			if got := statusTier(t, model, "input")["tokens"]; got != tc.input {
				t.Errorf("input tier tokens = %v, want %v", got, tc.input)
			}
			if got := statusTier(t, model, "output")["tokens"]; got != float64(7) {
				t.Errorf("output tier tokens = %v, want the readable 7", got)
			}
			// The turn stays on the timeline: prompt to answer still counts.
			if secs, ok := row["activeSeconds"].(float64); !ok || secs < 1 {
				t.Errorf("activeSeconds = %v, want the turn to stay billed", row["activeSeconds"])
			}
		})
	}
}

// TestPiCorruptCountsWarnOncePerCause pins the f9 worker dedupe: many identical
// corrupt token counts in one transcript retain one warning per cause, so a
// large corrupt session stays memory-bound in the worker.
func TestPiCorruptCountsWarnOncePerCause(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	lines := make([]string, 0, 41)
	for i := 0; i < 20; i++ {
		at := fmt.Sprintf("2026-01-01T00:%02d:00Z", i)
		lines = append(lines,
			`{"type":"message","timestamp":"`+at+`","message":{"role":"user","content":"hi"}}`,
			`{"type":"message","timestamp":"2026-01-01T00:`+fmt.Sprintf("%02d", i)+`:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"input":-5,"cost":{"total":0.1}}}}`,
		)
	}
	writePiSession(t, repo, "corrupt-counts", lines...)
	_, stderr, rc := runWTM(t, repo, nil, "status")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if n := strings.Count(stderr, "token input: negative"); n != 1 {
		t.Errorf("corrupt token warnings = %d, want 1:\n%s", n, stderr)
	}
}
