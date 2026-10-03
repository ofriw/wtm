package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

// gc_test.go — Tier-1 contracts for gc.go selection and removal against a real
// repo. gcSelection calls time.Now() internally, so fixtures use relative
// mtimes; exact boundaries live in status_test.go's pure worktree.unused test.

func TestGCAllPathMutuallyExclusive(t *testing.T) {
	err := cmdGC(&globals{}, []string{"--all", "--path", "/somewhere"})
	var ue usageError
	if !errors.As(err, &ue) {
		t.Fatalf("cmdGC(--all --path) = %v, want usageError", err)
	}
}

func TestGCSelectionNonInteractive(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	candidates := []worktree{
		{Path: "/active", LastUsed: now.Add(-time.Hour)},
		{Path: "/stale", LastUsed: time.Time{}},
	}
	// captureStdout swaps stdout for a pipe so interactive() reads a non-TTY
	// stdout and the no-selection warning fires deterministically regardless of
	// the stdin/stdout terminals of `go test`.
	captureStdout(t, func() {
		if _, err := chooseCandidates(candidates, gcOptions{}, ttl); !isUsage(err) {
			t.Errorf("chooseCandidates with candidates but no selection = %v, want usageError", err)
		}
		if got, err := gcSelection(nil, gcOptions{}, ttl, nil); err != nil || got != nil {
			t.Errorf("gcSelection with no candidates = (%v,%v), want (nil,nil)", got, err)
		}
	})
}

// TestFailAbortedExitZero pins Fix A: a declined gc returns errAborted, which
// fail maps to exit 0 (the "aborted" message is already on stderr, so fail is
// silent). The exit-code contract (0/1/2) lives in main.go's fail().
func TestFailAbortedExitZero(t *testing.T) {
	if rc := fail(errAborted); rc != 0 {
		t.Fatalf("fail(errAborted) = %d, want 0", rc)
	}
}

// isUsage reports whether err is the exit-code-2 usage contract.
func isUsage(err error) bool {
	var ue usageError
	return errors.As(err, &ue)
}

func TestGCSelectionAll(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	wts := []worktree{
		{Path: "/main", Main: true, LastUsed: time.Time{}},
		{Path: "/active", LastUsed: now.Add(-time.Hour)},
		{Path: "/stale-zero", LastUsed: time.Time{}},
		{Path: "/stale-old", LastUsed: now.Add(-11 * time.Hour)},
	}
	got, err := gcSelection(wts, gcOptions{all: true}, ttl, nil)
	if err != nil {
		t.Fatalf("gcSelection(--all): %v", err)
	}
	want := map[string]bool{"/stale-zero": true, "/stale-old": true}
	if len(got) != len(want) {
		t.Fatalf("selected %d worktrees, want %d: %v", len(got), len(want), got)
	}
	for _, w := range got {
		if !want[w.Path] {
			t.Fatalf("unexpected selection %q (main/active must never be selected)", w.Path)
		}
		if w.Main {
			t.Fatalf("main worktree selected: %q", w.Path)
		}
	}
}

// gridColumnStarts is where each column begins, given the two-cell gutters.
func gridColumnStarts(widths []int) []int {
	starts := make([]int, len(widths))
	for i := 1; i < len(widths); i++ {
		starts[i] = starts[i-1] + widths[i-1] + 2
	}
	return starts
}

// assertColumnGutters is the alignment contract: every column boundary sits at
// the same display offset on every line, so the rendered grid is a true table.
// Offsets and widths are display cells, so the check is grapheme-safe and holds
// for middle-elided cells.
func assertColumnGutters(t *testing.T, widths []int, lines []string) {
	t.Helper()
	for _, line := range lines {
		for _, s := range gridColumnStarts(widths)[1:] {
			if dispWidth(line) < s {
				continue // trailing columns are trimmed
			}
			if !strings.HasSuffix(ansi.Truncate(line, s, ""), "  ") {
				t.Fatalf("column gutter before display offset %d lost in %q", s, line)
			}
		}
	}
}

// TestGCPickerGrid pins the picker's shared-grid projection: $HOME shorthand,
// no repeated UNUSED, and the two consequence columns (dirty, upstream) shown
// with aligned columns.
func TestGCPickerGrid(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	home := t.TempDir()
	t.Setenv("HOME", home)
	candidates := []worktree{
		{Path: filepath.Join(home, "wt1"), Branch: "feat", LastUsed: time.Time{}},
		{Path: filepath.Join(home, "wt2"), Branch: "bug/x", Dirty: true,
			Upstream: upstream{Short: "origin/bug/x", Remote: "origin", Ref: "refs/heads/bug/x"}},
		// Non-ASCII cells prove the alignment check measures display cells, not bytes.
		{Path: filepath.Join(home, "wt-ünïçødé"), Branch: "café", LastUsed: time.Time{}},
	}
	header, body, widths := pickerGrid(candidates, columnContext{now: now, ttl: ttl}, 0)
	if len(body) != 3 {
		t.Fatalf("picker grid rows = %d, want 3: %v", len(body), body)
	}
	joined := header + "\n" + strings.Join(body, "\n")
	if strings.Contains(joined, "UNUSED") {
		t.Fatalf("picker must not repeat UNUSED per row; grid:\n%s", joined)
	}
	if !strings.Contains(joined, "~") || strings.Contains(joined, home) {
		t.Fatalf("picker must shorten $HOME paths; got:\n%s", joined)
	}
	if !strings.Contains(header, "UNCOMMITTED") || !strings.Contains(header, "UPSTREAM") {
		t.Fatalf("picker header missing consequence columns: %q", header)
	}
	if !strings.Contains(body[1], "dirty") || !strings.Contains(body[1], "origin/bug/x") {
		t.Fatalf("dirty + upstream row = %q", body[1])
	}
	assertColumnGutters(t, widths, append([]string{header}, body...))
}

