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
	if got := piAgentDir(); got != sb.AgentDir {
		t.Fatalf("piAgentDir = %q, want %q", got, sb.AgentDir)
	}
	t.Setenv("PI_CODING_AGENT_DIR", "")
	if got, want := piAgentDir(), filepath.Join(sb.Home, ".pi", "agent"); got != want {
		t.Fatalf("default piAgentDir = %q, want %q", got, want)
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
			if got := sessionCWD(p); got != tc.want {
				t.Fatalf("sessionCWD = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("oversize header returns empty", func(t *testing.T) {
		p := filepath.Join(dir, "big.jsonl")
		writeFile(t, p, "{\"cwd\":\""+strings.Repeat("a", maxSessionHeader)+"\"}\n", 0o644)
		if got := sessionCWD(p); got != "" {
			t.Fatalf("oversize must return empty, got %d chars", len(got))
		}
	})

	t.Run("nonexistent path returns empty", func(t *testing.T) {
		if got := sessionCWD(filepath.Join(dir, "nope")); got != "" {
			t.Fatalf("nonexistent must return empty, got %q", got)
		}
	})
}

func piContains(list []string, want string) bool {
	for _, p := range list {
		if p == want {
			return true
		}
	}
	return false
}

// indexSessions builds the Pi session index once for a test, failing on error.
func indexSessions(t *testing.T) sessionIndex {
	t.Helper()
	idx, err := indexPiSessions()
	if err != nil {
		t.Fatalf("indexPiSessions: %v", err)
	}
	return idx
}

// idxContains reports whether idx maps canonical(wt) to a slice holding p.
func idxContains(idx sessionIndex, wt, p string) bool {
	return piContains(idx[canonical(wt)], p)
}

func TestIndexPiSessions(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()

	t.Run("canonical cwd match", func(t *testing.T) {
		p := mkSession(t, wt, "s1", time.Now())
		idx := indexSessions(t)
		if !idxContains(idx, wt, p) {
			t.Fatalf("index[%s] = %v, want %s", canonical(wt), idx[canonical(wt)], p)
		}
	})

	t.Run("symlinked worktree path still matches", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "wtlink")
		mustSymlink(t, wt, link)
		p := mkSession(t, link, "s2", time.Now())
		idx := indexSessions(t)
		if !idxContains(idx, wt, p) {
			t.Fatalf("index(real) = %v, want %s", idx[canonical(wt)], p)
		}
		if !idxContains(idx, link, p) {
			t.Fatalf("index(link) = %v, want %s", idx[canonical(link)], p)
		}
	})

	t.Run("ignores non-directory and non-jsonl entries", func(t *testing.T) {
		base := filepath.Join(piAgentDir(), "sessions")
		header := "{\"cwd\":" + fmt.Sprintf("%q", wt) + "}\n"
		writeFile(t, filepath.Join(base, "loose.jsonl"), header, 0o644)
		writeFile(t, filepath.Join(base, "sdir", "notes.txt"), header, 0o644)
		idx := indexSessions(t)
		for _, p := range idx[canonical(wt)] {
			if strings.HasSuffix(p, "loose.jsonl") || strings.HasSuffix(p, "notes.txt") {
				t.Fatalf("non-session entry returned: %s", p)
			}
		}
	})
}

func TestLastUsed(t *testing.T) {
	sandbox(t)
	wt := t.TempDir()
	idx := indexSessions(t)
	if got := lastUsed(wt, idx); !got.IsZero() {
		t.Fatalf("lastUsed with no sessions = %v, want zero", got)
	}
	newest := time.Unix(2_000_000, 0)
	mkSession(t, wt, "old", time.Unix(1_000_000, 0))
	mkSession(t, wt, "new", newest)
	idx = indexSessions(t)
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

	n, err := purgePiSessions(wt, indexSessions(t))
	if err != nil {
		t.Fatalf("purgePiSessions: %v", err)
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

func TestCopyPiHarnessMCP(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()

	if err := copyPiHarness(src, dst); err != nil {
		t.Fatalf("absent src no-op: %v", err)
	}
	if exists(filepath.Join(dst, piMCPFile)) {
		t.Fatal("absent source must not create .mcp.json")
	}

	writeFile(t, filepath.Join(src, piMCPFile), "SRC", 0o644)
	if err := copyPiHarness(src, dst); err != nil {
		t.Fatalf("copy .mcp.json: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, piMCPFile)); string(b) != "SRC" {
		t.Fatalf(".mcp.json = %q, want SRC", b)
	}

	writeFile(t, filepath.Join(dst, piMCPFile), "DST", 0o644)
	if err := copyPiHarness(src, dst); err != nil {
		t.Fatalf("re-copy: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, piMCPFile)); string(b) != "DST" {
		t.Fatalf("existing .mcp.json overwritten: %q", b)
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

	if err := copyPiHarness(src, dst); err != nil {
		t.Fatalf("copyPiHarness: %v", err)
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
