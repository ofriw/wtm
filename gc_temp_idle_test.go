package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func idleTempCheckout(t *testing.T) (string, worktree, string) {
	t.Helper()
	repo, _, w := identityCheckout(t)
	path := w.Path
	seedTempRecord(t, path, w.Branch, "1h", tempTestTime)
	session := mkSession(t, path, "idle", tempTestTime)
	commitIdleBaseline(t, path)
	backdateGitActivity(t, path, tempTestTime)
	w = discovered(t, repo, path)
	if !w.unused(tempNow, time.Hour) {
		t.Fatal("fixture is not idle")
	}
	return repo, w, session
}

func commitIdleBaseline(t *testing.T, path string) {
	t.Helper()
	writeFile(t, filepath.Join(path, "tracked.txt"), "initial", 0600)
	mustGit(t, path, "add", "tracked.txt")
	// Old commit time isolates subsequent edits and staging as activity.
	t.Setenv("GIT_COMMITTER_DATE", tempTestTime.Format(time.RFC3339))
	mustGit(t, path, "commit", "-qm", "old", "--date="+tempTestTime.Format(time.RFC3339))
}

func activateTemp(t *testing.T, w worktree, activity string) {
	t.Helper()
	switch activity {
	case "edit":
		writeFile(t, filepath.Join(w.Path, "new.txt"), "keep", 0600)
	case "commit":
		t.Setenv("GIT_COMMITTER_DATE", tempActivation.Format(time.RFC3339))
		mustGit(t, w.Path, "commit", "--allow-empty", "-qm", "keep")
		// WHY: staging refreshes the private index with wall-clock time; pin it
		// back so only the commit timestamp can prove the checkout is ACTIVE.
		backdateActivityIndex(t, w.Path, tempTestTime)
	case "pi":
		mkSession(t, w.Path, "new", tempNow)
	}
}

// Cleanup reuses discovery's real ownership index, independent of idle refresh.
func cleanupSessionIndex(t *testing.T, repo string) sessionIndex {
	t.Helper()
	_, idx, err := discover(repo, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

func runIdleCleanup(t *testing.T, repo string, w worktree, idx sessionIndex) (gcResult, string) {
	t.Helper()
	var res gcResult
	warnings := captureStderr(t, func() {
		err := withProgress(&globals{json: true}, true, func(p *progress) error {
			var err error
			res, err = gcCleanup(repo, []worktree{w}, remotePlan{keep: true}, false, idx, gcMode{deleteTemp: true, idleOnly: true}, tempNow, p)
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
			if !exists(w.Path) || !exists(session) || !refExists(repo, "refs/heads/"+w.Branch) {
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
	if exists(w.Path) || exists(session) || refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatal("idle checkout resources survived")
	}
	if len(res.Removed) != 1 || len(res.DeletedBranches) != 1 {
		t.Fatalf("cleanup = %+v", res)
	}
	assertRemovedTempRecord(t, w.Path)
}
