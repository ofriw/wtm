package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cli_activity_regression_test.go — ACTIVE contracts at the CLI boundary. Both
// defects here were invisible to parser-only tests because a valid encoding or
// a human prompt text changed the classified turn.

type activityStatusRow struct {
	Path          string `json:"path"`
	ActiveSeconds *int64 `json:"activeSeconds"`
}

func statusActivityRow(t *testing.T, repo string) activityStatusRow {
	t.Helper()
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	var rows []activityStatusRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out)
	}
	return findActivityRow(t, rows, repo)
}

func findActivityRow(t *testing.T, rows []activityStatusRow, path string) activityStatusRow {
	t.Helper()
	base := filepath.Base(path)
	for _, row := range rows {
		if filepath.Base(row.Path) == base {
			return row
		}
	}
	t.Fatalf("status lost %s in %+v", path, rows)
	return activityStatusRow{}
}

// writePiSession writes one Pi transcript under the sandbox session dir.
func writePiSession(t *testing.T, repo, name string, lines ...string) {
	t.Helper()
	path := mkSession(t, repo, name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf("{\"id\":%q,\"cwd\":%q}\n", name, repo) + strings.Join(lines, "\n") + "\n"
	writeFile(t, path, body, 0o644)
}

func assertModelSeconds(t *testing.T, models map[string]any, name string, want float64) {
	t.Helper()
	if got := subscriptionObject(t, models[name])["activeSeconds"]; got != want {
		t.Fatalf("%s activeSeconds = %#v, want %v", name, got, want)
	}
}

func assertNoModelAbove(t *testing.T, models map[string]any, limit float64) {
	t.Helper()
	for name := range models {
		if got, ok := subscriptionObject(t, models[name])["activeSeconds"].(float64); ok && got > limit {
			t.Errorf("%s activeSeconds = %v, want no more than %v", name, got, limit)
		}
	}
}

// TestThreeTurnAnswerModelAttribution pins that a same-model chain of
// point events must never bridge a turn answered by another model. Three
// ten-second turns answer A, B, A; only A records usage. The report must give
// A both its turns (20s), B the middle turn (10s), and never let a chain of
// A's points cover B's turn.
func TestThreeTurnAnswerModelAttribution(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "a-b-a",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"one"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"A","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"user","content":"two"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:20Z","message":{"role":"assistant","model":"B","stopReason":"stop"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:20Z","message":{"role":"user","content":"three"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:30Z","message":{"role":"assistant","model":"A","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
	)
	pi := piUsageOfOnlyWorktree(t, repo)
	if got := pi["activeSeconds"]; got != float64(30) {
		t.Fatalf("harness activeSeconds = %#v, want 30", got)
	}
	models := subscriptionObject(t, pi["models"])
	assertModelSeconds(t, models, "A", 20)
	assertModelSeconds(t, models, "B", 10)
	assertNoModelAbove(t, models, 30)
}

// TestPre2000ClockAccepted pins that plausibility belongs to
// transcript timestamps alone, so the nowEnv clock seam accepts a pre-2000
// value without the misleading "not RFC3339" warning.
func TestPre2000ClockAccepted(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	t.Setenv(nowEnv, "1999-12-31T00:00:00Z")
	writePiSession(t, repo, "pre2000-clock",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:30Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
	)
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if strings.Contains(stderr, "not RFC3339") {
		t.Errorf("stderr must not warn for a pre-2000 clock:\n%s", stderr)
	}
	if !strings.Contains(out, "\"path\"") {
		t.Errorf("status JSON must still report the worktree:\n%s", out)
	}
}

// TestPiActivityKeepsSpacedMessageKey pins the encoder boundary: the same user
// record encoded with and without a space before the colon must both open a
// turn. Each worktree is separate, so one miss cannot hide behind the other.
func TestPiActivityKeepsSpacedMessageKey(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	stamp := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	type fixture struct {
		name string
		key  string
		wt   string
	}
	fixtures := []fixture{
		{name: "compact", key: `"message":`},
		{name: "spaced", key: `"message" :`},
	}
	for i := range fixtures {
		f := &fixtures[i]
		f.wt = filepath.Join(t.TempDir(), f.name)
		gitWorktreeAdd(t, repo, f.wt, "feat-"+f.name, "main")
		path := mkSession(t, f.wt, f.name, stamp)
		body := fmt.Sprintf("{\"id\":%q,\"cwd\":%q}\n", f.name, f.wt) +
			fmt.Sprintf(`{"type":"message","timestamp":"2026-01-01T00:00:00Z",%s{"role":"user","content":[{"type":"text","text":"hi"}]}}`+"\n"+
				`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}`+"\n", f.key)
		writeFile(t, path, body, 0o644)
	}
	for _, f := range fixtures {
		row := statusActivityRow(t, f.wt)
		if row.ActiveSeconds == nil || *row.ActiveSeconds != 60 {
			t.Errorf("%s activeSeconds = %v, want 60", f.name, row.ActiveSeconds)
		}
	}
}

// TestClaudeHumanTextToolResultEndsTurn pins the block-type boundary: a human
// prompt whose text is exactly tool_result must close the open turn, so the
// two-hour idle gap never counts as work.
func TestClaudeHumanTextToolResultEndsTurn(t *testing.T) {
	sandbox(t)
	pinRates(t)
	repo := nativeRepo(t)
	path := mkClaudeSession(t, repo, "tool-result-text", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf(`{"type":"queue-operation","cwd":null}
{"type":"user","timestamp":"2026-01-01T00:00:00Z","cwd":%q,"message":{"role":"user","content":"start"}}
{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","stop_reason":"tool_use","usage":{"input_tokens":100000}}}
{"type":"user","timestamp":"2026-01-01T02:00:00Z","message":{"role":"user","content":[{"type":"text","text":"tool_result"}]}}
{"type":"assistant","timestamp":"2026-01-01T02:01:00Z","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-4-6","stop_reason":"end_turn","usage":{"input_tokens":100000}}}
`, repo)
	writeFile(t, path, body, 0o644)
	row := statusActivityRow(t, repo)
	if row.ActiveSeconds == nil || *row.ActiveSeconds != 61 {
		got := int64(-1)
		if row.ActiveSeconds != nil {
			got = *row.ActiveSeconds
		}
		t.Fatalf("activeSeconds = %d, want 61 (1s tool run + 60s second turn)", got)
	}
}

// TestFutureStampsClampActiveTime pins the report clock: a record dated after
// the report instant cannot claim time that has not elapsed, and the table and
// the JSON read the same clamped total.
func TestFutureStampsClampActiveTime(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	now := time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)
	t.Setenv(nowEnv, now.Format(time.RFC3339))
	path := mkSession(t, repo, "future", now)
	body := fmt.Sprintf("{\"id\":\"future\",\"cwd\":%q}\n", repo) +
		`{"type":"message","timestamp":"2026-01-01T00:50:00Z","message":{"role":"user","content":"hi"}}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T02:00:00Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":1}}}}` + "\n"
	writeFile(t, path, body, 0o644)
	row := statusActivityRow(t, repo)
	// A ten-minute turn is all the time that had elapsed by the report instant.
	if row.ActiveSeconds == nil || *row.ActiveSeconds != 600 {
		t.Fatalf("activeSeconds = %v, want 600", row.ActiveSeconds)
	}
	out, stderr, rc := runWTM(t, repo, nil, "status")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if !strings.Contains(out, "10m") {
		t.Fatalf("table = %q, want the clamped 10m", out)
	}
}

// TestCorruptTimestampTimeExcludedFromActiveTime pins the corrupt-timestamp exclusion at the CLI boundary: a
// parseable-but-absurd timestamp (a Go zero time, an epoch-zero bug) is corrupt
// data and must never publish an exact ACTIVE figure. The corrupt record drops,
// the total turns partial, ACTIVE clamps to the remaining valid turn, and
// stderr discloses the drop.
func TestCorruptTimestampTimeExcludedFromActiveTime(t *testing.T) {
	sandbox(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	t.Setenv(nowEnv, now.Format(time.RFC3339))
	repo := nativeRepo(t)
	writePiSession(t, repo, "corrupt-time",
		`{"type":"message","timestamp":"0001-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"bash"}],"usage":{"cost":{"total":0.1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":"ok"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
	)
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if !strings.Contains(stderr, "dropped") || !strings.Contains(stderr, "partial") {
		t.Errorf("stderr must disclose the dropped corrupt record:\n%s", stderr)
	}
	row := statusRows(t, out)[0]
	if row["partial"] != true {
		t.Errorf("partial = %v, want true: corrupt time must taint the total", row["partial"])
	}
	if got, ok := row["activeSeconds"].(float64); !ok || got != 10 {
		t.Errorf("activeSeconds = %#v, want 10 (the valid turn only)", row["activeSeconds"])
	}
	table, _, rc := runWTM(t, repo, nil, "status")
	if rc != 0 {
		t.Fatalf("status exit %d", rc)
	}
	if strings.Contains(table, "292y") || strings.Contains(table, "9223372036") {
		t.Errorf("table must not publish an absurd ACTIVE:\n%s", table)
	}
	if !strings.Contains(table, "~10s") {
		t.Errorf("table must show the qualified clamped ACTIVE (~10s):\n%s", table)
	}
}

// piUsageOfOnlyWorktree reads the single worktree's Pi harness JSON object.
func piUsageOfOnlyWorktree(t *testing.T, repo string) map[string]any {
	t.Helper()
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	return subscriptionObject(t, subscriptionObject(t, rows[0]["usage"])["pi"])
}

// TestTurnOpeningAttribute pins how a turn's opening is attributed to models:
// work before a model's first event still reaches the answering model, the
// report clock bounds it, a spawned child keeps its own interval, and no model
// ever exceeds the harness total.
func TestTurnOpeningAttribute(t *testing.T) {
	user := `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`
	cases := []struct {
		name    string
		now     time.Time // zero = leave nowEnv unset
		lines   []string
		harness float64
		models  map[string]float64
		limit   float64 // >0 = assert no model above this
	}{
		{
			name: "model-covers-opening",
			lines: []string{
				user,
				`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m","stopReason":"toolUse","usage":{"cost":{"total":0.1}}}}`,
				`{"type":"message","timestamp":"2026-01-01T00:01:05Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
			},
			harness: 65,
			models:  map[string]float64{"m": 65},
		},
		{
			name: "clamped-opening",
			now:  time.Date(2026, 1, 1, 0, 1, 0, 0, time.UTC),
			lines: []string{
				user,
				`{"type":"message","timestamp":"2026-01-01T00:02:00Z","message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
			},
			harness: 60,
			models:  map[string]float64{"m": 60},
			limit:   60,
		},
		{
			name: "spawn-child",
			lines: []string{
				user,
				`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"parent","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"spawn"}],"usage":{"cost":{"total":0.1}}}}`,
				`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{"model":"child","stats":{"cost":0.5}}}}`,
				`{"type":"message","timestamp":"2026-01-01T00:01:05Z","message":{"role":"assistant","model":"parent","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
			},
			harness: 65,
			models:  map[string]float64{"child": 55},
			limit:   65,
		},
		{
			name: "usage-less-spawn",
			lines: []string{
				user,
				`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"parent","stopReason":"toolUse","content":[{"type":"toolCall","id":"c1","name":"spawn"}]}}`,
				`{"type":"message","timestamp":"2026-01-01T00:01:00Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{"model":"child","stats":{"cost":0.5}}}}`,
				`{"type":"message","timestamp":"2026-01-01T00:01:05Z","message":{"role":"assistant","model":"parent","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
			},
			harness: 65,
			models:  map[string]float64{"child": 55, "parent": 5},
			limit:   65,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			if !tc.now.IsZero() {
				t.Setenv(nowEnv, tc.now.Format(time.RFC3339))
			}
			repo := nativeRepo(t)
			writePiSession(t, repo, "opening-"+tc.name, tc.lines...)
			pi := piUsageOfOnlyWorktree(t, repo)
			if got := pi["activeSeconds"]; got != tc.harness {
				t.Fatalf("harness activeSeconds = %#v, want %v", got, tc.harness)
			}
			models := subscriptionObject(t, pi["models"])
			for name, want := range tc.models {
				assertModelSeconds(t, models, name, want)
			}
			if tc.limit > 0 {
				assertNoModelAbove(t, models, tc.limit)
			}
		})
	}
}

// TestTurnBoundaryKeepsAnsweringModelTime pins that an answer that closes one
// turn at the instant the next prompt opens another belongs to the turn it
// closed, so the next answer keeps its full turn.
func TestTurnBoundaryKeepsAnsweringModelTime(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "shared-boundary",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"one"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"assistant","model":"a","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"user","content":"two"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:10Z","message":{"role":"assistant","model":"b","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`,
	)
	pi := piUsageOfOnlyWorktree(t, repo)
	if got := pi["activeSeconds"]; got != float64(10) {
		t.Fatalf("harness activeSeconds = %#v, want 10", got)
	}
	models := subscriptionObject(t, pi["models"])
	assertModelSeconds(t, models, "a", 3)
	assertModelSeconds(t, models, "b", 7)
	assertNoModelAbove(t, models, 10)
}

// TestWarmupUsageDoesNotOwnTheTurnOpening pins that a cache-warming usage record
// is not an answer, so it cannot claim the turn's opening; the answering model
// keeps the whole turn. The warmup keeps only its own (point) span.
func TestWarmupUsageDoesNotOwnTheTurnOpening(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "warmup-opening",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"usage","timestamp":"2026-01-01T00:00:05Z","model":"warmup","usage":{"input":100,"cost":{"total":0.1}}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:30Z","message":{"role":"assistant","model":"answer","stopReason":"stop","usage":{"cost":{"total":0.2}}}}`,
	)
	pi := piUsageOfOnlyWorktree(t, repo)
	if got := pi["activeSeconds"]; got != float64(30) {
		t.Fatalf("harness activeSeconds = %#v, want 30", got)
	}
	models := subscriptionObject(t, pi["models"])
	assertModelSeconds(t, models, "answer", 30)
	assertModelSeconds(t, models, "warmup", 0)
}

// TestUsageLessAnswerStillProvesModelTime pins the other half of : a named
// assistant reply with no usage still proves model TIME, so ACTIVE never reads
// as unowned while the harness counts it.
func TestUsageLessAnswerStillProvesModelTime(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "usage-less-answer",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:30Z","message":{"role":"assistant","model":"answer","stopReason":"stop"}}`,
	)
	pi := piUsageOfOnlyWorktree(t, repo)
	if got := pi["activeSeconds"]; got != float64(30) {
		t.Fatalf("harness activeSeconds = %#v, want 30", got)
	}
	assertModelSeconds(t, subscriptionObject(t, pi["models"]), "answer", 30)
}

// TestUsageLessTurnStillProvesTimeAtCLI pins that a whole turn with zero usage
// events still proves its time (nonzero ACTIVE), never invents a free zero
// (absent human COST, null JSON cost), and never reads as partial. The
// transcripts reach the distinct len(events)==0 production path, so dropping an
// entire usage-less session fails here.
func TestUsageLessTurnStillProvesTimeAtCLI(t *testing.T) {
	cases := []struct {
		name    string
		seed    func(*testing.T, string)
		harness string
		model   string
	}{
		{"pi", func(t *testing.T, repo string) {
			writePiSession(t, repo, "usage-less",
				`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
				`{"type":"message","timestamp":"2026-01-01T00:00:30Z","message":{"role":"assistant","model":"answer","stopReason":"stop"}}`,
			)
		}, "pi", "answer"},
		{"claude", func(t *testing.T, repo string) {
			path := mkClaudeSession(t, repo, "usage-less", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
			body := fmt.Sprintf(`{"type":"queue-operation","cwd":null}
{"type":"user","timestamp":"2026-01-01T00:00:00Z","cwd":%q,"message":{"role":"user","content":"hi"}}
{"type":"assistant","timestamp":"2026-01-01T00:00:30Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","stop_reason":"end_turn"}}
`, repo)
			writeFile(t, path, body, 0o644)
		}, "claude", "claude-sonnet-4-6"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			repo := nativeRepo(t)
			tc.seed(t, repo)

			out, stderr, rc := runWTM(t, repo, nil, "status")
			if rc != 0 {
				t.Fatalf("status exit %d: %s", rc, stderr)
			}
			if !strings.Contains(out, "30s") {
				t.Errorf("table must keep the proven active time:\n%s", out)
			}
			if strings.Contains(out, "$") {
				t.Errorf("table must not price a usage-less turn:\n%s", out)
			}
			if strings.Contains(out, "~") || strings.Contains(out, "unreadable session records") {
				t.Errorf("usage-less turn must not read as partial:\n%s", out)
			}

			rows := subscriptionStatus(t, repo)
			if len(rows) != 1 {
				t.Fatalf("got %d worktrees, want 1", len(rows))
			}
			row := rows[0]
			if v, present := row["activeSeconds"]; !present || v != float64(30) {
				t.Errorf("activeSeconds = %#v (present %v), want 30", v, present)
			}
			if v, present := row["cost"]; !present || v != nil {
				t.Errorf("cost = %#v (present %v), want explicit null", v, present)
			}
			model := subscriptionObject(t, subscriptionObject(t, subscriptionObject(t, subscriptionObject(t, row["usage"])[tc.harness])["models"])[tc.model])
			if v, present := model["cost"]; !present || v != nil {
				t.Errorf("%s model cost = %#v (present %v), want explicit null", tc.model, v, present)
			}
		})
	}
}

// writePiTurns writes one Pi transcript of one-second turns on one model and
// returns the instant after its last turn, for the report clock.
func writePiTurns(t *testing.T, repo, name string, turns int) time.Time {
	t.Helper()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var b strings.Builder
	fmt.Fprintf(&b, "{\"id\":%q,\"cwd\":%q}\n", name, repo)
	for i := range turns {
		at := base.Add(time.Duration(2*i) * time.Second)
		fmt.Fprintf(&b, `{"type":"message","timestamp":%q,"message":{"role":"user","content":"hi"}}`+"\n", at.Format(time.RFC3339))
		fmt.Fprintf(&b, `{"type":"message","timestamp":%q,"message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`+"\n", at.Add(time.Second).Format(time.RFC3339))
	}
	writeFile(t, mkSession(t, repo, name, base), b.String(), 0o644)
	return base.Add(time.Duration(2*turns) * time.Second)
}

// TestPiStatusLargeTranscriptTotals pins ACTIVE attribution stays correct at the
// large-transcript boundary (20k turns): the walk must not drop or fabricate
// time as the transcript grows. The speed intent lives in BenchmarkLargeTranscriptFold;
// a wall-clock threshold would double-count git and process startup, so it has
// no home here. The CLI subprocess is bounded by runWTM's timeout.
func TestPiStatusLargeTranscriptTotals(t *testing.T) {
	const turns = 20000
	sandbox(t)
	repo := nativeRepo(t)
	t.Setenv(nowEnv, writePiTurns(t, repo, "large", turns).Format(time.RFC3339))
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	row := statusRows(t, out)[0]
	if got := row["activeSeconds"]; got != float64(turns) {
		t.Fatalf("activeSeconds = %v, want %d", got, turns)
	}
	if got := statusModel(t, row, piAgent.name, "m")["activeSeconds"]; got != float64(turns) {
		t.Fatalf("model activeSeconds = %v, want %d", got, turns)
	}
}

// BenchmarkLargeTranscriptFold is the speed intent of the former wall-clock gate:
// folding 20k one-second turns must stay linear (the old per-turn rescan took
// ~13s). It exercises the real fold path in-process, without binary startup.
func BenchmarkLargeTranscriptFold(b *testing.B) {
	const turns = 20000
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var sb strings.Builder
	for i := range turns {
		at := base.Add(time.Duration(2*i) * time.Second)
		fmt.Fprintf(&sb, `{"type":"message","timestamp":%q,"message":{"role":"user","content":"hi"}}`+"\n", at.Format(time.RFC3339))
		fmt.Fprintf(&sb, `{"type":"message","timestamp":%q,"message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":0.1}}}}`+"\n", at.Add(time.Second).Format(time.RFC3339))
	}
	parsed, err := piUsageEvents(bufio.NewReader(strings.NewReader(sb.String())))
	if err != nil {
		b.Fatal(err)
	}
	now := base.Add(time.Duration(2*turns) * time.Second)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var u worktreeUsage
		u.addBilledSession("pi", parsed.events, parsed.activity, nil, func(usageEvent) pricedEvent { return unpriced() }, now)
		if u.active() != time.Duration(turns)*time.Second {
			b.Fatalf("active = %v", u.active())
		}
	}
}

// TestActivityOnlyDetailSkipsEmptyModelGrid pins that activity without usage
// keeps the summary and any legend, but prints no header-only model grid and no
// width warning for token columns the empty grid cannot render.
func TestActivityOnlyDetailSkipsEmptyModelGrid(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "model-less-answer",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"assistant","stopReason":"stop"}}`,
	)
	out, stderr, rc := runWTM(t, repo, []string{"COLUMNS=36"}, "status")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if !strings.Contains(out, "active time") || !strings.Contains(out, "3s") {
		t.Errorf("activity-only detail must keep the summary and its active time:\n%s", out)
	}
	if strings.Contains(out, "MODEL") {
		t.Errorf("empty model set must not print a header-only grid:\n%s", out)
	}
	if strings.Contains(stderr, "hid in,") || strings.Contains(stderr, "hit%") {
		t.Errorf("empty model grid must not warn about hidden token columns:\n%s", stderr)
	}
}

// TestAnsiModelLabelStaysPlain pins that a usage-free assistant answer whose
// model carries an ANSI escape renders a plain single-line table label, while
// the JSON report keeps the raw model identity. Styling input must never come
// from transcript bytes.
func TestAnsiModelLabelStaysPlain(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	// The transcript embeds the model as a JSON-escaped string, which decodes to
	// real ANSI control bytes in the parsed record.
	escaped := "marker\\u001b[31mPROBE\\u001b[0m"
	raw := "marker\u001b[31mPROBE\u001b[0m"
	writePiSession(t, repo, "ansi-model",
		`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"user","content":"hi"}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:03Z","message":{"role":"assistant","model":"`+escaped+`","stopReason":"stop"}}`,
	)
	out, stderr, rc := runWTM(t, repo, []string{"COLUMNS=60"}, "status", "--color", "always")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	// The table must render the label as plain display text, not emit the raw
	// escape bytes (which would recolor the report).
	if strings.Contains(out, "\u001b[31m") {
		t.Errorf("table emitted raw ANSI bytes:\n%q", out)
	}
	if !strings.Contains(out, "markerPROBE") {
		t.Errorf("table must show the sanitized model label:\n%s", out)
	}
	// JSON keeps the raw identity untouched.
	pi := piUsageOfOnlyWorktree(t, repo)
	models := subscriptionObject(t, pi["models"])
	if models[raw] == nil {
		t.Errorf("JSON must keep the raw model identity; got keys %v", models)
	}
}

