package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// status_test.go — Tier-1 contracts for status.go. Discovery runs against a
// real repo with linked worktrees; time-based helpers pin RELATIVE mtimes so
// assertions never race the wall clock.

func setMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func TestWorktreeUnused(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	cases := []struct {
		name     string
		lastUsed time.Time
		want     bool
	}{
		{"zero LastUsed is unused", time.Time{}, true},
		{"younger than ttl is in use", now.Add(-time.Hour), false},
		{"exactly ttl is still in use (cutoff is exclusive)", now.Add(-ttl), false},
		{"older than ttl is unused", now.Add(-ttl - time.Second), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := worktree{LastUsed: tc.lastUsed}
			if got := w.unused(now, ttl); got != tc.want {
				t.Fatalf("unused(now, %v) = %v, want %v", ttl, got, tc.want)
			}
		})
	}
}

func TestWorktreeStatus(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	cases := []struct {
		name     string
		lastUsed time.Time
		want     string
	}{
		{"zero is UNUSED", time.Time{}, "UNUSED"},
		{"recent is ACTIVE", now.Add(-time.Hour), "ACTIVE"},
		{"exactly ttl is ACTIVE", now.Add(-ttl), "ACTIVE"},
		{"older than ttl is UNUSED", now.Add(-ttl - time.Second), "UNUSED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := worktree{LastUsed: tc.lastUsed}
			if got := w.status(now, ttl); got != tc.want {
				t.Fatalf("status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBranchName(t *testing.T) {
	if got := (worktree{}).branchName(); got != "(detached)" {
		t.Fatalf("empty branch = %q, want (detached)", got)
	}
	if got := (worktree{Branch: "main"}).branchName(); got != "main" {
		t.Fatalf("named branch = %q, want main", got)
	}
}

func TestSortWorktrees(t *testing.T) {
	base := time.Now()
	wts := []worktree{
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

// wtlSessionWins pins the Pi session as the newest activity source.
func wtlSessionWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	mkSession(t, repo, "s", want)
	setMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	writeFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	setMtime(t, filepath.Join(repo, "dirty.txt"), now.Add(-4*time.Hour))
	return want
}

// wtlGitDirWins pins the .git mtime as the newest activity source.
func wtlGitDirWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	setMtime(t, filepath.Join(repo, ".git"), want)
	mkSession(t, repo, "s", now.Add(-3*time.Hour))
	writeFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	setMtime(t, filepath.Join(repo, "dirty.txt"), now.Add(-4*time.Hour))
	return want
}

// wtlCommitWins pins the latest commit time as the newest activity source.
func wtlCommitWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour).Truncate(time.Second)
	t.Setenv("GIT_AUTHOR_DATE", want.Format(time.RFC3339))
	t.Setenv("GIT_COMMITTER_DATE", want.Format(time.RFC3339))
	writeFile(t, filepath.Join(repo, "later.txt"), "x", 0o644)
	gitCommit(t, repo, "later", "later.txt")
	setMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	mkSession(t, repo, "s", now.Add(-5*time.Hour))
	return want
}

// wtlDirtyWins pins a modified untracked file as the newest activity source.
func wtlDirtyWins(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-time.Hour)
	writeFile(t, filepath.Join(repo, "dirty.txt"), "x", 0o644)
	setMtime(t, filepath.Join(repo, "dirty.txt"), want)
	setMtime(t, filepath.Join(repo, ".git"), now.Add(-3*time.Hour))
	mkSession(t, repo, "s", now.Add(-4*time.Hour))
	return want
}

// wtlChunkHoundIgnored proves .chunkhound/* never counts as activity.
func wtlChunkHoundIgnored(t *testing.T, repo string, now time.Time) time.Time {
	t.Helper()
	want := now.Add(-2 * time.Hour) // .git mtime; the only commit is the 2020 seed
	setMtime(t, filepath.Join(repo, ".git"), want)
	ch := filepath.Join(repo, chunkhoundDir, "db", "chunks.db")
	writeFile(t, ch, "db", 0o644)
	setMtime(t, ch, now.Add(-time.Hour))
	return want
}

func TestWorktreeLastUsed(t *testing.T) {
	sandbox(t)
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
			repo := initRepo(t, "main")
			gitTestCommit(t, repo)
			want := tc.setup(t, repo, now)
			backdateActivityIndex(t, repo, now.Add(-6*time.Hour))
			idx := indexSessions(t)
			if got := worktreeLastUsed(repo, idx); !got.Equal(want) {
				t.Fatalf("worktreeLastUsed = %v, want %v", got, want)
			}
		})
	}
	t.Run("future timestamp is clamped to now", func(t *testing.T) {
		repo := initRepo(t, "main")
		gitTestCommit(t, repo)
		mkSession(t, repo, "future", now.Add(48*time.Hour))
		idx := indexSessions(t)
		got := worktreeLastUsed(repo, idx)
		if got.After(time.Now()) {
			t.Fatalf("future timestamp not clamped: %v", got)
		}
		if got.Before(now.Add(-time.Minute)) {
			t.Fatalf("clamp lost recency: %v", got)
		}
	})
}

