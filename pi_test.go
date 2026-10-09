package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pi_test.go — Tier-1 contracts for pi.go against the real agent-dir layout
// and real git repos. No mocks.

func TestPiAgentDir(t *testing.T) {
	sb := sandbox(t)
	if got := piAgent.configDir(); got != sb.AgentDir {
		t.Fatalf("piAgent.configDir() = %q, want %q", got, sb.AgentDir)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got, want := piAgent.configDir(), filepath.Join(sb.Home, ".pi", "agent"); got != want {
		t.Fatalf("default piAgent.configDir() = %q, want %q", got, want)
	}
}

func TestSessionCWD(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{"valid", "{\"cwd\":\"/x\"}\n", "/x"},
		{"missing cwd key", "{\"id\":\"a\"}\n", ""},
		{"malformed json", "not json\n", ""},
		{"empty file", "", ""},
		{"no trailing newline", "{\"cwd\":\"/y\"}", "/y"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i))
			writeFile(t, p, tc.content, 0o644)
			if got, err := piAgent.readSessionOwner(p); err != nil || got != tc.want {
				t.Fatalf("session owner = %q, error %v, want %q", got, err, tc.want)
			}
		})
	}

	t.Run("oversize header returns empty", func(t *testing.T) {
		p := filepath.Join(dir, "big.jsonl")
		writeFile(t, p, "{\"cwd\":\""+strings.Repeat("a", maxSessionHeader)+"\"}\n", 0o644)
		if got, err := piAgent.readSessionOwner(p); err != nil || got != "" {
			t.Fatalf("oversize must return empty, got %d chars, error %v", len(got), err)
		}
	})

	t.Run("nonexistent path reports error", func(t *testing.T) {
		if got, err := piAgent.readSessionOwner(filepath.Join(dir, "nope")); !os.IsNotExist(err) || got != "" {
			t.Fatalf("nonexistent must report missing path, got %q, error %v", got, err)
		}
	})
}

// mustIndexSessions builds the session index once for a test, failing on error.
func mustIndexSessions(t *testing.T) sessionIndex {
	t.Helper()
	idx, err := indexSessions(agents(), sessionErrPolicy{failClosed: true})
	if err != nil {
		t.Fatalf("indexSessions: %v", err)
	}
	return idx
}

// idxContains reports whether idx maps canonical(wt) to a ref holding p.
func idxContains(idx sessionIndex, wt, p string) bool {
	for _, r := range idx[canonical(wt)] {
		if r.path == p {
			return true
		}
	}
	return false
}

func TestIndexPiSessions(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()

	t.Run("canonical cwd match", func(t *testing.T) {
		p := mkSession(t, wt, "s1", time.Now())
		idx := mustIndexSessions(t)
		if !idxContains(idx, wt, p) {
			t.Fatalf("index[%s] = %v, want %s", canonical(wt), idx[canonical(wt)], p)
		}
	})

	t.Run("symlinked worktree path still matches", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "wtlink")
		mustSymlink(t, wt, link)
		p := mkSession(t, link, "s2", time.Now())
		idx := mustIndexSessions(t)
		if !idxContains(idx, wt, p) {
			t.Fatalf("index(real) = %v, want %s", idx[canonical(wt)], p)
		}
		if !idxContains(idx, link, p) {
			t.Fatalf("index(link) = %v, want %s", idx[canonical(link)], p)
		}
	})

	t.Run("recursive walk ignores non-jsonl entries", func(t *testing.T) {
		base := filepath.Join(piAgent.configDir(), "sessions")
		header := "{\"cwd\":" + fmt.Sprintf("%q", wt) + "}\n"
		writeFile(t, filepath.Join(base, "loose.jsonl"), header, 0o644)
		writeFile(t, filepath.Join(base, "sdir", "notes.txt"), header, 0o644)
		idx := mustIndexSessions(t)
		if !idxContains(idx, wt, filepath.Join(base, "loose.jsonl")) {
			t.Fatalf("recursive walk must index a loose jsonl session")
		}
		for _, r := range idx[canonical(wt)] {
			if strings.HasSuffix(r.path, "notes.txt") {
				t.Fatalf("non-session entry returned: %s", r.path)
			}
		}
	})
}

// TestIndexSessionsSymlinkedRoot pins that a symlinked session root still
// indexes: WalkDir never descends a symlink, so the root is resolved first —
// otherwise a dotfiles-style symlink would hide live sessions as UNUSED.
func TestIndexSessionsSymlinkedRoot(t *testing.T) {
	sb := sandbox(t)
	wt := t.TempDir()
	sessions := filepath.Join(sb.AgentDir, "sessions")
	if err := os.RemoveAll(sessions); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(t.TempDir(), "sessions")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, real, sessions)

	p := mkSession(t, wt, "s1", time.Now())
	want, err := filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	idx := mustIndexSessions(t)
	if !idxContains(idx, wt, want) {
		t.Fatalf("symlinked root: index[%s] = %v, want %s", canonical(wt), idx[canonical(wt)], want)
	}
}

func TestLastUsed(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()
	idx := mustIndexSessions(t)
	if got := lastUsed(wt, idx); !got.IsZero() {
		t.Fatalf("lastUsed with no sessions = %v, want zero", got)
	}
	newest := time.Unix(2_000_000, 0)
	mkSession(t, wt, "old", time.Unix(1_000_000, 0))
	mkSession(t, wt, "new", newest)
	idx = mustIndexSessions(t)
	if got := lastUsed(wt, idx); !got.Equal(newest) {
		t.Fatalf("lastUsed = %v, want %v", got, newest)
	}
}

