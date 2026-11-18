package harness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

func TestClaudeMetadataBeforeOwner(t *testing.T) {
	Sandbox(t)
	owner := t.TempDir()
	stamp := time.Now().Add(-time.Hour).Truncate(time.Second)
	path := MkClaudeSession(t, owner, "metadata-first", stamp)
	idx := mustIndexSessions(t)
	if !idxContains(idx, owner, path) || !core.LastUsed(owner, idx).Equal(stamp) {
		t.Fatalf("metadata-first transcript not attributed to %s at %s", owner, stamp)
	}
}

// Unknown files must survive even when the only indexed owner is removed.
func TestPurgePreservesUnattributedFiles(t *testing.T) {
	for _, name := range []string{"malformed", "oversized", "artifact", "late-owner"} {
		t.Run(name, func(t *testing.T) {
			Sandbox(t)
			owner := t.TempDir()
			owned := MkClaudeSession(t, owner, "collision", time.Now())
			keep := filepath.Join(filepath.Dir(owned), name+".jsonl")
			content := unownedTranscript(t, name)
			WriteFile(t, keep, content, 0o644)
			idx := mustIndexSessions(t)
			if name == "late-owner" {
				WriteFile(t, keep, ClaudeTranscript(t.TempDir()), 0o644)
			}
			assertPurgeKeepsFile(t, owner, idx, owned, keep)
		})
	}
}

func unownedTranscript(t *testing.T, name string) string {
	t.Helper()
	if name == "oversized" {
		return "{\"cwd\":\"" + strings.Repeat("x", 4<<20) + "\"}\n"
	}
	if name == "artifact" {
		return "{\"type\":\"queue-operation\",\"cwd\":null}\n"
	}
	return "not valid JSON\n"
}

func assertPurgeKeepsFile(t *testing.T, owner string, idx core.SessionIndex, owned, keep string) {
	t.Helper()
	before, err := os.ReadFile(keep)
	if err != nil {
		t.Fatal(err)
	}
	assertOwnedSessionPurged(t, owner, idx, owned)
	after, err := os.ReadFile(keep)
	if err != nil || string(after) != string(before) {
		t.Fatalf("unrelated history changed: %s: %v", keep, err)
	}
}

// A transcript can become unreadable after indexing; purge cannot remove it
// merely because it shares the indexed owner's directory.
func TestPurgePreservesUnreadableFile(t *testing.T) {
	Sandbox(t)
	owner := t.TempDir()
	owned := MkClaudeSession(t, owner, "collision", time.Now())
	idx := mustIndexSessions(t)
	keep := filepath.Join(filepath.Dir(owned), "unreadable.jsonl")
	WriteFile(t, keep, ClaudeTranscript(t.TempDir()), 0o644)
	makeUnreadable(t, keep)
	assertOwnedSessionPurged(t, owner, idx, owned)
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("unreadable history removed: %v", err)
	}
}

func assertOwnedSessionPurged(t *testing.T, owner string, idx core.SessionIndex, owned string) {
	t.Helper()
	n, err := PurgeSessions(owner, idx)
	if err != nil || n != 1 || fs.Exists(owned) {
		t.Fatalf("purge: count=%d error=%v owned survives=%v", n, err, fs.Exists(owned))
	}
}

func makeUnreadable(t *testing.T, path string) {
	t.Helper()
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })
	if f, err := os.Open(path); err == nil {
		_ = f.Close()
		t.Skip("host allows reading mode-000 files")
	}
}

// TestUnreadableSessionFailClosed pins the one policy split: reclamation must
// refuse to act on an incomplete index, while read-only discovery discloses the
// unreadable transcript and reports what it knows.
func TestUnreadableSessionFailClosed(t *testing.T) {
	Sandbox(t)
	repo := NativeRepo(t)
	path := MkClaudeSession(t, repo, "unreadable", time.Now())
	makeUnreadable(t, path)

	if _, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: true}); err == nil || !strings.Contains(err.Error(), path) {
		t.Fatalf("reclamation index must fail on unreadable transcript %s: %v", path, err)
	}
	var idx core.SessionIndex
	warned := CaptureStderr(t, func() {
		var err error
		idx, err = IndexSessions(agents(), core.SessionErrPolicy{})
		if err != nil {
			t.Fatalf("discovery index must not fail: %v", err)
		}
	})
	if !strings.Contains(warned, path) || !strings.Contains(warned, ui.WarnPrefix) {
		t.Fatalf("discovery must warn about %s, got %q", path, warned)
	}
	if len(idx[CanonicalPath(repo)]) != 0 {
		t.Fatalf("unreadable transcript indexed as owned: %v", idx[CanonicalPath(repo)])
	}

	_, errOut, rc := RunWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status must not fail on an unreadable transcript: rc=%d stderr=%s", rc, errOut)
	}
	if !strings.Contains(errOut, path) {
		t.Fatalf("status must disclose the unreadable transcript %s: %s", path, errOut)
	}
}

