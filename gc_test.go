package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
		if _, err := gcSelection(candidates, gcOptions{}, ttl); !isUsage(err) {
			t.Errorf("gcSelection with candidates but no selection = %v, want usageError", err)
		}
		if got, err := gcSelection(nil, gcOptions{}, ttl); err != nil || got != nil {
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
	got, err := gcSelection(wts, gcOptions{all: true}, ttl)
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

// candidateLabel is the gc picker's informative line: dirty is surfaced so the
// user's keep/remove choice is informed, and nothing is auto-filtered.
func TestCandidateLabel(t *testing.T) {
	now := time.Now()
	ttl := 10 * time.Hour
	clean := worktree{Path: "/w", Branch: "feat", LastUsed: time.Time{}}
	dirty := clean
	dirty.Dirty = true
	if got := candidateLabel(clean, now, ttl); got != "/w  (feat, UNUSED)" {
		t.Fatalf("clean label = %q", got)
	}
	if got := candidateLabel(dirty, now, ttl); got != "/w  (feat, UNUSED, dirty)" {
		t.Fatalf("dirty label = %q", got)
	}
	if got := candidateLabel(worktree{Path: "/d", Main: false}, now, ttl); got != "/d  ((detached), UNUSED)" {
		t.Fatalf("detached label = %q", got)
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
func gcRunRemove(t *testing.T, g *globals, repo string, selected []worktree, keepSessions bool) (gcResult, error) {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() { runErr = gcRemove(g, repo, selected, keepSessions) })
	var res gcResult
	if out != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("gcRemove JSON %q: %v", out, err)
		}
	}
	return res, runErr
}

func TestGCRemove(t *testing.T) {
	sandbox(t)
	repo, linked, session := gcTestLinkedWorktree(t)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false)
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
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, true)
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
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false)
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
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: linked}}, false)
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
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{{Path: locked}}, false)
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
			Failed:         []gcFailure{{Path: "/c", Error: "boom"}},
			SessionsPurged: 3,
			KeptSessions:   true,
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
		wantKeys := []string{"removed", "failed", "sessionsPurged", "keptSessions", "pruned"}
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
			got.KeptSessions != want.KeptSessions || len(got.Removed) != 2 ||
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
}
