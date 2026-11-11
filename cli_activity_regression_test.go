package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
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