func TestDiscover(t *testing.T) {
	sandbox(t)
	now := time.Now()
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linkedA := canonical(filepath.Join(filepath.Dir(repo), "linked-a"))
	linkedB := canonical(filepath.Join(filepath.Dir(repo), "linked-b"))
	gitWorktreeAdd(t, repo, linkedA, "feat-a", "refs/heads/main")
	gitWorktreeAdd(t, repo, linkedB, "feat-b", "refs/heads/main")

	// Inventory: main has config+db, linked-a has mcp, linked-b has none.
	writeFile(t, filepath.Join(repo, chunkhoundConfigFile), "{}", 0o644)
	writeFile(t, chunkhoundDBPath(repo), "db", 0o644)
	writeFile(t, filepath.Join(linkedA, piMCPFile), "{}", 0o644)
	mkSession(t, repo, "main-s", now.Add(-3*time.Hour))
	mkSession(t, linkedA, "a-s", now.Add(-time.Hour))
	mkSession(t, linkedB, "b-s", now.Add(-2*time.Hour))

	// Pin non-session activity old so the sessions above decide ordering.
	for _, p := range []string{repo, linkedA, linkedB} {
		backdateGitActivity(t, p, now.Add(-10*time.Hour))
	}
	setMtime(t, filepath.Join(repo, chunkhoundConfigFile), now.Add(-10*time.Hour))
	setMtime(t, chunkhoundDBPath(repo), now.Add(-10*time.Hour))
	setMtime(t, filepath.Join(linkedA, piMCPFile), now.Add(-10*time.Hour))

	wts, err := discover(repo, nil)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	wantOrder := []string{canonical(repo), linkedA, linkedB}
	if len(wts) != len(wantOrder) {
		t.Fatalf("discover returned %d worktrees, want %d", len(wts), len(wantOrder))
	}
	for i, p := range wantOrder {
		if wts[i].Path != p {
			t.Fatalf("order[%d] = %q, want %q", i, wts[i].Path, p)
		}
	}
	if !wts[0].Main || wts[1].Main || wts[2].Main {
		t.Fatalf("Main flag = %v/%v/%v, want true/false/false", wts[0].Main, wts[1].Main, wts[2].Main)
	}
	if !wts[0].Config || !wts[0].DB || wts[0].MCP {
		t.Fatalf("main inventory = config:%v db:%v mcp:%v", wts[0].Config, wts[0].DB, wts[0].MCP)
	}
	if !wts[1].MCP || wts[1].Config || wts[1].DB {
		t.Fatalf("linked-a inventory = config:%v db:%v mcp:%v", wts[1].Config, wts[1].DB, wts[1].MCP)
	}
	if wts[2].Config || wts[2].DB || wts[2].MCP {
		t.Fatalf("linked-b must have empty inventory: %+v", wts[2])
	}
}