// TestGCPickerDescription pins the D3/D4 disclosure contract: the destructive
// remote effect is stated once, and the grid header is indented by exactly the
// option prefix so it sits above the keys.
func TestGCPickerDescription(t *testing.T) {
	header := "PATH  BRANCH"
	got := gcPickerDescription(false, header)
	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("description = %q, want a policy line and a header line", got)
	}
	if !strings.Contains(lines[0], "deleted") {
		t.Errorf("default policy must disclose remote deletion: %q", lines[0])
	}
	if !strings.Contains(gcPickerDescription(true, header), "kept") {
		t.Errorf("--keep-remote policy must say remotes are kept")
	}
	indent := len(lines[1]) - len(strings.TrimLeft(lines[1], " "))
	if indent != pickerOptionPrefixWidth {
		t.Errorf("header indent = %d, want %d so it aligns with option keys", indent, pickerOptionPrefixWidth)
	}
}

// TestGCPickerColumnBudget pins the fit arithmetic at every width: the label
// plus huh's chrome and option prefix must fit the terminal. This is the
// at-the-source guarantee a stripped PTY stream cannot assert reliably.
func TestGCPickerColumnBudget(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	t.Setenv("HOME", "/home/nobody")
	candidates := []worktree{{
		Path:   "/home/nobody/very/long/checkout/path/that/keeps/going/on/and/on/wt",
		Branch: "feature/a-rather-long-branch-name", Dirty: true,
		Upstream: upstream{Short: "origin/feature/a-rather-long-branch-name", Remote: "origin", Ref: "refs/heads/x"},
	}}
	headers := columnHeaders(gcPickerColumns)
	rows := gridRows(gcPickerColumns, candidates, columnContext{now: now, ttl: ttl})
	for _, cols := range []int{200, 120, 80, 60, 44} {
		t.Setenv("COLUMNS", strconv.Itoa(cols))
		widths := columnWidths(headers, rows, pickerBudget())
		if got := lineWidth(widths) + pickerChromeWidth + pickerOptionPrefixWidth; got > cols {
			t.Errorf("picker line = %d cells at %d cols, want <= %d", got, cols, cols)
		}
	}
}

// gcSeedDirs returns canonical worktree paths under root and materializes the
// non-main ones so canonical/symlinked path equivalence can be exercised.
func gcSeedDirs(t *testing.T, root string) (main, active, stale string) {
	t.Helper()
	main = canonical(filepath.Join(root, "main"))
	active = canonical(filepath.Join(root, "active"))
	stale = canonical(filepath.Join(root, "stale"))
	for _, p := range []string{active, stale} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return main, active, stale
}

func TestGCByPath(t *testing.T) {
	sandbox(t)
	now := time.Now()
	ttl := 10 * 24 * time.Hour
	root := t.TempDir()
	mainPath, activePath, stalePath := gcSeedDirs(t, root)
	wts := []worktree{
		{Path: mainPath, Main: true},
		{Path: activePath, LastUsed: now.Add(-time.Hour)},
		{Path: stalePath, LastUsed: time.Time{}},
	}

	t.Run("refuses the main worktree", func(t *testing.T) {
		_, err := gcByPath(wts, []string{mainPath}, ttl, now)
		if err == nil || !strings.Contains(err.Error(), "refusing to remove the main worktree") {
			t.Fatalf("gcByPath(main) = %v, want refusal", err)
		}
	})

	t.Run("errors on an unknown path", func(t *testing.T) {
		_, err := gcByPath(wts, []string{canonical(filepath.Join(root, "ghost"))}, ttl, now)
		if err == nil || !strings.Contains(err.Error(), "not a worktree") {
			t.Fatalf("gcByPath(unknown) = %v, want not-a-worktree", err)
		}
	})

	t.Run("dedupes and accepts a canonical-equivalent path", func(t *testing.T) {
		dotdot := filepath.Join(stalePath, "sub", "..")
		got, err := gcByPath(wts, []string{stalePath, dotdot}, ttl, now)
		if err != nil {
			t.Fatalf("gcByPath: %v", err)
		}
		if len(got) != 1 || got[0].Path != stalePath {
			t.Fatalf("gcByPath = %+v, want exactly [%s]", got, stalePath)
		}
	})

	t.Run("accepts a path through a symlinked ancestor", func(t *testing.T) {
		link := filepath.Join(t.TempDir(), "linkroot")
		mustSymlink(t, root, link)
		got, err := gcByPath(wts, []string{filepath.Join(link, "stale")}, ttl, now)
		if err != nil {
			t.Fatalf("gcByPath(symlinked): %v", err)
		}
		if len(got) != 1 || got[0].Path != stalePath {
			t.Fatalf("gcByPath = %+v, want exactly [%s]", got, stalePath)
		}
	})

	t.Run("selects an ACTIVE worktree with a warning", func(t *testing.T) {
		var got []worktree
		errOut := captureStderr(t, func() {
			var err error
			got, err = gcByPath(wts, []string{activePath}, ttl, now)
			if err != nil {
				t.Errorf("gcByPath(active): %v", err)
			}
		})
		if len(got) != 1 || got[0].Path != activePath {
			t.Fatalf("gcByPath(active) = %+v, want [%s]", got, activePath)
		}
		if !strings.Contains(errOut, "warning:") || !strings.Contains(errOut, "ACTIVE") {
			t.Fatalf("active removal must warn on stderr, got %q", errOut)
		}
	})
}

