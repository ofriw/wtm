package main

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func sharedIdleTemps(t *testing.T) (string, string, []worktree, []string) {
	t.Helper()
	sandbox(t)
	t.Setenv("GIT_COMMITTER_DATE", tempTestTime.Format(time.RFC3339))
	t.Setenv("GIT_AUTHOR_DATE", tempTestTime.Format(time.RFC3339))
	repo, first, origin := gcFixtureWithUpstream(t)
	second := canonical(filepath.Join(filepath.Dir(repo), "linked2"))
	gitWorktreeAdd(t, repo, second, "feat2", "refs/heads/main")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", "feat2")
	var selected []worktree
	var sessions []string
	for i, path := range []string{first, second} {
		w, session := seedSharedIdleTemp(t, repo, path, strconv.Itoa(i))
		selected = append(selected, w)
		sessions = append(sessions, session)
	}
	return repo, origin, selected, sessions
}

func seedSharedIdleTemp(t *testing.T, repo, path, sessionName string) (worktree, string) {
	t.Helper()
	seedTempRecord(t, path, discovered(t, repo, path).Branch, "1h", tempTestTime)
	session := mkSession(t, path, sessionName, tempTestTime)
	backdateGitActivity(t, path, tempTestTime)
	w := discovered(t, repo, path)
	if !w.Temp || !w.unused(tempNow, time.Hour) {
		t.Fatalf("fixture must be an idle temp: %+v", w)
	}
	return w, session
}

func TestGCTempSharedUpstreamSurvivesActivityAfterSelection(t *testing.T) {
	for _, activeFirst := range []bool{false, true} {
		t.Run("activeFirst="+strconv.FormatBool(activeFirst), func(t *testing.T) {
			testSharedTempActivity(t, activeFirst)
		})
	}
}

func testSharedTempActivity(t *testing.T, activeFirst bool) {
	t.Helper()
	repo, origin, selected, sessions := sharedIdleTemps(t)
	active, idle := selected[0], selected[1]
	plan := planRemotes(selected, nil, false, repo)
	if plan.count() != 1 {
		t.Fatalf("shared upstream must initially be a delete target: %+v", plan)
	}
	want := mustReadTempStore(t)
	delete(want, idle.Path)
	fresh := mkSession(t, active.Path, "fresh", tempNow)
	if !activeFirst {
		selected[0], selected[1] = selected[1], selected[0]
	}
	res, warnings := cleanupSharedIdleTemps(t, repo, selected, plan)
	assertTempStore(t, want)
	assertSharedActiveSurvives(t, repo, origin, active, []string{sessions[0], fresh}, warnings)
	assertSharedIdleRemoved(t, repo, idle, sessions[1], res)
}

func cleanupSharedIdleTemps(t *testing.T, repo string, selected []worktree, plan remotePlan) (gcResult, string) {
	t.Helper()
	var res gcResult
	warnings := captureStderr(t, func() {
		err := withProgress(&globals{json: true}, true, func(p *progress) error {
			var err error
			res, err = gcCleanup(repo, selected, plan, false, gcMode{deleteTemp: true, idleOnly: true}, tempNow, p)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	})
	return res, warnings
}

func assertSharedActiveSurvives(t *testing.T, repo, origin string, active worktree, sessions []string, warnings string) {
	t.Helper()
	if !exists(active.Path) || !refExists(repo, "refs/heads/"+active.Branch) {
		t.Fatal("ACTIVE checkout or local branch was removed")
	}
	for _, session := range sessions {
		if !exists(session) {
			t.Fatalf("ACTIVE Pi session was removed: %s", session)
		}
	}
	if !remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("shared remote branch was deleted")
	}
	if !strings.Contains(warnings, "ACTIVE") || !strings.Contains(warnings, "still tracked by "+active.Path) {
		t.Fatalf("missing ACTIVE or shared-upstream skip reason: %s", warnings)
	}
}

func assertSharedIdleRemoved(t *testing.T, repo string, idle worktree, session string, res gcResult) {
	t.Helper()
	assertTempCleanupSucceeded(t, repo, idle, res)
	if exists(session) {
		t.Fatal("removed idle checkout retained its Pi session")
	}
	if len(res.Removed) != 1 || res.Removed[0] != idle.Path || res.SessionsPurged != 1 {
		t.Fatalf("idle cleanup report: %+v", res)
	}
	if len(res.RemoteDeleted) != 0 || len(res.Failed) != 0 || res.KeptRemote || res.KeptSessions {
		t.Fatalf("ACTIVE skip reported remote deletion, failure, or keep flags: %+v", res)
	}
}