// TestReclaimFailsClosedOnUnreadableSession pins the user-facing half of the
// policy: naming a branch is consent, but reclamation still refuses to run on an
// index that skipped an unreadable transcript, because that transcript may be
// the only evidence the core.Worktree is in use.
func TestReclaimFailsClosedOnUnreadableSession(t *testing.T) {
	Sandbox(t)
	repo := NativeRepo(t)
	linked := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked"))
	GitWorktreeAdd(t, repo, linked, "feat", "refs/heads/main")
	session := MkClaudeSession(t, linked, "unreadable", time.Now())
	makeUnreadable(t, session)

	_, errOut, rc := RunWTM(t, repo, nil, "delete", "feat", "--yes")
	if rc == 0 {
		t.Fatalf("delete must abort on an unreadable transcript: stderr=%s", errOut)
	}
	if !strings.Contains(errOut, session) {
		t.Fatalf("delete must name the unreadable transcript %s: %s", session, errOut)
	}
	if !fs.Exists(linked) {
		t.Fatalf("core.Worktree removed despite an incomplete session index: %s", linked)
	}
}

func TestCorruptNativeMCPStatusConsistent(t *testing.T) {
	Sandbox(t)
	t.Setenv("COLUMNS", "240") // Keep capability columns visible regardless of the host terminal.
	repo := NativeRepo(t)
	WriteFile(t, filepath.Join(repo, PiNativeMCPFile), "{broken\n", 0o644)
	for _, args := range [][]string{{"status"}, {"status", "--wide"}, {"status", "--json"}} {
		out, errOut, rc := RunWTM(t, repo, nil, args...)
		// The warning renders an OS-native path (agents.go filepath.Join); normalize
		// the haystack so the logical forward-slash constant still matches on Windows.
		if rc != 0 || !strings.Contains(filepath.ToSlash(errOut), PiNativeMCPFile) || !strings.Contains(errOut, "treating as absent") {
			t.Fatalf("%v: rc=%d warning=%s", args, rc, errOut)
		}
		assertNativeMCPAbsent(t, args, out)
	}
}

func assertNativeMCPAbsent(t *testing.T, args []string, out string) {
	t.Helper()
	if args[len(args)-1] != "--json" {
		if args[len(args)-1] == "--wide" {
			assertStatusTableCells(t, out, []string{"PI", "MCP"}, "n/a")
		} else {
			assertStatusTableCells(t, out, []string{"INTEGRATIONS"}, "-")
		}
		return
	}
	for _, w := range decodeStatus(t, out) {
		for _, id := range []string{"pi", "mcp"} {
			if got := w.Integrations[id]; got != "n/a" {
				t.Fatalf("corrupt config reports %s=%q, want absent", id, got)
			}
		}
	}
}

// Table gutters separate columns; spaces inside LAST USED remain part of its cell.
var statusTableGutter = regexp.MustCompile(`\s{2,}`)

func statusTableCells(line string) []string {
	return statusTableGutter.Split(strings.TrimSpace(line), -1)
}

func assertStatusTableCells(t *testing.T, out string, names []string, want string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) < 2 {
		t.Fatalf("status table has no rows: %s", out)
	}
	headers := statusTableCells(lines[0])
	for _, name := range names {
		i := slices.Index(headers, name)
		for _, line := range lines[1:] {
			cells := statusTableCells(line)
			if i < 0 || i >= len(cells) || len(cells) != len(headers) || cells[i] != want {
				t.Fatalf("status column %s must be %q: %s", name, want, out)
			}
		}
	}
}

func decodeStatus(t *testing.T, out string) []core.JSONWorktree {
	t.Helper()
	var rows []core.JSONWorktree
	if err := json.Unmarshal([]byte(out), &rows); err != nil || len(rows) == 0 {
		t.Fatalf("invalid status inventory: %v: %s", err, out)
	}
	return rows
}