// TestDiscoverUpstreamsUnavailableReachesNotice pins that when branchUpstreams
// fails, discovery degrades instead of aborting, and the warning must travel
// through the renderer's persistent notice path. A raw stderr write during the
// renderer is overwritten by the next frame — the warning would vanish.
func TestDiscoverUpstreamsUnavailableReachesNotice(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	// A corrupt packed-refs makes for-each-ref fail while `worktree list` still
	// succeeds, so the branchUpstreams error path is reachable with real git and
	// no stub.
	writeFile(t, filepath.Join(repo, ".git", "packed-refs"), "garbage\n", 0o644)

	events := make(chan progressEvent, 64)
	p := &progress{ctx: context.Background(), events: events}
	if _, err := discover(repo, p); err != nil {
		t.Fatalf("discover must degrade on a bad upstream query, not fail: %v", err)
	}
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if got := countNotices(notices, "upstreams unavailable"); got != 1 {
		t.Fatalf("upstreams-unavailable notices = %d (%v), want exactly 1", got, notices)
	}

	stderr := captureStderr(t, func() {
		if _, err := discover(repo, nil); err != nil {
			t.Fatalf("discover(nil progress): %v", err)
		}
	})
	if !strings.Contains(stderr, "upstreams unavailable") {
		t.Fatalf("nil progress must warn on stderr: %q", stderr)
	}
}

// TestBuildWorktreeNativeMCP proves the MCP signal keys off the shared
// piMCPFiles SSOT, not just the adapter's .mcp.json.
func TestBuildWorktreeNativeMCP(t *testing.T) {
	for _, rel := range append([]string{""}, piMCPFiles()...) {
		t.Run(rel, func(t *testing.T) {
			root := t.TempDir()
			if rel != "" {
				writeFile(t, filepath.Join(root, rel), "{}", 0o644)
			}
			w := buildWorktree(gworktree{Path: root, Main: true}, sessionIndex{}, nil, tempStore{}, nil)
			if w.MCP != (rel != "") || w.MCPNative != (rel == piNativeMCPFile) {
				t.Fatalf("%s: MCP = %v, MCPNative = %v", rel, w.MCP, w.MCPNative)
			}
		})
	}
}

