package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func backdateActivityIndex(t *testing.T, path string, when time.Time) string {
	t.Helper()
	dir, err := gitPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	index := filepath.Join(dir, "index")
	setMtime(t, index, when)
	return index
}

func backdateGitActivity(t *testing.T, path string, when time.Time) {
	t.Helper()
	backdateActivityIndex(t, path, when)
	setMtime(t, filepath.Join(path, ".git"), when)
}

func stageTempActivity(t *testing.T, w worktree, kind string) string {
	t.Helper()
	path := filepath.Join(w.Path, "tracked.txt")
	if kind == "new" {
		path = filepath.Join(w.Path, "new.txt")
	}
	if kind == "delete" {
		mustGit(t, w.Path, "rm", "tracked.txt")
		return ""
	}
	writeFile(t, path, "staged activity", 0600)
	mustGit(t, w.Path, "add", "--", path)
	return path
}

func pinStagedActivity(t *testing.T, w worktree, kind, source string) {
	t.Helper()
	path := stageTempActivity(t, w, kind)
	recent := tempNow.Add(-30 * time.Minute)
	fileTime, indexTime := recent, tempTestTime
	if source == "index" {
		fileTime, indexTime = tempTestTime, recent
	}
	if path != "" {
		setMtime(t, path, fileTime)
	}
	backdateActivityIndex(t, w.Path, indexTime)
}

func assertStagedSurvives(t *testing.T, kind, source string) {
	t.Helper()
	repo, w, session := idleTempCheckout(t)
	before := mustReadTempStore(t)
	pinStagedActivity(t, w, kind, source)
	res, _ := runIdleCleanup(t, repo, w)
	assertTempStore(t, before)
	if len(res.Skipped) != 1 || !exists(w.Path) || !exists(session) || !refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatalf("staged activity lost resources: %+v", res)
	}
	if discovered(t, repo, w.Path).unused(tempNow, time.Hour) {
		t.Fatal("status missed staged activity")
	}
}

func TestGCProtectsStagedActivity(t *testing.T) {
	for _, kind := range []string{"modify", "new", "delete"} {
		for _, source := range []string{"index", "file"} {
			if kind == "delete" && source == "file" {
				continue
			}
			t.Run(kind+"/"+source, func(t *testing.T) { assertStagedSurvives(t, kind, source) })
		}
	}
}

func inspectIdleRepeatedly(t *testing.T, repo string, w worktree) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if !discovered(t, repo, w.Path).unused(tempNow, time.Hour) {
			t.Fatal("status restarted idle window")
		}
		if _, err := verifyTempIdle(mustReadTempStore(t), w, tempNow); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivityInspectionDoesNotRefreshIndex(t *testing.T) {
	repo, w, _ := idleTempCheckout(t)
	path := stageTempActivity(t, w, "modify")
	setMtime(t, path, tempTestTime)
	index := backdateActivityIndex(t, w.Path, tempTestTime)
	inspectIdleRepeatedly(t, repo, w)
	info, err := os.Stat(index)
	if err != nil || !info.ModTime().Equal(tempTestTime) {
		t.Fatalf("inspection changed index: %v, %v", info, err)
	}
	res, _ := runIdleCleanup(t, repo, w)
	if len(res.Removed) != 1 || exists(w.Path) {
		t.Fatalf("abandoned staged checkout survived: %+v", res)
	}
}
