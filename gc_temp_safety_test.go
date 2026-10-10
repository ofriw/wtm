package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func tempSafetyFixture(t *testing.T) (string, string, worktree) {
	t.Helper()
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "temp-safety"))
	gitWorktreeAdd(t, repo, linked, "tmp/safety", "refs/heads/main")
	seedTempRecord(t, linked, "tmp/safety", "1h", tempNow)
	return repo, linked, discovered(t, repo, linked)
}

func TestGCRejectsChangedTempIdentity(t *testing.T) {
	for _, change := range []string{"branch", "same-branch-recreation"} {
		t.Run(change, func(t *testing.T) {
			repo, linked, w := tempSafetyFixture(t)
			changeTempCheckout(t, repo, linked, change)
			res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{w}, false, true)
			if err != nil || len(res.Removed) != 0 {
				t.Fatalf("changed identity removal: result=%+v error=%v", res, err)
			}
			if len(res.Skipped) != 1 {
				t.Fatalf("%s drift must skip: result=%+v", change, res)
			}
			if !exists(linked) || !refExists(repo, "refs/heads/tmp/safety") {
				t.Fatal("changed checkout or original branch was removed")
			}
		})
	}
}

func changeTempCheckout(t *testing.T, repo, linked, change string) {
	t.Helper()
	if change == "branch" {
		mustGit(t, linked, "checkout", "-b", "permanent")
		return
	}
	mustGit(t, repo, "worktree", "remove", linked)
	mustGit(t, repo, "worktree", "add", linked, "tmp/safety")
}

func TestGCTempBranchDeletionFailure(t *testing.T) {
	repo, linked, w := tempSafetyFixture(t)
	other := filepath.Join(filepath.Dir(repo), "other")
	mustGit(t, repo, "worktree", "add", "--force", other, w.Branch)
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{w}, false, true)
	if err == nil || len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Error, "delete branch tmp/safety") {
		t.Fatalf("branch failure: result=%+v error=%v", res, err)
	}
	if exists(linked) || !exists(other) || !refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatal("branch deletion failure changed the wrong checkout or removed the branch")
	}
	assertRemovedTempRecord(t, linked)
}

func assertRemovedTempRecord(t *testing.T, linked string) {
	t.Helper()
	if _, ok := mustReadTempStore(t)[linked]; ok {
		t.Fatal("removed checkout retained temp registry permission")
	}
}

func TestTempCleanupFailurePreservesLocalBranch(t *testing.T) {
	for _, command := range []string{"gc", "delete"} {
		t.Run(command, func(t *testing.T) {
			sandbox(t)
			repo, linked, origin := gcFixtureWithUpstream(t)
			seedTempRecord(t, linked, "feat", "1h", tempNow)
			mustGit(t, origin, "config", "receive.denyDeletes", "true")
			res := runTempCleanupFailure(t, command, repo, linked)
			if len(res.Removed) != 1 || len(res.Failed) != 1 || res.DeletedBranch != "" || len(res.DeletedBranches) != 0 {
				t.Fatalf("cleanup failure result=%+v", res)
			}
			if exists(linked) || !refExists(repo, "refs/heads/feat") {
				t.Fatal("cleanup failure did not preserve the local recovery branch")
			}
			assertRemovedTempRecord(t, linked)
		})
	}
}

func runTempCleanupFailure(t *testing.T, command, repo, linked string) gcResult {
	t.Helper()
	var runErr error
	out := captureStdout(t, func() {
		g := &globals{root: repo, json: true, yes: true}
		if command == "delete" {
			runErr = cmdDelete(g, []string{"feat"})
		} else {
			runErr = cmdGC(g, []string{"--path", linked})
		}
	})
	var res gcResult
	if runErr == nil || json.Unmarshal([]byte(out), &res) != nil {
		t.Fatalf("expected reported cleanup failure, error=%v output=%s", runErr, out)
	}
	return res
}

func TestGCTempConsentNamesLocalBranches(t *testing.T) {
	selected := []worktree{{Temp: true, Branch: "tmp/one"}, {Branch: "permanent"}, {Temp: true, Branch: "tmp/two"}}
	for _, keep := range []bool{false, true} {
		title := gcConsentTitle(selected, remotePlan{keep: keep})
		if !strings.Contains(title, "force-delete temp LOCAL branches: tmp/one, tmp/two") || strings.Contains(title, "permanent") {
			t.Fatalf("unsafe consent title: %s", title)
		}
		if !strings.Contains(gcPickerPolicy(keep, true), "Temp LOCAL branches will be force-deleted") {
			t.Fatalf("picker does not disclose local branch deletion (keep=%v)", keep)
		}
	}
}

func TestGCTempPromotedBeforeRemovalSkips(t *testing.T) {
	repo, linked, w := tempSafetyFixture(t)
	// Promote the temp record before the locked verification, simulating
	// a concurrent promote between discovery and removal.
	_, err := promoteTempRecord(linked)
	if err != nil {
		t.Fatalf("promote: %v", err)
	}
	res, err := gcRunRemove(t, &globals{json: true}, repo, []worktree{w}, false, true)
	if err != nil {
		t.Fatalf("gc after promote: %v", err)
	}
	if len(res.Skipped) != 1 || res.Skipped[0] != linked {
		t.Fatalf("skipped = %v, want [%s]", res.Skipped, linked)
	}
	if len(res.Failed) != 0 {
		t.Fatalf("failed = %v, want none after promote", res.Failed)
	}
	if !exists(linked) || !refExists(repo, "refs/heads/tmp/safety") {
		t.Fatal("promoted worktree or branch was removed")
	}
}

func TestGCConsentTitleNoTempBranches(t *testing.T) {
	selected := []worktree{{Branch: "feature/a"}, {Branch: "feature/b"}}
	title := gcConsentTitle(selected, remotePlan{})
	if strings.Contains(title, "temp LOCAL") || strings.Contains(title, "force-delete") {
		t.Fatalf("permanent-only title must not mention temp branches: %s", title)
	}
	if !strings.Contains(title, "2 worktree(s)") {
		t.Fatalf("title missing worktree count: %s", title)
	}
}