// TestPiUnreadableSpawnModelStaysUnknown pins that a legacy spawn child whose
// model figure is not a string must never render as a literal "�unreadable"
// money row carrying the child's real cost. It warns once and reports the
// model as unknown instead.
func TestPiUnreadableSpawnModelStaysUnknown(t *testing.T) {
	sandbox(t)
	repo := nativeRepo(t)
	writePiSession(t, repo, "corrupt-spawn-model",
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","model":"m-parent","content":[{"type":"toolCall","id":"c1","name":"spawn"}]}}`,
		`{"type":"message","timestamp":"2026-01-01T00:00:35Z","message":{"role":"toolResult","toolCallId":"c1","toolName":"spawn","details":{"mode":"spawn","results":[{"prompt":"p","usage":{"model":123,"cost":0.2},"output":"o"}]}}}`,
	)
	out, stderr, rc := runWTM(t, repo, []string{"COLUMNS=60"}, "status", "--color", "always")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if strings.Contains(out, "�unreadable") {
		t.Errorf("table must not render a literal unreadable model row:\n%s", out)
	}
	if n := strings.Count(stderr, "model figure unreadable"); n != 1 {
		t.Errorf("model-unreadable warnings = %d, want 1:\n%s", n, stderr)
	}
	pi := piUsageOfOnlyWorktree(t, repo)
	models := subscriptionObject(t, pi["models"])
	// The child's real money stays visible under the unknown model, never
	// attached to a garbage name.
	unknown := subscriptionObject(t, models["unknown"])
	if got := unknown["cost"]; got != 0.2 {
		t.Errorf("unknown model cost = %v, want 0.2 (the child's real money preserved)", got)
	}
	for name := range models {
		if strings.Contains(name, "unreadable") {
			t.Errorf("JSON must not carry an unreadable model key: %q", name)
		}
	}
}

// TestClaudeUnreadableServiceTierWarns pins that a Claude record whose
// service_tier figure is not a string must warn on stderr instead of silently
// returning nil rates. The cost reads unknown, never an exact figure.
func TestClaudeUnreadableServiceTierWarns(t *testing.T) {
	usage := `{"input_tokens":100,"output_tokens":10,"service_tier":123}`
	model, stderr := claudeCatalogStatus(t, claudeSiblingRates, usage)
	if n := strings.Count(stderr, "service_tier figure unreadable"); n != 1 {
		t.Errorf("service_tier warnings = %d, want 1:\n%s", n, stderr)
	}
	if got := model["cost"]; got != nil {
		t.Errorf("model cost = %v, want null (unknown): a non-string tier must never price exactly", got)
	}
}