// gcTestLinkedWorktree builds a repo with one clean linked worktree that has a
// Pi session, returning canonical paths plus the session file.
func gcTestLinkedWorktree(t *testing.T) (repo, linked, session string) {
	t.Helper()
	repo = initRepo(t, "main")
	gitTestCommit(t, repo)
	linked = canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feat", "refs/heads/main")
	session = mkSession(t, linked, "linked-s", time.Now())
	return repo, linked, session
}

// gcRunRemove runs gcRemove under --json and returns the parsed report.
func gcRunRemove(t *testing.T, g *globals, repo string, selected []worktree, keepSessions, keepRemote bool) (gcResult, error) {
	t.Helper()
	var res gcResult
	var runErr error
	out := captureStdout(t, func() {
		res, runErr = gcRemove(repo, selected, planRemotes(selected, nil, keepRemote, repo), keepSessions, nil)
		if rerr := gcReport(g, res); rerr != nil {
			runErr = rerr
		}
	})
	if out != "" {
		var parsed gcResult
		if err := json.Unmarshal([]byte(out), &parsed); err != nil {
			t.Fatalf("gcRemove JSON %q: %v", out, err)
		}
	}
	return res, runErr
}

// gcRunRemoveNotices runs gcRemove behind a capture progress handle and returns
// the persistent notice lines a real renderer would have printed. It is how the
// tests see disclosures that must survive the renderer.
func gcRunRemoveNotices(t *testing.T, repo string, selected []worktree, keepSessions, keepRemote bool) (gcResult, []string) {
	t.Helper()
	events := make(chan progressEvent, 64)
	p := &progress{ctx: context.Background(), events: events}
	res, err := gcRemove(repo, selected, planRemotes(selected, nil, keepRemote, repo), keepSessions, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	return res, notices
}

// countNotices counts capture notices containing sub, so a test can pin exactly
// how many times a disclosure reaches the user.
func countNotices(notices []string, sub string) int {
	n := 0
	for _, line := range notices {
		if strings.Contains(line, sub) {
			n++
		}
	}
	return n
}

func TestGCRemove(t *testing.T) {
	sandbox(t)
	repo, linked, session := gcTestLinkedWorktree(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false, false)
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != linked {
		t.Fatalf("Removed = %v, want [%s]", res.Removed, linked)
	}
	if res.SessionsPurged != 1 {
		t.Fatalf("SessionsPurged = %d, want 1", res.SessionsPurged)
	}
	if !res.Pruned {
		t.Fatal("Pruned must be true after a successful removal")
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want none", res.Failed)
	}
	if exists(linked) {
		t.Fatalf("worktree path survived: %s", linked)
	}
	if exists(session) {
		t.Fatalf("session file survived: %s", session)
	}
}

func TestGCRemoveKeepSessions(t *testing.T) {
	sandbox(t)
	repo, linked, session := gcTestLinkedWorktree(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, true, false)
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if !res.KeptSessions {
		t.Fatal("KeptSessions must be true")
	}
	if res.SessionsPurged != 0 {
		t.Fatalf("SessionsPurged = %d, want 0", res.SessionsPurged)
	}
	if !exists(session) {
		t.Fatalf("--keep-sessions must preserve %s", session)
	}
	if exists(linked) {
		t.Fatalf("worktree path survived: %s", linked)
	}
}

// Explicit selection is consent: a dirty worktree is force-removed with no
// extra flag or prompt. The picker is what makes the choice informed.
func TestGCRemoveDirty(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcTestLinkedWorktree(t)
	writeFile(t, filepath.Join(linked, "dirty.txt"), "x", 0o644)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false, false)
	if err != nil {
		t.Fatalf("gcRemove(dirty): %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != linked || len(res.Failed) != 0 {
		t.Fatalf("Removed/Failed = %v/%v, want [%s]/none", res.Removed, res.Failed, linked)
	}
	if exists(linked) {
		t.Fatalf("dirty worktree survived: %s", linked)
	}
}

// A worktree whose directory survives but whose .git backlink was deleted
// out-of-band is exactly what `git worktree remove` refuses; gc must repair the
// link, then still delete both the directory and its admin entry.
func TestGCRemoveMissingGitLink(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcTestLinkedWorktree(t)
	if err := os.Remove(filepath.Join(linked, ".git")); err != nil {
		t.Fatal(err)
	}
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false, false)
	if err != nil {
		t.Fatalf("gcRemove(broken link): %v", err)
	}
	if len(res.Removed) != 1 || res.Removed[0] != linked || len(res.Failed) != 0 {
		t.Fatalf("Removed/Failed = %v/%v, want [%s]/none", res.Removed, res.Failed, linked)
	}
	if exists(linked) {
		t.Fatalf("broken worktree survived: %s", linked)
	}
	if out := mustGit(t, repo, "worktree", "list", "--porcelain"); strings.Contains(out, linked) {
		t.Fatalf("worktree still registered after gc:\n%s", out)
	}
}

