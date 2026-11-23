package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

// testNoMCP is the probe-hook stand-in: discovery always supplies a real hook,
// but composition tests exercise buildWorktree directly and need a non-nil
// callback that reports no MCP inventory.
var testNoMCP = func(string, *ui.Progress) map[string]bool { return nil }

// TestLoadWorkspaceRequiresHooks pins the fail-fast seam: every discovery entry
// path rejects nil hooks with a diagnostic instead of panicking deeper in the
// flow. The guard runs before any IO, so no repo is needed.
func TestLoadWorkspaceRequiresHooks(t *testing.T) {
	g := &Globals{Root: t.TempDir()}
	cases := map[string]Hooks{
		"both nil":     {},
		"nil sessions": {MCP: testNoMCP},
		"nil mcp": {
			Sessions: func(*ui.Progress) (SessionIndex, []temp.UnverifiedSession, error) {
				return nil, nil, nil
			},
		},
	}
	for name, hooks := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := LoadWorkspace(g, false, hooks)
			if err == nil || !strings.Contains(err.Error(), "discovery hooks are required") {
				t.Fatalf("err = %v, want 'discovery hooks are required'", err)
			}
		})
	}
}

// TestSortWorktrees pins the total order: main first, then most recently used,
// then path — so machine output never depends on git's listing order.
func TestSortWorktrees(t *testing.T) {
	base := time.Now()
	wts := []Worktree{
		{Path: "/e", LastUsed: base},
		{Path: "/d", LastUsed: base},
		{Path: "/active-new", LastUsed: base.Add(time.Hour)},
		{Path: "/main", Main: true, LastUsed: base.Add(-time.Hour)},
		{Path: "/active-old", LastUsed: base.Add(-time.Hour)},
	}
	sortWorktrees(wts)
	want := []string{"/main", "/active-new", "/d", "/e", "/active-old"}
	for i, p := range want {
		if wts[i].Path != p {
			t.Fatalf("order[%d] = %q, want %q (full: %v)", i, wts[i].Path, p, wts)
		}
	}
}

// seededTempWorktree creates a real temporary checkout with a verified temp
// record, mirroring the cli temp-identity fixtures: a live git path gets a
// real identity via temp.AddTempRecord.
func seededTempWorktree(t *testing.T) (repo, path string) {
	t.Helper()
	Sandbox(t)
	repo = InitRepo(t, "main")
	MustGit(t, repo, "commit", "--allow-empty", "-qm", "initial")
	path = filepath.Join(filepath.Dir(repo), "temp")
	GitWorktreeAdd(t, repo, path, "tmp/test", "HEAD")
	rec := temp.Record{CreatedAt: TempTestTime, TTL: "1h", Branch: "tmp/test"}
	if _, err := temp.AddTempRecord(CanonicalPath(path), rec); err != nil {
		t.Fatal(err)
	}
	return repo, path
}

// TestApplyTempRecordInvalidTTLSkips pins the defensive skip: a record with an
// unparsable TTL must not mark the worktree temp, even if the store bypassed
// decode validation.
func TestApplyTempRecordInvalidTTLSkips(t *testing.T) {
	_, path := seededTempWorktree(t)
	temps, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	bad := temps[CanonicalPath(path)]
	bad.TTL = "bogus"
	temps[CanonicalPath(path)] = bad
	w := Worktree{Path: CanonicalPath(path), Branch: "tmp/test"}
	warnings := CaptureStderr(t, func() { applyTempRecord(&w, temps, nil) })
	if !strings.Contains(warnings, "treating as permanent") {
		t.Fatalf("invalid TTL must warn: %s", warnings)
	}
	if w.Temp {
		t.Fatalf("invalid TTL record marked temp: %+v", w)
	}
}

// TestApplyTempRecordInvalidIdentitySkips pins that an identity that fails
// verification (empty, malformed, oversized) never marks the worktree temp,
// and that exercising composition never mutates the live registry.
func TestApplyTempRecordInvalidIdentitySkips(t *testing.T) {
	_, path := seededTempWorktree(t)
	temps, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	for _, identity := range []string{"", "bad", strings.Repeat("a", 63)} {
		r := temps[CanonicalPath(path)]
		r.Identity = identity
		store := temp.Store{CanonicalPath(path): r}
		got := buildWorktree(git.GWorktree{Path: path, Branch: "tmp/test"}, SessionIndex{}, nil, store, nil, testNoMCP)
		if got.Temp {
			t.Fatalf("invalid identity %q marked temp", identity)
		}
	}
	after, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	if after[CanonicalPath(path)].Identity != temps[CanonicalPath(path)].Identity {
		t.Fatal("composition loop mutated the live registry")
	}
}

