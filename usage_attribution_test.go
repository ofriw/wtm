package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// usage_attribution_test.go — a session belongs to the worktree that contains
// its cwd, not only to a worktree whose path equals it exactly. Display usage
// follows the deepest containing worktree, so a nested checkout is never
// double-counted. Destructive purge keeps using the exact index.

func refPaths(refs []sessionRef) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.path
	}
	return out
}

func TestSessionsForWorktreeAttributesDescendants(t *testing.T) {
	base := t.TempDir()
	root := canonical(filepath.Join(base, "repo"))
	sub := canonical(filepath.Join(root, "website"))
	nested := canonical(filepath.Join(root, "nested"))
	deep := canonical(filepath.Join(nested, "deep"))
	other := canonical(filepath.Join(base, "other"))
	idx := sessionIndex{
		root:   {{path: "root"}},
		sub:    {{path: "sub"}},
		nested: {{path: "nested"}},
		deep:   {{path: "deep"}},
		other:  {{path: "other"}},
	}
	wts := []worktree{{Path: root}, {Path: nested}}

	if got, want := refPaths(sessionsForWorktree(root, wts, idx)), []string{"root", "sub"}; !slices.Equal(got, want) {
		t.Fatalf("root sessions = %v, want %v", got, want)
	}
	if got, want := refPaths(sessionsForWorktree(nested, wts, idx)), []string{"nested", "deep"}; !slices.Equal(got, want) {
		t.Fatalf("nested sessions = %v, want %v", got, want)
	}
	if got, want := refPaths(sessionsForWorktree(other, wts, idx)), []string{"other"}; !slices.Equal(got, want) {
		t.Fatalf("other sessions = %v, want %v", got, want)
	}
}

func TestSessionsForWorktreeIgnoresSiblingPrefix(t *testing.T) {
	base := t.TempDir()
	root := canonical(filepath.Join(base, "repo"))
	sibling := canonical(filepath.Join(base, "repo-feature"))
	idx := sessionIndex{root: {{path: "root"}}, sibling: {{path: "sibling"}}}
	got := refPaths(sessionsForWorktree(root, []worktree{{Path: root}}, idx))
	if want := []string{"root"}; !slices.Equal(got, want) {
		t.Fatalf("sessions = %v, want %v", got, want)
	}
}

// piTurnBody is one Pi session with a prompt at start and a stop at end, so its
// causal block is exactly the interval and its billed cost is the given value.
func piTurnBody(cwd, start, end string, cost float64) string {
	return strings.Join([]string{
		fmt.Sprintf(`{"type":"session","timestamp":%q,"cwd":%q}`, start, cwd),
		fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"user","content":"hi"}}`, start),
		fmt.Sprintf(`{"type":"message","timestamp":%q,"message":{"role":"assistant","model":"m","stopReason":"stop","usage":{"cost":{"total":%v}}}}`, end, cost),
	}, "\n") + "\n"
}

func writeTurnSession(t *testing.T, dir, name, cwd, start, end string, cost float64) sessionRef {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, piTurnBody(cwd, start, end, cost), 0o644)
	return sessionRef{path: path, harness: piAgent.name}
}

func TestSessionLastUsedCountsDescendant(t *testing.T) {
	base := t.TempDir()
	root := canonical(filepath.Join(base, "repo"))
	sub := canonical(filepath.Join(root, "website"))
	dir := t.TempDir()
	exact := filepath.Join(dir, "exact.jsonl")
	descendant := filepath.Join(dir, "descendant.jsonl")
	writeFile(t, exact, "exact\n", 0o644)
	writeFile(t, descendant, "descendant\n", 0o644)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	newest := old.Add(2 * time.Hour)
	if err := os.Chtimes(exact, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(descendant, newest, newest); err != nil {
		t.Fatal(err)
	}
	idx := sessionIndex{root: {{path: exact}}, sub: {{path: descendant}}}
	got, err := sessionLastUsed(root, idx, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(newest) {
		t.Fatalf("last used = %v, want the descendant's %v", got, newest)
	}
}

func TestBuildUsageForCountsDescendantSessions(t *testing.T) {
	base := t.TempDir()
	root := canonical(filepath.Join(base, "repo"))
	sub := canonical(filepath.Join(root, "website"))
	sessions := t.TempDir()
	rootRef := writeTurnSession(t, sessions, "root.jsonl", root, "2026-01-01T00:00:00Z", "2026-01-01T00:05:00Z", 1)
	subRef := writeTurnSession(t, sessions, "sub.jsonl", sub, "2026-01-01T00:10:00Z", "2026-01-01T00:17:00Z", 1)
	idx := sessionIndex{root: {rootRef}, sub: {subRef}}

	exact, err := buildUsage(root, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := exact.active().Minutes(), 5.0; got != want {
		t.Fatalf("exact active = %v, want %v", got, want)
	}
	if got, want := exact.cost().billed.amount, 1.0; got != want {
		t.Fatalf("exact cost = %v, want %v", got, want)
	}

	all, err := buildUsageFor(root, []worktree{{Path: root}}, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := all.active().Minutes(), 12.0; got != want {
		t.Fatalf("descendant active = %v, want %v", got, want)
	}
	if got, want := all.cost().billed.amount, 2.0; got != want {
		t.Fatalf("descendant cost = %v, want %v", got, want)
	}

	nested, err := buildUsageFor(root, []worktree{{Path: root}, {Path: sub}}, idx, nil, subscriptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := nested.active().Minutes(), 5.0; got != want {
		t.Fatalf("nested-worktree active = %v, want %v (no double count)", got, want)
	}
}