func TestClaudeTranscriptKeepsOldWorktreeActive(t *testing.T) {
	Sandbox(t)
	SetTTL(t, "30d")
	old := time.Now().Add(-400 * 24 * time.Hour).Truncate(time.Second)
	t.Setenv("GIT_AUTHOR_DATE", old.Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", old.Format(time.RFC3339))
	repo := NativeRepo(t)
	wt := filepath.Join(filepath.Dir(repo), "old-core.Worktree")
	GitWorktreeAdd(t, repo, wt, "old-core.Worktree", "main")
	// Git activity now participates alongside sessions; isolate the transcript.
	BackdateGitActivity(t, wt, old)
	assertTranscriptActivity(t, repo, wt)
}

func assertTranscriptActivity(t *testing.T, repo, wt string) {
	t.Helper()
	assertCLIWorktreeActivity(t, repo, wt, "UNUSED", time.Time{})
	stamp := time.Now().Add(-24 * time.Hour).Truncate(time.Second)
	MkClaudeSession(t, wt, "recent", stamp)
	assertCLIWorktreeActivity(t, repo, wt, "ACTIVE", stamp)
	out, errOut, rc := RunWTM(t, repo, nil, "gc", "--all", "--yes", "--json")
	if rc != 0 || !fs.Exists(wt) {
		t.Fatalf("recent Claude activity must prevent GC: rc=%d stderr=%s output=%s", rc, errOut, out)
	}
	AssertWorktreeListed(t, repo, wt)
}

func assertCLIWorktreeActivity(t *testing.T, repo, path, status string, stamp time.Time) {
	t.Helper()
	out, errOut, rc := RunWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status: rc=%d stderr=%s", rc, errOut)
	}
	for _, w := range decodeStatus(t, out) {
		if CanonicalPath(w.Path) == CanonicalPath(path) {
			if w.Status != status || (!stamp.IsZero() && (w.LastUsed == nil || *w.LastUsed != stamp.UTC().Format(time.RFC3339))) {
				t.Fatalf("activity: got %+v, want %s at %s", w, status, stamp)
			}
			return
		}
	}
	t.Fatalf("core.Worktree absent from status: %s", path)
}

func unlistableSessionFixture(t *testing.T) (owner, owned, blocked string) {
	t.Helper()
	Sandbox(t)
	owner = t.TempDir()
	owned = MkClaudeSession(t, owner, "visible", time.Now())
	blocked = filepath.Join(ClaudeAgent.ConfigDir(), ClaudeAgent.SessionSubdir(), "blocked")
	WriteFile(t, filepath.Join(blocked, "inside.jsonl"), ClaudeTranscript(owner), 0o644)
	if err := os.Chmod(blocked, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	if _, err := os.ReadDir(blocked); err == nil {
		t.Skip("host allows listing mode-000 directories")
	}
	return
}

// Hidden child contents cannot prove inactivity, so reclamation must stop.
func TestIndexRejectsUnlistableDirectory(t *testing.T) {
	_, _, blocked := unlistableSessionFixture(t)
	if _, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: true}); err == nil || !strings.Contains(err.Error(), blocked) {
		t.Fatalf("strict index must identify unreadable child %s: %v", blocked, err)
	}
}

func TestIndexWarnsOnUnlistableDirectory(t *testing.T) {
	owner, owned, blocked := unlistableSessionFixture(t)
	var idx core.SessionIndex
	warning := CaptureStderr(t, func() {
		var err error
		idx, err = IndexSessions(agents(), core.SessionErrPolicy{})
		if err != nil {
			t.Fatalf("lenient index: %v", err)
		}
	})
	if !strings.Contains(warning, blocked) || !strings.Contains(warning, ui.WarnPrefix) {
		t.Fatalf("unreadable child warning missing: %q", warning)
	}
	if !idxContains(idx, owner, owned) {
		t.Fatalf("sibling session not indexed: %s", owned)
	}
}

// A dangling symlink named like a session holds nothing to attribute or purge,
// so the walk must skip it instead of failing the index.
func TestIndexSkipsDanglingSessionSymlink(t *testing.T) {
	Sandbox(t)
	owner := t.TempDir()
	owned := MkClaudeSession(t, owner, "real", time.Now())
	ghost := filepath.Join(filepath.Dir(owned), "ghost.jsonl")
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), ghost); err != nil {
		t.Fatal(err)
	}

	idx, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: true})
	if err != nil {
		t.Fatalf("dangling symlink must not abort indexing: %v", err)
	}
	if !idxContains(idx, owner, owned) {
		t.Fatalf("sibling session not indexed: %s", owned)
	}
}