// A run where every removal fails must still prune stale admin entries: the
// old `Removed > 0` gate skipped prune precisely here, wedging later gc runs.
func TestGCRemovePrunesWhenNothingRemoved(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	locked := canonical(filepath.Join(filepath.Dir(repo), "locked"))
	stale := canonical(filepath.Join(filepath.Dir(repo), "stale"))
	gitWorktreeAdd(t, repo, locked, "locked", "refs/heads/main")
	gitWorktreeAdd(t, repo, stale, "stale", "refs/heads/main")
	mustGit(t, repo, "worktree", "lock", locked)
	mustGit(t, repo, "config", "gc.worktreePruneExpire", "now")
	if err := os.RemoveAll(stale); err != nil {
		t.Fatal(err)
	}
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: locked}}, false, false)
	if err == nil {
		t.Fatal("gcRemove(locked) succeeded, want failure")
	}
	if len(res.Failed) == 0 {
		t.Fatalf("Failed = %v, want the locked worktree", res.Failed)
	}
	if !res.Pruned {
		t.Fatal("Pruned must be true: prune must run even when nothing was removed")
	}
	out := mustGit(t, repo, "worktree", "list", "--porcelain")
	if strings.Contains(out, stale) {
		t.Fatalf("stale worktree not pruned:\n%s", out)
	}
	if !strings.Contains(out, locked) {
		t.Fatalf("locked worktree must remain registered:\n%s", out)
	}
}

func TestGCReport(t *testing.T) {
	t.Run("json shape", func(t *testing.T) {
		want := gcResult{
			Removed:        []string{"/a", "/b"},
			RemoteDeleted:  []string{"origin/a"},
			Failed:         []gcFailure{{Path: "/c", Error: "boom"}},
			SessionsPurged: 3,
			KeptSessions:   true,
			KeptRemote:     true,
			Pruned:         true,
		}
		out := captureStdout(t, func() {
			if err := gcReport(&globals{json: true}, want); err != nil {
				t.Errorf("gcReport: %v", err)
			}
		})
		var keys map[string]json.RawMessage
		if err := json.Unmarshal([]byte(out), &keys); err != nil {
			t.Fatalf("gcReport JSON: %v", err)
		}
		wantKeys := []string{"removed", "remoteDeleted", "failed", "sessionsPurged", "keptSessions", "keptRemote", "pruned"}
		if len(keys) != len(wantKeys) {
			t.Fatalf("JSON keys = %v, want %v", keys, wantKeys)
		}
		for _, k := range wantKeys {
			if _, ok := keys[k]; !ok {
				t.Fatalf("missing JSON key %q in %v", k, keys)
			}
		}
		var got gcResult
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatal(err)
		}
		if got.SessionsPurged != want.SessionsPurged || got.Pruned != want.Pruned ||
			got.KeptSessions != want.KeptSessions || got.KeptRemote != want.KeptRemote ||
			len(got.Removed) != 2 || len(got.RemoteDeleted) != 1 ||
			len(got.Failed) != 1 || got.Failed[0].Error != "boom" {
			t.Fatalf("gcReport round-trip = %+v, want %+v", got, want)
		}
	})

	t.Run("nothing to do", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := gcReport(&globals{}, gcResult{Removed: []string{}, Failed: []gcFailure{}}); err != nil {
				t.Errorf("gcReport: %v", err)
			}
		})
		if out != "nothing to do\n" {
			t.Fatalf("gcReport = %q, want nothing-to-do line", out)
		}
	})

	t.Run("kept and pruned lines", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := gcReport(&globals{}, gcResult{
				Removed: []string{"/x"}, Failed: []gcFailure{}, KeptSessions: true, Pruned: true,
			}); err != nil {
				t.Errorf("gcReport: %v", err)
			}
		})
		if want := "removed /x\npruned\nsessions kept\n"; out != want {
			t.Fatalf("gcReport = %q, want %q", out, want)
		}
	})

	t.Run("purged count and failed stderr", func(t *testing.T) {
		var errOut string
		out := captureStdout(t, func() {
			errOut = captureStderr(t, func() {
				if err := gcReport(&globals{}, gcResult{
					Removed: []string{}, Failed: []gcFailure{{Path: "/p", Error: "boom"}}, SessionsPurged: 4,
				}); err != nil {
					t.Errorf("gcReport: %v", err)
				}
			})
		})
		if out != "sessions purged: 4\n" {
			t.Fatalf("stdout = %q, want purged count", out)
		}
		if errOut != "failed /p: boom\n" {
			t.Fatalf("stderr = %q, want failed line", errOut)
		}
	})

	t.Run("deleted remote and kept remote lines", func(t *testing.T) {
		out := captureStdout(t, func() {
			if err := gcReport(&globals{}, gcResult{
				Removed: []string{"/x"}, RemoteDeleted: []string{"origin/feat"}, Failed: []gcFailure{}, Pruned: true,
			}); err != nil {
				t.Errorf("gcReport: %v", err)
			}
		})
		if want := "removed /x\ndeleted remote origin/feat\npruned\nsessions purged: 0\n"; out != want {
			t.Fatalf("gcReport = %q, want %q", out, want)
		}
		out = captureStdout(t, func() {
			if err := gcReport(&globals{}, gcResult{Removed: []string{"/x"}, Failed: []gcFailure{}, KeptRemote: true}); err != nil {
				t.Errorf("gcReport: %v", err)
			}
		})
		if want := "removed /x\nsessions purged: 0\nremote branches kept\n"; out != want {
			t.Fatalf("gcReport(keep) = %q, want %q", out, want)
		}
	})
}

