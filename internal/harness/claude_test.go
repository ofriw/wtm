package harness

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	. "wtm/internal/testutil"
)

// claude_test.go — Tier-1 contracts for the Claude Code harness descriptor
// against the real config-dir layout and real git repos. No mocks.

func TestClaudeConfigDir(t *testing.T) {
	sb := Sandbox(t)
	if got := ClaudeAgent.ConfigDir(); got != sb.ClaudeDir {
		t.Fatalf("ClaudeAgent.ConfigDir() = %q, want %q", got, sb.ClaudeDir)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	if got, want := ClaudeAgent.ConfigDir(), filepath.Join(sb.Home, ".claude"); got != want {
		t.Fatalf("default ClaudeAgent.ConfigDir() = %q, want %q", got, want)
	}
}

// TestCopyHarnessClaude pins the Claude seed rules: CLAUDE.local.md and
// git-ignored .claude files carry over; tracked, unignored and the nested
// .claude/worktrees tree never do; existing files are never overwritten.
func TestCopyHarnessClaude(t *testing.T) {
	src := InitRepo(t, "main")
	dst := InitRepo(t, "main")
	ignore := "CLAUDE.local.md\n.claude/settings.local.json\n.claude/keep.txt\n.claude/worktrees/\n"
	WriteFile(t, filepath.Join(dst, ".gitignore"), ignore, 0o644)
	WriteFile(t, filepath.Join(dst, ".claude", "tracked.txt"), "from-dst", 0o644)
	GitCommit(t, dst, "tracked claude file", ".gitignore", ".claude/tracked.txt")
	WriteFile(t, filepath.Join(dst, ".claude", "keep.txt"), "keep", 0o644)

	WriteFile(t, filepath.Join(src, "CLAUDE.local.md"), "local", 0o644)
	WriteFile(t, filepath.Join(src, ".claude", "settings.local.json"), "{}", 0o644)
	WriteFile(t, filepath.Join(src, ".claude", "keep.txt"), "from-src", 0o644)
	WriteFile(t, filepath.Join(src, ".claude", "tracked.txt"), "from-src", 0o644)
	WriteFile(t, filepath.Join(src, ".claude", "plain.txt"), "from-src", 0o644)
	WriteFile(t, filepath.Join(src, ".claude", "worktrees", "nested", "x.txt"), "wt", 0o644)

	if err := CopyHarness(src, dst); err != nil {
		t.Fatalf("CopyHarness: %v", err)
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
	if fs.Exists(claudeFile("plain.txt")) {
		t.Fatal("src-unignored file must not be copied")
	}
	if fs.Exists(claudeFile("worktrees")) {
		t.Fatal(".claude/worktrees must never be seeded")
	}
}

// TestIndexAndPurgeClaudeSessions pins recursive Claude session indexing and
// cleanup: owned sessions nested under subagents/ are removed, empty directories
// are cleaned, and another core.Worktree's history survives.
func TestIndexAndPurgeClaudeSessions(t *testing.T) {
	Sandbox(t)
	wt := t.TempDir()
	other := t.TempDir()
	p1 := MkClaudeSession(t, wt, "s1", time.Now())
	sub := filepath.Join(ClaudeAgent.ConfigDir(), "projects", "wt-nested", "s2", "subagents", "agent-1.jsonl")
	WriteFile(t, sub, ClaudeTranscript(wt), 0o644)
	keep := MkClaudeSession(t, other, "s3", time.Now())

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

	n, err := PurgeSessions(wt, idx)
	if err != nil {
		t.Fatalf("PurgeSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	if fs.Exists(filepath.Dir(p1)) {
		t.Fatalf("group dir survived: %s", filepath.Dir(p1))
	}
	if fs.Exists(sub) {
		t.Fatalf("nested session survived recursive purge: %s", sub)
	}
	if !fs.Exists(keep) {
		t.Fatalf("other core.Worktree session was purged: %s", keep)
	}
}

// TestPurgeSessionsSharedGroup pins the lossy-encoding safety net: when two
// worktrees' cwds index into ONE group dir, purging one must not recursively
// delete the dir — the other core.Worktree's session must survive.
func TestPurgeSessionsSharedGroup(t *testing.T) {
	Sandbox(t)
	a, b := t.TempDir(), t.TempDir()
	group := filepath.Join(ClaudeAgent.ConfigDir(), "projects", "collision")
	fa, fb := filepath.Join(group, "a.jsonl"), filepath.Join(group, "b.jsonl")
	WriteFile(t, fa, ClaudeTranscript(a), 0o644)
	WriteFile(t, fb, ClaudeTranscript(b), 0o644)
	idx := mustIndexSessions(t)

	n, err := PurgeSessions(a, idx)
	if err != nil {
		t.Fatalf("PurgeSessions: %v", err)
	}
	if n != 1 {
		t.Fatalf("purged %d, want 1", n)
	}
	if fs.Exists(fa) {
		t.Fatalf("purged core.Worktree's file survived: %s", fa)
	}
	if !fs.Exists(fb) {
		t.Fatalf("shared group must keep the other core.Worktree's session: %s", fb)
	}
}

// TestBuildWorktreeClaude pins the claude capability to the project or local
// settings file.
func TestBuildWorktreeClaude(t *testing.T) {
	for _, rel := range []string{"", claudeSettingsFile, claudeLocalSettingsFile} {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			if rel != "" {
				WriteFile(t, filepath.Join(root, rel), "{}", 0o644)
			}
			w := core.Worktree{Path: root, Main: true, Caps: core.CapabilityStates(core.CapInput{Root: root, MCP: MCPConfigs(root, nil)})}
			want := core.CapAbsent
			if rel != "" {
				want = core.CapPresent
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
		WriteFile(t, filepath.Join(root, claudeSettingsFile), "{not json", 0o644)
		w := core.Worktree{Path: root, Main: true, Caps: core.CapabilityStates(core.CapInput{Root: root, MCP: MCPConfigs(root, nil)})}
		if w.Caps["claude"] != core.CapPresent {
			t.Fatalf("corrupt settings: claude = %v, want present", w.Caps["claude"])
		}
	})
}