// wtlSessionWins pins the Pi session as the newest activity source.
func wtlSessionWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	MkSession(t, repo, "s", want)
	SetMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	WriteFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	SetMtime(t, filepath.Join(repo, "dirty.txt"), now.Add(-4*time.Hour))
	return want
}

// wtlGitDirWins pins the .git mtime as the newest activity source.
func wtlGitDirWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	SetMtime(t, filepath.Join(repo, ".git"), want)
	MkSession(t, repo, "s", now.Add(-3*time.Hour))
	WriteFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	SetMtime(t, filepath.Join(repo, "dirty.txt"), now.Add(-4*time.Hour))
	return want
}

// wtlCommitWins pins the latest commit time as the newest activity source.
func wtlCommitWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour).Truncate(time.Second)
	t.Setenv("GIT_AUTHOR_DATE", want.Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", want.Format(time.RFC3339))
	WriteFile(t, filepath.Join(repo, "later.txt"), "x", 0o644)
	GitCommit(t, repo, "later", "later.txt")
	SetMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	MkSession(t, repo, "s", now.Add(-5*time.Hour))
	return want
}

// wtlDirtyWins pins a modified untracked file as the newest activity source.
func wtlDirtyWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	WriteFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	SetMtime(t, filepath.Join(repo, "dirty.txt"), want)
	SetMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	MkSession(t, repo, "s", now.Add(-4*time.Hour))
	return want
}

// wtlChunkHoundIgnored proves .chunkhound/* never counts as activity.
func wtlChunkHoundIgnored(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-2 * time.Hour) // .git mtime; the only commit is the 2020 seed
	SetMtime(t, filepath.Join(repo, ".git"), want)
	ch := filepath.Join(repo, ".chunkhound", "chunks.db")
	WriteFile(t, ch, "db", 0o644)
	SetMtime(t, ch, now.Add(-time.Hour))
	return want
}

// testSessionIndex builds a SessionIndex from the sandboxed Pi session dir.
// The fixtures create at most one named session per worktree; lastUsed reads
// only SessionRef.Path, so the owner callback is unnecessary here.
func testSessionIndex(t *testing.T, repo string, names ...string) SessionIndex {
	t.Helper()
	cfg := AgentConfigDir("PI_CODING_AGENT_DIR", ".pi", "agent")
	var refs []SessionRef
	for _, name := range names {
		p := filepath.Join(cfg, "sessions", name, name+".jsonl")
		if _, err := os.Stat(p); err == nil {
			refs = append(refs, SessionRef{Path: p})
		}
	}
	return SessionIndex{fs.Canonical(repo): refs}
}

// TestWorktreeLastUsed pins lastUsed composition (sessions + git activity)
// independent of discovery, for the same contract cli's status exercise via
// the shared LoadWorkspace path.
func TestWorktreeLastUsed(t *testing.T) {
	Sandbox(t)
	now := time.Now()
	cases := []struct {
		name  string
		setup func(t *testing.T, repo string, now time.Time) time.Time
	}{
		{"session mtime is newest", wtlSessionWins},
		{"git dir mtime is newest", wtlGitDirWins},
		{"commit time is newest", wtlCommitWins},
		{"modified untracked file is newest", wtlDirtyWins},
		{"chunkhound db is not activity", wtlChunkHoundIgnored},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := InitRepo(t, "main")
			GitTestCommit(t, repo)
			want := tc.setup(t, repo, now)
			BackdateActivityIndex(t, repo, now.Add(-6*time.Hour))
			idx := testSessionIndex(t, repo, "s")
			if got := worktreeLastUsed(repo, idx); !got.Equal(want) {
				t.Fatalf("worktreeLastUsed = %v, want %v", got, want)
			}
		})
	}
	t.Run("future timestamp is clamped to now", func(t *testing.T) {
		repo := InitRepo(t, "main")
		GitTestCommit(t, repo)
		MkSession(t, repo, "future", now.Add(48*time.Hour))
		idx := testSessionIndex(t, repo, "future")
		got := worktreeLastUsed(repo, idx)
		if got.After(time.Now()) {
			t.Fatalf("future timestamp not clamped: %v", got)
		}
		if got.Before(now.Add(-time.Minute)) {
			t.Fatalf("clamp lost recency: %v", got)
		}
	})
}