// TestGCAbortKeepsRemoteJSON pins the abort/empty report: with --keep-remote
// a gc that removes nothing still reports keptRemote (no remote branch was
// deleted), so --json clients see the flag honored instead of a false echo.
func TestGCAbortKeepsRemoteJSON(t *testing.T) {
	res := abortGCResult(gcOptions{keepSessions: true, keepRemote: true})
	if !res.KeptRemote || !res.KeptSessions {
		t.Fatalf("abort result = %+v, want kept sessions+remote", res)
	}
	out := captureStdout(t, func() {
		if err := gcReport(&globals{json: true}, res); err != nil {
			t.Errorf("gcReport: %v", err)
		}
	})
	var got gcResult
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("abort JSON %q: %v", out, err)
	}
	if !got.KeptRemote {
		t.Fatalf("abort JSON keptRemote = false, want true in %s", out)
	}
	if res := abortGCResult(gcOptions{}); res.KeptRemote || res.KeptSessions {
		t.Fatalf("default abort result = %+v, want no keeps", res)
	}
}

// gcFixtureWithUpstream seeds a linked worktree whose branch tracks a branch on
// a local bare origin: a real remote that works offline.
func gcFixtureWithUpstream(t *testing.T) (repo, linked, origin string) {
	t.Helper()
	repo, origin = gitTestUpstreamFixture(t)
	linked = canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feat", "refs/heads/main")
	mustGit(t, repo, "push", "-q", "origin", "feat:feat")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat")
	return repo, linked, origin
}

// discovered returns the one discovered worktree at path, with its upstream.
func discovered(t *testing.T, repo, path string) worktree {
	t.Helper()
	wts, err := discover(repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range wts {
		if w.Path == path {
			return w
		}
	}
	t.Fatalf("worktree %s not discovered", path)
	return worktree{}
}

func TestGCRemoteDelete(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if len(res.RemoteDeleted) != 1 || res.RemoteDeleted[0] != "origin/feat" {
		t.Fatalf("RemoteDeleted = %v, want [origin/feat]", res.RemoteDeleted)
	}
	if remoteHasBranch(t, repo, origin, "feat") {
		t.Fatalf("remote branch feat survived gc")
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
}

func TestGCRemoteKeep(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, true)
	if err != nil {
		t.Fatalf("gcRemove(keep-remote): %v", err)
	}
	if !res.KeptRemote || len(res.RemoteDeleted) != 0 {
		t.Fatalf("KeptRemote=%v RemoteDeleted=%v, want true/[]", res.KeptRemote, res.RemoteDeleted)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatalf("--keep-remote must preserve the remote branch")
	}
}

// TestGCRemoteFailureExitsNonZero pins the failure contract: a rejected remote
// delete is reported and makes gcRemove non-zero while the worktree is removed.
func TestGCRemoteFailureExitsNonZero(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, origin, "config", "receive.denyDeletes", "true")
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if err == nil {
		t.Fatal("gcRemove succeeded, want a remote-delete failure")
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "delete remote origin/feat") {
		t.Fatalf("Failed = %v, want one delete-remote failure", res.Failed)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatalf("rejected delete must leave the remote branch intact")
	}
}

// TestGCRemoteUnreachableFails pins that an unreachable remote is a reported
// failure, not a silent "already gone": the local worktree is still removed,
// but the failed existence probe lands in Failed and gc exits non-zero.
func TestGCRemoteUnreachableFails(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcFixtureWithUpstream(t)
	mustGit(t, repo, "remote", "set-url", "origin", filepath.Join(repo, "no-such.git"))
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if err == nil {
		t.Fatal("gcRemove succeeded, want a remote-delete failure")
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "delete remote origin/feat") {
		t.Fatalf("Failed = %v, want one delete-remote failure", res.Failed)
	}
	if exists(linked) {
		t.Fatalf("local worktree must still be removed: %s", linked)
	}
}

