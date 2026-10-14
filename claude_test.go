package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// claude_test.go — Tier-1 contracts for the Claude Code harness descriptor
// against the real config-dir layout and real git repos. No mocks.

func TestClaudeConfigDir(t *testing.T) {
	sb := sandbox(t)
	if got := claudeAgent.configDir(); got != sb.ClaudeDir {
		t.Fatalf("claudeAgent.configDir() = %q, want %q", got, sb.ClaudeDir)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, want := claudeAgent.configDir(), filepath.Join(sb.Home, ".claude"); got != want {
		t.Fatalf("default claudeAgent.configDir() = %q, want %q", got, want)
	}
}

// TestCopyHarnessClaude pins the Claude seed rules: CLAUDE.local.md and
// git-ignored .claude files carry over; tracked, unignored and the nested
// .claude/worktrees tree never do; existing files are never overwritten.
func TestCopyHarnessClaude(t *testing.T) {
	src := initRepo(t, "main")
	dst := initRepo(t, "main")
	ignore := "CLAUDE.local.md\n.claude/settings.local.json\n.claude/keep.txt\n.claude/worktrees/\n"
	writeFile(t, filepath.Join(dst, ".gitignore"), ignore, 0o644)
	writeFile(t, filepath.Join(dst, ".claude", "tracked.txt"), "from-dst", 0o644)
	gitCommit(t, dst, "tracked claude file", ".gitignore", ".claude/tracked.txt")
	writeFile(t, filepath.Join(dst, ".claude", "keep.txt"), "keep", 0o644)

	writeFile(t, filepath.Join(src, "CLAUDE.local.md"), "local", 0o644)
	writeFile(t, filepath.Join(src, ".claude", "settings.local.json"), "{}", 0o644)
	writeFile(t, filepath.Join(src, ".claude", "keep.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".claude", "tracked.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".claude", "plain.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".claude", "worktrees", "nested", "x.txt"), "wt", 0o644)

	if err := copyHarness(src, dst); err != nil {
		t.Fatalf("copyHarness: %v", err)
	}
	claudeFile := func(rel string) string { return filepath.Join(dst, ".claude", rel) }
	if b, _ := os.ReadFile(filepath.Join(dst, "CLAUDE.local.md")); string(b) != "local" {
		t.Fatalf("CLAUDE.local.md = %q, want local", b)
	}
	if b, _ := os.ReadFile(claudeFile("settings.local.json")); string(b) != "{}" {
		t.Fatalf("settings.local.json = %q, want {}", b)
	}
	if b, _ := os.ReadFile(claudeFile("keep.txt")); string(b) != "keep" {
		t.Fatalf("existing dst file overwritten: %q", b)
	}
	if b, _ := os.ReadFile(claudeFile("tracked.txt")); string(b) != "from-dst" {
		t.Fatalf("dst-tracked file overwritten: %q", b)
	}
	if exists(claudeFile("plain.txt")) {
		t.Fatal("src-unignored file must not be copied")
	}
	if exists(claudeFile("worktrees")) {
		t.Fatal(".claude/worktrees must never be seeded")
	}
}

// TestIndexAndPurgeClaudeSessions pins recursive Claude session indexing and
// cleanup: owned sessions nested under subagents/ are removed, empty directories
// are cleaned, and another worktree's history survives.
func TestIndexAndPurgeClaudeSessions(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()
	other := t.TempDir()
	p1 := mkClaudeSession(t, wt, "s1", time.Now())
	sub := filepath.Join(claudeAgent.configDir(), "projects", "wt-nested", "s2", "subagents", "agent-1.jsonl")
	writeFile(t, sub, claudeTranscript(wt), 0o644)
	keep := mkClaudeSession(t, other, "s3", time.Now())

	idx := mustIndexSessions(t)
	for _, p := range []string{p1, sub, keep} {
		want := wt
		if p == keep {
			want = other
		}
		if !idxContains(idx, want, p) {
			t.Fatalf("session not indexed for %s: %s", want, p)
		}
	}

	n, err := purgeSessions(wt, idx)
	if err != nil {
		t.Fatalf("purgeSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	if exists(filepath.Dir(p1)) {
		t.Fatalf("group dir survived: %s", filepath.Dir(p1))
	}
	if exists(sub) {
		t.Fatalf("nested session survived recursive purge: %s", sub)
	}
	if !exists(keep) {
		t.Fatalf("other worktree session was purged: %s", keep)
	}
}

// TestPurgeSessionsSharedGroup pins the lossy-encoding safety net: when two
// worktrees' cwds index into ONE group dir, purging one must not recursively
// delete the dir — the other worktree's session must survive.
func TestPurgeSessionsSharedGroup(t *testing.T) {
	sandbox(t)
	a, b := t.TempDir(), t.TempDir()
	group := filepath.Join(claudeAgent.configDir(), "projects", "collision")
	fa, fb := filepath.Join(group, "a.jsonl"), filepath.Join(group, "b.jsonl")
	writeFile(t, fa, claudeTranscript(a), 0o644)
	writeFile(t, fb, claudeTranscript(b), 0o644)
	idx := mustIndexSessions(t)

	n, err := purgeSessions(a, idx)
	if err != nil {
		t.Fatalf("purgeSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if exists(fa) {
		t.Fatalf("purged worktree's file survived: %s", fa)
	}
	if !exists(fb) {
		t.Fatalf("shared group must keep the other worktree's session: %s", fb)
	}
}

// TestBuildWorktreeClaude pins the claude capability to the project or local
// settings file.
func TestBuildWorktreeClaude(t *testing.T) {
	for _, rel := range []string{"", claudeSettingsFile, claudeLocalSettingsFile} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			if rel != "" {
				writeFile(t, filepath.Join(root, rel), "{}", 0o644)
			}
			w := buildWorktree(gworktree{Path: root, Main: true}, sessionIndex{}, nil, nil, nil)
			want := capAbsent
			if rel != "" {
				want = capPresent
			}
			if w.Caps["claude"] != want {
				t.Fatalf("%s: claude = %v, want %v", rel, w.Caps["claude"], want)
			}
		})
	}
	// A harness settings file is a presence signal, not a parsed config: even a
	// corrupt one counts as wired (contrast the MCP configs, which must parse).
	t.Run("corrupt settings still wired", func(t *testing.T) {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, claudeSettingsFile), "{not json", 0o644)
		w := buildWorktree(gworktree{Path: root, Main: true}, sessionIndex{}, nil, nil, nil)
		if w.Caps["claude"] != capPresent {
			t.Fatalf("corrupt settings: claude = %v, want present", w.Caps["claude"])
		}
	})
}
