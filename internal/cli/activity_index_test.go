package cli

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
)

func stageWorktreeActivity(t *testing.T, w core.Worktree, kind string) string {
	t.Helper()
	path := filepath.Join(w.Path, "tracked.txt")
	if kind == "new" {
		path = filepath.Join(w.Path, "new.txt")
	}
	if kind == "delete" {
		MustGit(t, w.Path, "rm", "tracked.txt")
		return ""
	}
	WriteFile(t, path, "staged activity", 0600)
	MustGit(t, w.Path, "add", "--", path)
	return path
}

func pinStagedActivity(t *testing.T, w core.Worktree, kind, source string) {
	t.Helper()
	path := stageWorktreeActivity(t, w, kind)
	recent := TempNow.Add(-30 * time.Minute)
	fileTime, indexTime := recent, TempTestTime
	if source == "index" {
		fileTime, indexTime = TempTestTime, recent
	}
	if path != "" {
		SetMtime(t, path, fileTime)
	}
	BackdateActivityIndex(t, w.Path, indexTime)
}

func assertStagedSurvives(t *testing.T, kind, source string) {
	t.Helper()
	repo, w, session := idleTempCheckout(t)
	before := mustReadTempStore(t)
	idx := cleanupSessionIndex(t, repo)
	pinStagedActivity(t, w, kind, source)
	res, _ := runIdleCleanup(t, repo, w, idx)
	assertTempStore(t, before)
	if len(res.Skipped) != 1 || !fs.Exists(w.Path) || !fs.Exists(session) || !git.RefExists(repo, "refs/heads/"+w.Branch) {
		t.Fatalf("staged activity lost resources: %+v", res)
	}
	if discovered(t, repo, w.Path).Unused(TempNow, time.Hour) {
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

func inspectIdleRepeatedly(t *testing.T, repo string, w core.Worktree) {
	t.Helper()
	for i := 0; i < 3; i++ {
		if !discovered(t, repo, w.Path).Unused(TempNow, time.Hour) {
			t.Fatal("status restarted idle window")
		}
		if _, err := temp.VerifyTempIdle(mustReadTempStore(t), w.Path, TempNow, harness.FreshWorktreeActivity); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivityInspectionDoesNotRefreshIndex(t *testing.T) {
	repo, w, _ := idleTempCheckout(t)
	path := stageWorktreeActivity(t, w, "modify")
	SetMtime(t, path, TempTestTime)
	index := BackdateActivityIndex(t, w.Path, TempTestTime)
	inspectIdleRepeatedly(t, repo, w)
	info, err := os.Stat(index)
	if err != nil || !info.ModTime().Equal(TempTestTime) {
		t.Fatalf("inspection changed index: %v, %v", info, err)
	}
	res, _ := runIdleCleanup(t, repo, w, cleanupSessionIndex(t, repo))
	if len(res.Removed) != 1 || fs.Exists(w.Path) {
		t.Fatalf("abandoned staged checkout survived: %+v", res)
	}
}
