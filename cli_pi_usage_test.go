package main

import (
	"fmt"
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