// TestDisplayPath pins the only user-visible path rewrite: $HOME becomes ~.
func TestDisplayPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	sep := string(os.PathSeparator)
	cases := []struct{ in, want string }{
		{home, "~"},
		{home + sep + "repo", "~" + sep + "repo"},
		{home + "x" + sep + "repo", home + "x" + sep + "repo"},
		{sep + "elsewhere", sep + "elsewhere"},
	}
	for _, tc := range cases {
		if got := displayPath(tc.in); got != tc.want {
			t.Errorf("displayPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPrintTable(t *testing.T) {
	t.Setenv("COLUMNS", "0") // non-TTY goldens must stay at natural widths
	headers := []string{"A", "BB"}
	rows := [][]string{{"x", "y"}, {"long", "z"}}
	out := captureStdout(t, func() { printTable(headers, rows, colorAuto, nil, nil) })
	want := "A     BB\nx     y\nlong  z\n"
	if out != want {
		t.Fatalf("printTable = %q, want %q", out, want)
	}
}

// TestStatusHeadersSSOT pins the status header literal and proves the shrink
// policy is derived from the same registry: a rename or a new rank cannot pass
// without updating this contract.
func TestStatusHeadersSSOT(t *testing.T) {
	want := []string{"PATH", "BRANCH", "UPSTREAM", "LAST USED", "STATUS", "TEMP", "CONFIG", "DB", "MCP"}
	if got := columnHeaders(statusColumns); !slices.Equal(got, want) {
		t.Fatalf("statusColumns = %v, want %v", got, want)
	}
	for _, name := range shrinkPriority {
		if !slices.Contains(want, name) {
			t.Errorf("shrink column %q is not a status header; shrink policy leaked from another grid", name)
		}
	}
	// The derived elision order is a contract, not an implementation detail.
	wantShrink := []string{"PATH", "UPSTREAM", "BRANCH", "LAST USED"}
	if !slices.Equal(shrinkPriority, wantShrink) {
		t.Errorf("shrinkPriority = %v, want %v", shrinkPriority, wantShrink)
	}
	for _, c := range allColumns {
		if c.shrinkRank == 0 {
			continue
		}
		if shrinkFloor[c.header] != c.floor {
			t.Errorf("column %q floor = %d, want the registered %d", c.header, shrinkFloor[c.header], c.floor)
		}
	}
	// Every column a grid uses must be registered in allColumns, or it silently
	// escapes the shared shrink policy.
	registered := map[string]bool{}
	for _, c := range allColumns {
		registered[c.header] = true
	}
	for _, cols := range [][]worktreeColumn{statusColumns, gcPickerColumns} {
		for _, c := range cols {
			if !registered[c.header] {
				t.Errorf("column %q is used by a grid but missing from allColumns", c.header)
			}
		}
	}
}

// TestRenderGridMatchesPrintTable pins the extraction: printTable's bytes must
// equal the shared grid engine's header+body rejoined under a no-color profile.
func TestRenderGridMatchesPrintTable(t *testing.T) {
	t.Setenv("COLUMNS", "0")
	headers := []string{"A", "BB", "CCC"}
	rows := [][]string{{"x", "yy", "zzz"}, {"longer", "y", "z"}}
	out := captureStdout(t, func() { printTable(headers, rows, colorNever, nil, []string{"CCC"}) })
	header, body := renderGrid(sizedGrid(headers, rows, widthBudget(), []string{"CCC"}))
	if got := gridString(header, body) + "\n"; got != out {
		t.Fatalf("renderGrid = %q, printTable = %q", got, out)
	}
}

// TestPrintStatusTableFits proves the whole status table honors a pinned budget
// end to end, and that displayPath runs before width budgeting.
func TestPrintStatusTableFits(t *testing.T) {
	t.Setenv("COLUMNS", "80")
	t.Setenv("HOME", "/home/agent")
	wts := []worktree{{
		Path:   "/home/agent/very/long/checkout/that/never/ends/repo",
		Branch: "feature/a-rather-long-branch-name", LastUsed: time.Now(),
		Upstream: upstream{Short: "origin/feature/a-rather-long-branch-name", Remote: "origin", Ref: "refs/heads/x"},
		Config:   true, DB: true, MCP: true,
	}}
	out := captureStdout(t, func() { printStatusTable(wts, time.Now(), 10*time.Hour, colorAuto) })
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if w := dispWidth(line); w > 80 {
			t.Errorf("line exceeds 80 cells: %d\n%q", w, line)
		}
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("long path must be middle-elided:\n%s", out)
	}
	if strings.Contains(out, "/home/agent/") {
		t.Fatalf("displayPath must rewrite $HOME before budgeting:\n%s", out)
	}
}

func TestPrintStatusJSON(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	last := time.Date(2021, 2, 3, 4, 5, 6, 0, time.UTC)
	wts := []worktree{
		{Path: "/detached", LastUsed: last, Config: true, MCP: true, MCPNative: true},
		{Path: "/main", Branch: "main", Main: true, DB: true, Upstream: upstream{Short: "origin/main", Remote: "origin", Ref: "refs/heads/main"}},
	}
	out := captureStdout(t, func() {
		if err := printStatusJSON(wts, now, ttl); err != nil {
			t.Errorf("printStatusJSON: %v", err)
		}
	})
	if !strings.Contains(out, `"lastUsed": null`) {
		t.Fatalf("zero LastUsed must serialize as null: %s", out)
	}
	var got []jsonWorktree
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, out)
	}
	if len(got) != 2 {
		t.Fatalf("got %d worktrees, want 2", len(got))
	}
	if got[0].Branch != "(detached)" {
		t.Fatalf("detached branch = %q, want (detached)", got[0].Branch)
	}
	if got[0].LastUsed == nil || *got[0].LastUsed != "2021-02-03T04:05:06Z" {
		t.Fatalf("RFC3339 UTC lastUsed = %v, want 2021-02-03T04:05:06Z", got[0].LastUsed)
	}
	if got[0].Status != "UNUSED" {
		t.Fatalf("stale status = %q, want UNUSED", got[0].Status)
	}
	if got[1].LastUsed != nil {
		t.Fatalf("zero LastUsed = %v, want nil", *got[1].LastUsed)
	}
	if !got[1].Main || !got[1].DB || !got[0].Config || !got[0].MCP || !got[0].MCPNative || got[1].MCPNative {
		t.Fatalf("flags lost in JSON: %+v", got)
	}
	if got[1].Upstream != "origin/main" || got[0].Upstream != "" {
		t.Fatalf("upstream lost in JSON: %+v", got)
	}
	// Field names are the script contract: decode to maps so a rename fails
	// fast even when the struct tags follow.
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("re-decode JSON: %v", err)
	}
	for _, key := range []string{"upstream", "mcpNative"} {
		for i, m := range raw {
			if _, ok := m[key]; !ok {
				t.Fatalf("row %d missing key %q: %v", i, key, m)
			}
		}
	}
}