// TestGCRemoteNotDeletedWhenRemovalFails pins the purge ordering: the remote
// delete runs only after the local worktree is gone, so a failed local removal
// never orphans a still-needed remote branch.
func TestGCRemoteNotDeletedWhenRemovalFails(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, repo, "worktree", "lock", linked)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if err == nil {
		t.Fatal("gcRemove(locked) succeeded, want the local removal to fail")
	}
	if len(res.Failed) == 0 {
		t.Fatalf("Failed = %v, want the locked worktree", res.Failed)
	}
	if len(res.RemoteDeleted) != 0 {
		t.Fatalf("RemoteDeleted = %v, want none: the remote branch must outlive a failed local removal", res.RemoteDeleted)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("remote branch feat was deleted although the local removal failed")
	}
	if !exists(linked) {
		t.Fatal("locked worktree was removed")
	}
}

// TestGCRemoteAbsentNoOp pins decision 3: no upstream behaves exactly as
// before — no remote work, no failure, empty remote report fields.
func TestGCRemoteAbsentNoOp(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcTestLinkedWorktree(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if len(res.RemoteDeleted) != 0 || res.KeptRemote {
		t.Fatalf("no-upstream result = remoteDeleted:%v keptRemote:%v, want none/false", res.RemoteDeleted, res.KeptRemote)
	}
}

func TestGCPromptTitle(t *testing.T) {
	if got := gcPromptTitle(2, 0); got != "Remove 2 worktree(s)?" {
		t.Fatalf("no-remote title = %q", got)
	}
	if got := gcPromptTitle(2, 3); got != "Remove 2 worktree(s) and delete 3 remote branch(es)?" {
		t.Fatalf("remote title = %q", got)
	}
}

func TestRemotePlanCount(t *testing.T) {
	selected := []worktree{
		{Upstream: upstream{Short: "origin/feat", Remote: "origin", Ref: "refs/heads/feat"}},
		{},
		{Upstream: upstream{Short: "o/x", Remote: "o", Ref: "refs/heads/x"}},
		// A local-branch upstream is configured but never a delete target.
		{Upstream: upstream{Short: "base", Remote: ".", Ref: "refs/heads/base"}},
	}
	// Root "" skips the existence probe so these cases stay offline.
	if got := planRemotes(selected, nil, false, "").count(); got != 2 {
		t.Fatalf("plan count = %d, want 2", got)
	}
	if got := planRemotes(selected, nil, true, "").count(); got != 0 {
		t.Fatalf("plan count(keepRemote) = %d, want 0", got)
	}
	// A shared upstream counts once, matching deleteRemote's dedup.
	shared := []worktree{
		{Upstream: upstream{Short: "origin/feat", Remote: "origin", Ref: "refs/heads/feat"}},
		{Upstream: upstream{Short: "origin/feat", Remote: "origin", Ref: "refs/heads/feat"}},
	}
	if got := planRemotes(shared, nil, false, "").count(); got != 1 {
		t.Fatalf("plan count(shared) = %d, want 1", got)
	}
	// An upstream tracked by a surviving worktree is excluded from the count.
	surviving := []worktree{
		{Path: "/survivor", Upstream: upstream{Short: "origin/feat", Remote: "origin", Ref: "refs/heads/feat"}},
	}
	if got := planRemotes(shared, surviving, false, "").count(); got != 0 {
		t.Fatalf("plan count(with survivor) = %d, want 0", got)
	}
}

// TestRemotePlanStaysOffline pins that planning is a pure local computation:
// an upstream deleted out of band is still a target (no pre-consent network
// probe), and the runtime no-op is pinned by TestGCRemoteAlreadyGone instead.
func TestRemotePlanStaysOffline(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, origin, "update-ref", "-d", "refs/heads/feat")
	selected := []worktree{discovered(t, repo, linked)}
	plan := planRemotes(selected, nil, false, repo)
	if plan.count() != 1 || len(plan.targets) != 1 {
		t.Fatalf("plan = count %d, targets %v; want one offline target", plan.count(), plan.targets)
	}
	if _, ok := plan.targets[selected[0].Upstream.key()]; !ok {
		t.Fatalf("plan targets = %v, want only %s", plan.targets, selected[0].Upstream.key())
	}
}

// TestGCRemoteLocalUpstreamNoOp pins the local-upstream guard: git marks an
// upstream that is another LOCAL branch as remote ".". A push-delete to "."
// would destroy that tracked local branch, so gc must skip it entirely — the
// worktree is removed, both local branches survive, and nothing is reported.
func TestGCRemoteLocalUpstreamNoOp(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcTestLinkedWorktree(t)
	gitTestBranch(t, repo, "base")
	mustGit(t, repo, "config", "branch.feat.remote", ".")
	mustGit(t, repo, "config", "branch.feat.merge", "refs/heads/base")

	w := discovered(t, repo, linked)
	if !w.Upstream.present() || w.Upstream.onRemote() {
		t.Fatalf("local upstream = %+v, want present but not onRemote", w.Upstream)
	}
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{w}, false, false)
	if err != nil {
		t.Fatalf("gcRemove(local upstream): %v", err)
	}
	if len(res.RemoteDeleted) != 0 || res.KeptRemote || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v keptRemote:%v failed:%v, want all empty", res.RemoteDeleted, res.KeptRemote, res.Failed)
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
	mustGit(t, repo, "rev-parse", "--verify", "refs/heads/base")
}

