package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

func idleTempCheckout(t *testing.T) (string, core.Worktree, string) {
	t.Helper()
	repo, _, w := identityCheckout(t)
	path := w.Path
	seedTempRecord(t, path, w.Branch, "1h", TempTestTime)
	session := MkSession(t, path, "idle", TempTestTime)
	commitIdleBaseline(t, path)
	BackdateGitActivity(t, path, TempTestTime)
	w = discovered(t, repo, path)
	if !w.Unused(TempNow, time.Hour) {
		t.Fatal("fixture is not idle")
	}
	return repo, w, session
}

func commitIdleBaseline(t *testing.T, path string) {
	t.Helper()
	WriteFile(t, filepath.Join(path, "tracked.txt"), "initial", 0600)
	MustGit(t, path, "add", "tracked.txt")
	// Old commit time isolates subsequent edits and staging as activity.
	t.Setenv("GIT_COMMITTER_DATE", TempTestTime.Format(time.RFC3339))
	MustGit(t, path, "commit", "-qm", "old", "--date="+TempTestTime.Format(time.RFC3339))
}

func activateTemp(t *testing.T, w core.Worktree, activity string) {
	t.Helper()
	switch activity {
	case "edit":
		WriteFile(t, filepath.Join(w.Path, "new.txt"), "keep", 0600)
	case "commit":
		t.Setenv("GIT_COMMITTER_DATE", tempActivation.Format(time.RFC3339))
		MustGit(t, w.Path, "commit", "--allow-empty", "-qm", "keep")
		// WHY: staging refreshes the private index with wall-clock time; pin it
		// back so only the commit timestamp can prove the checkout is ACTIVE.
		BackdateActivityIndex(t, w.Path, TempTestTime)
	case "pi":
		MkSession(t, w.Path, "new", TempNow)
	}
}

// Cleanup reuses discovery's real ownership index, independent of idle refresh.
func cleanupSessionIndex(t *testing.T, repo string) core.SessionIndex {
	t.Helper()
	ws, err := core.LoadWorkspace(&core.Globals{Root: repo}, false, defaultHooks())
	if err != nil {
		t.Fatal(err)
	}
	return ws.Idx
}

func runIdleCleanup(t *testing.T, repo string, w core.Worktree, idx core.SessionIndex) (gcResult, string) {
	t.Helper()
	var res gcResult
	warnings := CaptureStderr(t, func() {
		err := withProgress(&core.Globals{JSON: true}, true, func(p *ui.Progress) error {
			var err error
			res, err = gcCleanup(repo, []core.Worktree{w}, remotePlan{keep: true}, false, idx, gcMode{deleteTemp: true, idleOnly: true}, TempNow, p)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	return res, warnings
}

func TestGCRefreshesTempActivityAfterSelection(t *testing.T) {
	for _, activity := range []string{"edit", "commit", "pi"} {
		t.Run(activity, func(t *testing.T) {
			repo, w, session := idleTempCheckout(t)
			before := mustReadTempStore(t)
			idx := cleanupSessionIndex(t, repo)
			activateTemp(t, w, activity)
			res, warnings := runIdleCleanup(t, repo, w, idx)
			assertTempStore(t, before)
			if !fs.Exists(w.Path) || !fs.Exists(session) || !git.RefExists(repo, "refs/heads/"+w.Branch) {
				t.Fatal("ACTIVE checkout resources were removed")
			}
			if len(res.Removed)+len(res.Skipped)+len(res.DeletedBranches)+len(res.RemoteDeleted)+len(res.Failed)+res.SessionsPurged != 1 {
				t.Fatalf("skip not reported as the only outcome: %+v", res)
			}
			if len(res.Skipped) != 1 || res.Skipped[0] != w.Path {
				t.Fatalf("Skipped = %v, want [%s]", res.Skipped, w.Path)
			}
			if !strings.Contains(warnings, "ACTIVE") {
				t.Fatalf("skip reason missing: %s", warnings)
			}
		})
	}
}

func TestGCRemovesStillIdleTemp(t *testing.T) {
	repo, w, session := idleTempCheckout(t)
	res, _ := runIdleCleanup(t, repo, w, cleanupSessionIndex(t, repo))
	if fs.Exists(w.Path) || fs.Exists(session) || git.RefExists(repo, "refs/heads/"+w.Branch) {
		t.Fatal("idle checkout resources survived")
	}
	if len(res.Removed) != 1 || len(res.DeletedBranches) != 1 {
		t.Fatalf("cleanup = %+v", res)
	}
	assertRemovedTempRecord(t, w.Path)
}