func TestPurgePiSessions(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()
	other := t.TempDir()
	p1 := mkSession(t, wt, "d1", time.Now())
	p2 := mkSession(t, wt, "d2", time.Now())
	keep := mkSession(t, other, "d3", time.Now())

	n, err := purgeSessions(wt, mustIndexSessions(t))
	if err != nil {
		t.Fatalf("purgeSessions: %v", err)
	}
	if n != 2 {
		t.Fatalf("purged %d, want 2", n)
	}
	for _, p := range []string{p1, p2} {
		if exists(p) {
			t.Fatalf("session file survived: %s", p)
		}
		if exists(filepath.Dir(p)) {
			t.Fatalf("emptied session dir survived: %s", filepath.Dir(p))
		}
	}
	if !exists(keep) {
		t.Fatalf("other worktree session was purged: %s", keep)
	}
}

// TestCopyPiHarnessMCP pins the explicit MCP seed: every recognized config
// copies whenever the source has one and the checkout does not, ignored or
// not. The ordinary-file control proves the exemption is MCP-only: gated .pi
// files copy solely when ignored, and the native config is owned by the seed
// alone (the .pi walk skips it instead of re-gating it).
func TestCopyPiHarnessMCP(t *testing.T) {
	for _, rel := range mcpConfigFiles() {
		for _, ignored := range []bool{false, true} {
			tag := "unignored"
			if ignored {
				tag = "ignored"
			}
			t.Run(rel+"/"+tag, func(t *testing.T) {
				src := initRepo(t, "main")
				dst := initRepo(t, "main")
				if ignored {
					writeFile(t, filepath.Join(dst, ".gitignore"), ".mcp.json\n.pi/mcp.json\n.pi/note.txt\n", 0o644)
				}
				dstFile := filepath.Join(dst, rel)

				if err := copyHarness(src, dst); err != nil {
					t.Fatalf("absent src no-op: %v", err)
				}
				if exists(dstFile) {
					t.Fatalf("absent source must not create %s", rel)
				}

				writeFile(t, filepath.Join(src, rel), "SRC", 0o644)
				writeFile(t, filepath.Join(src, ".pi", "note.txt"), "NOTE", 0o644)
				if err := copyHarness(src, dst); err != nil {
					t.Fatalf("copyHarness: %v", err)
				}
				if b, _ := os.ReadFile(dstFile); string(b) != "SRC" {
					t.Fatalf("%s %s = %q, want SRC", rel, tag, b)
				}
				if got := exists(filepath.Join(dst, ".pi", "note.txt")); got != ignored {
					t.Fatalf("gated control copied=%v, want %v", got, ignored)
				}

				writeFile(t, dstFile, "DST", 0o644)
				if err := copyHarness(src, dst); err != nil {
					t.Fatalf("re-copy: %v", err)
				}
				if b, _ := os.ReadFile(dstFile); string(b) != "DST" {
					t.Fatalf("existing %s overwritten: %q", rel, b)
				}
			})
		}
	}
}

// TestCorruptMCPConfigTreatedAsAbsent pins that an existing but unparseable
// MCP config warns and counts as absent: status must never claim it healthy.
func TestCorruptMCPConfigTreatedAsAbsent(t *testing.T) {
	for _, rel := range mcpConfigFiles() {
		root := t.TempDir()
		writeFile(t, filepath.Join(root, rel), "{not json", 0o644)
		if configs := mcpConfigs(root, nil); configs[rel] {
			t.Fatalf("corrupt %s must count as absent, got %v", rel, configs)
		}
	}
}

func TestCopyPiHarnessPI(t *testing.T) {
	src := initRepo(t, "main")
	dst := initRepo(t, "main")
	writeFile(t, filepath.Join(dst, ".gitignore"), ".pi/ignored.txt\n.pi/keep.txt\n", 0o644)
	writeFile(t, filepath.Join(dst, ".pi", "tracked.txt"), "from-dst", 0o644)
	gitCommit(t, dst, "tracked pi file", ".gitignore", ".pi/tracked.txt")
	writeFile(t, filepath.Join(dst, ".pi", "keep.txt"), "keep", 0o644)

	writeFile(t, filepath.Join(src, ".pi", "ignored.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".pi", "keep.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".pi", "tracked.txt"), "from-src", 0o644)
	writeFile(t, filepath.Join(src, ".pi", "plain.txt"), "from-src", 0o644)

	if err := copyHarness(src, dst); err != nil {
		t.Fatalf("copyHarness: %v", err)
	}
	piFile := func(rel string) string { return filepath.Join(dst, ".pi", rel) }
	if b, _ := os.ReadFile(piFile("ignored.txt")); string(b) != "from-src" {
		t.Fatalf("dst-ignored file = %q, want from-src", b)
	}
	if b, _ := os.ReadFile(piFile("keep.txt")); string(b) != "keep" {
		t.Fatalf("existing dst file overwritten: %q", b)
	}
	if b, _ := os.ReadFile(piFile("tracked.txt")); string(b) != "from-dst" {
		t.Fatalf("dst-tracked file overwritten: %q", b)
	}
	if exists(piFile("plain.txt")) {
		t.Fatal("src-unignored file must not be copied")
	}
}

// TestCorruptMCPConfigWarnsThroughRenderer pins that the corrupt-config warning
// travels as a persistent renderer notice, not a raw stderr write that the next
// frame would erase.
func TestCorruptMCPConfigWarnsThroughRenderer(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, projectMCPFile), "{not json", 0o644)
	events := make(chan progressEvent, 8)
	p := &progress{ctx: t.Context(), events: events}
	mcpConfigs(root, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], warnPrefix) {
		t.Fatalf("notices = %v, want one %q-prefixed notice", notices, warnPrefix)
	}
}