// TestGCRemoteDeduplicates pins the duplicate-upstream guard: two worktrees
// tracking the same remote branch delete it once, and the run still exits 0.
// The "deleting remote" notice must appear exactly once: emitting it before the
// dedup would disclose a delete that never happens.
func TestGCRemoteDeduplicates(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcFixtureWithUpstream(t)
	linked2 := canonical(filepath.Join(filepath.Dir(repo), "linked2"))
	gitWorktreeAdd(t, repo, linked2, "feat2", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat2")
	w1 := discovered(t, repo, linked)
	w2 := discovered(t, repo, linked2)
	if w1.Upstream.Short != "origin/feat" || w2.Upstream.Short != "origin/feat" {
		t.Fatalf("upstreams = %q/%q, want both origin/feat", w1.Upstream.Short, w2.Upstream.Short)
	}
	res, notices := gcRunRemoveNotices(t, repo, []worktree{w1, w2}, false, false)
	if len(res.RemoteDeleted) != 1 || res.RemoteDeleted[0] != "origin/feat" {
		t.Fatalf("RemoteDeleted = %v, want exactly [origin/feat]", res.RemoteDeleted)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("Failed = %v, want none: the duplicate delete must be skipped, not failed", res.Failed)
	}
	if got := countNotices(notices, "deleting remote origin/feat"); got != 1 {
		t.Fatalf("deleting-remote notices = %d (%v), want exactly 1", got, notices)
	}
}

// TestGCRemoteSharedFailureDeduped pins the shared-upstream failure contract:
// when two worktrees track the same remote branch and the delete is rejected,
// the failure lands in Failed once per upstream key — not once per sharer —
// and gc still exits non-zero. Both local worktrees are still removed.
func TestGCRemoteSharedFailureDeduped(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	linked2 := canonical(filepath.Join(filepath.Dir(repo), "linked2"))
	gitWorktreeAdd(t, repo, linked2, "feat2", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat2")
	mustGit(t, origin, "config", "receive.denyDeletes", "true")
	w1 := discovered(t, repo, linked)
	w2 := discovered(t, repo, linked2)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{w1, w2}, false, false)
	if err == nil {
		t.Fatal("gcRemove succeeded, want a remote-delete failure")
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "delete remote origin/feat") {
		t.Fatalf("Failed = %v, want one deduped delete-remote failure", res.Failed)
	}
	if len(res.RemoteDeleted) != 0 {
		t.Fatalf("RemoteDeleted = %v, want none: the rejected delete must not report", res.RemoteDeleted)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("rejected delete must leave the remote branch intact")
	}
}

// TestGCRemoteAlreadyGone pins remote-delete idempotency: an upstream whose
// remote branch is already gone (deleted out of band, or by an interrupted run)
// is a converged no-op, so gc exits 0 with an empty remote report instead of
// retrying a delete that can never succeed.
func TestGCRemoteAlreadyGone(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, origin, "update-ref", "-d", "refs/heads/feat")
	res, notices := gcRunRemoveNotices(t, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v failed:%v, want both empty", res.RemoteDeleted, res.Failed)
	}
	if got := countNotices(notices, "deleting remote"); got != 0 {
		t.Fatalf("absent upstream must not disclose a delete: %v", notices)
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
}

// TestGCRemoteSkipsDefaultBranch pins the safety guard: a worktree branch that
// tracks the remote's default branch must never delete it — the worktree is
// still removed, the remote default survives, and the skip is disclosed.
func TestGCRemoteSkipsDefaultBranch(t *testing.T) {
	sandbox(t)
	repo, origin := gitTestUpstreamFixture(t)
	gitTestRemoteHead(t, repo, "origin", "main")
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked-default"))
	gitWorktreeAdd(t, repo, linked, "wt-default", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/main", "wt-default")

	w := discovered(t, repo, linked)
	if w.Upstream.Ref != "refs/heads/main" {
		t.Fatalf("upstream = %+v, want refs/heads/main", w.Upstream)
	}
	res, notices := gcRunRemoveNotices(t, repo, []worktree{w}, false, false)
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v failed:%v, want both empty", res.RemoteDeleted, res.Failed)
	}
	if !remoteHasBranch(t, repo, origin, "main") {
		t.Fatal("remote default branch main was deleted")
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
	if got := countNotices(notices, "default branch"); got != 1 {
		t.Fatalf("default-branch notices = %d (%v), want exactly 1", got, notices)
	}
}

// TestGCRemoteProtectsDefaultBranchWithoutRemoteHead pins the fallback safety
// guard: even if refs/remotes/origin/HEAD was never created in the local clone,
// the default branch is still resolved and protected against remote deletion.
func TestGCRemoteProtectsDefaultBranchWithoutRemoteHead(t *testing.T) {
	sandbox(t)
	repo, origin := gitTestUpstreamFixture(t)
	// Deliberately do NOT call gitTestRemoteHead: origin/HEAD does not exist.
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked-default-nohead"))
	gitWorktreeAdd(t, repo, linked, "wt-default-nohead", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/main", "wt-default-nohead")

	w := discovered(t, repo, linked)
	res, notices := gcRunRemoveNotices(t, repo, []worktree{w}, false, false)
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v failed:%v, want both empty", res.RemoteDeleted, res.Failed)
	}
	if !remoteHasBranch(t, repo, origin, "main") {
		t.Fatal("remote default branch main was deleted when origin/HEAD was missing")
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
	if got := countNotices(notices, "default branch"); got != 1 {
		t.Fatalf("default-branch notices = %d (%v), want exactly 1", got, notices)
	}
}

// TestGCRemoteProtectsDefaultBranchOnSecondRemote pins the offline fallback when
// another remote owns the only recorded HEAD: the repo-wide default name must
// still protect a same-named branch on a remote that has no <remote>/HEAD.
func TestGCRemoteProtectsDefaultBranchOnSecondRemote(t *testing.T) {
	sandbox(t)
	repo, _ := gitTestUpstreamFixture(t)
	gitTestRemoteHead(t, repo, "origin", "main")
	fork := filepath.Join(filepath.Dir(repo), "fork.git")
	mustGit(t, repo, "init", "-q", "--bare", "-b", "main", fork)
	// Without this, git refuses the delete itself, masking the guard under test.
	mustGit(t, fork, "config", "receive.denyDeleteCurrent", "ignore")
	mustGit(t, repo, "remote", "add", "fork", fork)
	mustGit(t, repo, "push", "-q", "fork", "main")
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked-fork"))
	gitWorktreeAdd(t, repo, linked, "wt-fork", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=fork/main", "wt-fork")

	res, notices := gcRunRemoveNotices(t, repo, []worktree{discovered(t, repo, linked)}, false, false)
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v failed:%v, want both empty", res.RemoteDeleted, res.Failed)
	}
	if !remoteHasBranch(t, repo, fork, "main") {
		t.Fatal("default branch main on the second remote was deleted")
	}
	if exists(linked) {
		t.Fatalf("worktree survived: %s", linked)
	}
	if got := countNotices(notices, "default branch"); got != 1 {
		t.Fatalf("default-branch notices = %d (%v), want exactly 1", got, notices)
	}
}

// TestGCRemoteSurvivesWhenTrackedByActiveWorktree proves that when multiple
// worktrees track the same remote upstream, removing one does not delete the
// remote branch if a surviving worktree still tracks it.
func TestGCRemoteSurvivesWhenTrackedByActiveWorktree(t *testing.T) {
	sandbox(t)
	repo, linked1, origin := gcFixtureWithUpstream(t)
	linked2 := canonical(filepath.Join(filepath.Dir(repo), "linked2"))
	gitWorktreeAdd(t, repo, linked2, "feat2", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat2")

	w1 := discovered(t, repo, linked1)
	w2 := discovered(t, repo, linked2)

	events := make(chan progressEvent, 64)
	p := &progress{ctx: context.Background(), events: events}
	res, err := gcRemove(repo, []worktree{w1}, planRemotes([]worktree{w1}, []worktree{w2}, false, repo), false, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = remoteDeleted:%v failed:%v, want no remote deletion", res.RemoteDeleted, res.Failed)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("remote branch feat was deleted even though surviving linked2 tracks it")
	}
	if exists(linked1) {
		t.Fatalf("worktree 1 survived: %s", linked1)
	}
	if got := countNotices(notices, "still tracked by"); got != 1 {
		t.Fatalf("still-tracked notices = %d (%v), want exactly 1", got, notices)
	}
}

// TestGCRemoteSharedVetoedUpstreamWarnsOnce pins that two selected worktrees
// tracking the same vetoed upstream disclose the veto once, not once per sharer.
func TestGCRemoteSharedVetoedUpstreamWarnsOnce(t *testing.T) {
	sandbox(t)
	repo, linked1, origin := gcFixtureWithUpstream(t)
	linked2 := canonical(filepath.Join(filepath.Dir(repo), "linked2"))
	gitWorktreeAdd(t, repo, linked2, "feat2", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat2")
	linked3 := canonical(filepath.Join(filepath.Dir(repo), "linked3"))
	gitWorktreeAdd(t, repo, linked3, "feat3", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat3")

	w1 := discovered(t, repo, linked1)
	w2 := discovered(t, repo, linked2)
	w3 := discovered(t, repo, linked3)

	events := make(chan progressEvent, 64)
	p := &progress{ctx: context.Background(), events: events}
	res, err := gcRemove(repo, []worktree{w1, w2}, planRemotes([]worktree{w1, w2}, []worktree{w3}, false, repo), false, p)
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if err != nil {
		t.Fatalf("gcRemove: %v", err)
	}
	if len(res.Removed) != 2 || len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 {
		t.Fatalf("result = removed:%v remoteDeleted:%v failed:%v", res.Removed, res.RemoteDeleted, res.Failed)
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("vetoed remote branch must survive")
	}
	if got := countNotices(notices, "still tracked by"); got != 1 {
		t.Fatalf("shared-veto notices = %d (%v), want exactly 1", got, notices)
	}
}