// With no harness state dir resolvable (no env override, no HOME), every
// harness silently contributes nothing: an empty index, no error.
func TestIndexSessionsWithoutHome(t *testing.T) {
	Sandbox(t)
	t.Setenv("PI_CODING_AGENT_DIR", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("HOME", "") // os.UserHomeDir fails; harness state is unknowable
	idx, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: true})
	if err != nil {
		t.Fatalf("indexSessions without HOME: %v", err)
	}
	if len(idx) != 0 {
		t.Fatalf("index without HOME must be empty, got %v", idx)
	}
}

// TestAgentDescriptorsComplete pins the fields the shared code dereferences, so
// a future harness that omits one fails here instead of panicking at runtime.
func TestAgentDescriptorsComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range agents() {
		if a.name == "" || a.statusToken == "" || a.configDirEnv == "" || a.SessionSubdir() == "" || a.sessionOwner == nil {
			t.Errorf("agent %q has an empty required field: %+v", a.name, a)
		}
		if seen[a.name] {
			t.Errorf("duplicate agent name %q", a.name)
		}
		seen[a.name] = true
	}
}

// TestClaudeOwnerRecordBound pins maxSessionOwnerRecords: an owner within the
// bound is indexed, one past it is unknown, and unknown history survives purge.
func TestClaudeOwnerRecordBound(t *testing.T) {
	Sandbox(t)
	owner := t.TempDir()
	inside := MkClaudeSession(t, owner, "inside", time.Now())
	dir := filepath.Dir(inside)
	atBound := filepath.Join(dir, "at-bound.jsonl")
	WriteFile(t, atBound, cwdAfterRecords(owner, maxSessionOwnerRecords-1), 0o644)
	pastBound := filepath.Join(dir, "past-bound.jsonl")
	WriteFile(t, pastBound, cwdAfterRecords(owner, maxSessionOwnerRecords), 0o644)

	idx := mustIndexSessions(t)
	if !idxContains(idx, owner, atBound) {
		t.Fatalf("owner at record %d must be indexed", maxSessionOwnerRecords-1)
	}
	if idxContains(idx, owner, pastBound) {
		t.Fatalf("owner past record %d must be unknown", maxSessionOwnerRecords)
	}
	n, err := PurgeSessions(owner, idx)
	if err != nil || n != 2 {
		t.Fatalf("purge: count=%d error=%v, want 2", n, err)
	}
	if !fs.Exists(pastBound) {
		t.Fatalf("history past the owner bound must survive purge: %s", pastBound)
	}
}

// cwdAfterRecords builds a Claude transcript with `before` cwd-less metadata
// records before the user record that carries the owner cwd.
func cwdAfterRecords(owner string, before int) string {
	var b strings.Builder
	for i := 0; i < before; i++ {
		b.WriteString("{\"type\":\"queue-operation\",\"operation\":\"dequeue\",\"cwd\":null}\n")
	}
	fmt.Fprintf(&b, "{\"type\":\"user\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"seed\"}}\n", owner)
	return b.String()
}

// Destructive indexing refuses an unreadable root; display reports the gap
// without blocking the inventory.
func TestIndexUnreadableSessionRootIsFatal(t *testing.T) {
	Sandbox(t)
	owner := t.TempDir()
	MkClaudeSession(t, owner, "visible", time.Now())
	root := filepath.Join(ClaudeAgent.ConfigDir(), ClaudeAgent.SessionSubdir())
	if err := os.Chmod(root, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })
	if _, err := os.ReadDir(root); err == nil {
		t.Skip("host allows listing mode-000 session root")
	}
	if _, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: true}); err == nil || !strings.Contains(err.Error(), root) {
		t.Fatalf("strict index on unreadable root = %v, want error naming %s", err, root)
	}
	warning := CaptureStderr(t, func() {
		if _, err := IndexSessions(agents(), core.SessionErrPolicy{}); err != nil {
			t.Fatalf("display index on unreadable Root: %v", err)
		}
	})
	if !strings.Contains(warning, root) {
		t.Fatalf("display warning must name %s, got %q", root, warning)
	}
}