// TestDispatchValidatesColor pins Fix C: --color is validated once in
// dispatch, so every subcommand rejects a bogus value as a usage error (exit 2)
// before touching the repo.
func TestDispatchValidatesColor(t *testing.T) {
	for _, cmd := range []string{"status", "add", "delete", "gc", "config"} {
		g := &globals{color: "bogus"}
		if err := dispatch(cmd, g, nil); !isUsage(err) {
			t.Errorf("dispatch(%q, color:bogus) = %v, want usageError", cmd, err)
		}
	}
}

// TestParseSubRejectsInvalidGlobalsAfterSubcommand pins that a bogus global
// placed AFTER the subcommand is validated: subFlags re-binds globals, so the
// value is only visible once parseSub has parsed it.
func TestParseSubRejectsInvalidGlobalsAfterSubcommand(t *testing.T) {
	for _, args := range [][]string{
		{"--color", "bogus"},
		{"--progress", "sometimes"},
	} {
		if err := cmdStatus(&globals{root: t.TempDir()}, args); !isUsage(err) {
			t.Errorf("cmdStatus(%v) = %v, want usageError", args, err)
		}
	}
}

// TestCellTemp pins the shared TEMP cell: a dash for permanent worktrees, the
// compact remaining window while active, "expired" once the window has passed.
func TestCellTemp(t *testing.T) {
	now := tempTestTime
	cases := []struct {
		name string
		w    worktree
		want string
	}{
		{"permanent is dash", worktree{Branch: "main"}, "-"},
		{"active temp shows remaining window", worktree{Temp: true, TempCreated: now.Add(-2 * time.Hour), TempTTL: 3 * time.Hour}, "1h"},
		{"lapsed temp reads expired", worktree{Temp: true, TempCreated: now.Add(-3 * time.Hour), TempTTL: time.Hour}, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cellTemp(tc.w, columnContext{now: now}); got != tc.want {
				t.Fatalf("cellTemp = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestTempDeadlineStatusAndSelectionAgree(t *testing.T) {
	w := worktree{Temp: true, TempCreated: tempTestTime, LastUsed: tempTestTime.Add(time.Second), TempTTL: 1500 * time.Millisecond}
	deadline := tempTestTime.Add(2500 * time.Millisecond)
	if got := w.tempExpiresAt(); !got.Equal(deadline) {
		t.Fatalf("deadline = %s, want %s", got, deadline)
	}
	if w.unused(deadline, time.Hour) || !w.unused(deadline.Add(time.Nanosecond), time.Hour) {
		t.Fatal("status and selection disagree at the fractional deadline")
	}
}

// TestBuildWorktreeInvalidTempTTL pins the defensive skip: a record with an
// unparsable TTL must not mark the worktree temp, even if the store bypassed
// decode validation.
func TestBuildWorktreeInvalidTempTTL(t *testing.T) {
	sandbox(t)
	dir := t.TempDir()
	pth := canonical(dir)
	temps := tempStore{pth: tempRecord{CreatedAt: tempTestTime, TTL: "bogus", Branch: "tmp/x"}}
	w := buildWorktree(gworktree{Path: dir}, sessionIndex{}, nil, temps, nil)
	if w.Temp {
		t.Fatalf("invalid TTL record marked temp: %+v", w)
	}
}
