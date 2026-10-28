package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanWorktreesWarningOrder pins the diagnostic contract: workers may run
// in any order, but warnings emit in input order, so identical inventories
// produce identical stderr. Malformed MCP configs give every worktree exactly
// one warning without needing a git repository.
func TestScanWorktreesWarningOrder(t *testing.T) {
	const worktrees, runs = 8, 10
	in := discoveryInputs{raw: make([]gworktree, worktrees)}
	want := make([]string, worktrees)
	for i := range in.raw {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, projectMCPFile), "{broken\n", 0o644)
		in.raw[i] = gworktree{Path: dir}
		want[i] = canonical(dir)
	}
	for run := range runs {
		events := make(chan progressEvent, 4*worktrees+8)
		p := &progress{ctx: context.Background(), events: events}
		scanWorktrees(in, p)
		close(events)
		var notices []string
		for ev := range events {
			if ev.kind == evNotice {
				notices = append(notices, ev.text)
			}
		}
		if len(notices) != worktrees {
			t.Fatalf("run %d: %d notices, want %d", run, len(notices), worktrees)
		}
		for i, text := range notices {
			if !strings.Contains(text, filepath.Join(want[i], projectMCPFile)) {
				t.Fatalf("run %d: notice %d = %q, want warning for %s", run, i, text, want[i])
			}
		}
	}
}
