package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func addCleanupTemp(t *testing.T, repo, branch string) worktree {
	t.Helper()
	path := canonical(filepath.Join(filepath.Dir(repo), branch))
	gitWorktreeAdd(t, repo, path, branch, "refs/heads/main")
	seedTempRecord(t, path, branch, "1h", tempNow)
	return discovered(t, repo, path)
}

func runFailedTempGC(t *testing.T, repo string, selected []worktree) (gcResult, string) {
	t.Helper()
	var res gcResult
	warnings := captureStderr(t, func() {
		var err error
		res, err = gcRunRemove(t, &globals{json: true}, repo, selected, false, false)
		if err == nil {
			t.Error("cleanup unexpectedly succeeded")
		}
	})
	return res, warnings
}

func assertTempRecoveryBranch(t *testing.T, repo string, w worktree, warnings string) {
	t.Helper()
	if !refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatalf("recovery branch %s was deleted", w.Branch)
	}
	for _, text := range []string{"kept local branch " + w.Branch, "fix the cause", "wtm gc --path", "wtm delete"} {
		if !strings.Contains(warnings, text) {
			t.Fatalf("missing recovery instruction %q: %s", text, warnings)
		}
	}
}

func assertTempCleanupSucceeded(t *testing.T, repo string, w worktree, res gcResult) {
	t.Helper()
	if exists(w.Path) || refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatalf("successful checkout %s or its branch survived", w.Path)
	}
	if len(res.DeletedBranches) != 1 || res.DeletedBranches[0] != w.Branch {
		t.Fatalf("deleted branches = %v, want %s", res.DeletedBranches, w.Branch)
	}
	assertRemovedTempRecord(t, w.Path)
}

func TestGCTempPruneFailureRetainsRecoveryBranch(t *testing.T) {
	repo, _, w := identityCheckout(t)
	removed, _, err := removeTempWorktreeIf(w, false, tempNow)
	if err != nil || !removed {
		t.Fatalf("remove checkout: removed=%v, error=%v", removed, err)
	}
	config := filepath.Join(repo, ".git", "config")
	original, err := os.ReadFile(config)
	mustProcessError(t, err)
	mustProcessError(t, os.WriteFile(config, []byte("[invalid\n"), 0o600))
	res := gcResult{Removed: []string{w.Path}}
	warnings := captureStderr(t, func() {
		finishSelectedCleanup(repo, []worktree{w}, nil, remotePlan{}, true, gcMode{deleteTemp: true}, &res, nil, nil)
	})
	mustProcessError(t, os.WriteFile(config, original, 0o600))
	assertTempPruneFailure(t, repo, w, res, warnings)
}

// A repo-global prune failure must not veto this checkout's branch purge.
// The branch survives only because its own deletion failed, and gc says so.
func assertTempPruneFailure(t *testing.T, repo string, w worktree, res gcResult, warnings string) {
	t.Helper()
	if res.Pruned {
		t.Fatal("prune failure was not reported")
	}
	if !hasFailurePrefix(res, "prune:") {
		t.Fatalf("prune failure not reported: %+v", res.Failed)
	}
	if len(res.DeletedBranches) != 0 {
		t.Fatalf("branch deleted despite deletion failure: %v", res.DeletedBranches)
	}
	if !refExists(repo, "refs/heads/"+w.Branch) {
		t.Fatalf("recovery branch %s was deleted", w.Branch)
	}
	for _, text := range []string{"kept local branch " + w.Branch, "wtm gc --path", "wtm delete"} {
		if !strings.Contains(warnings, text) {
			t.Fatalf("missing recovery instruction %q: %s", text, warnings)
		}
	}
}

func hasFailurePrefix(res gcResult, prefix string) bool {
	for _, f := range res.Failed {
		if strings.HasPrefix(f.Error, prefix) {
			return true
		}
	}
	return false
}

func TestGCTempRegistryWriteFailureRetainsRecoveryBranch(t *testing.T) {
	repo, _, w := identityCheckout(t)
	path := w.Path
	restrictRegistryWrites(t)
	res, warnings := runFailedTempGC(t, repo, []worktree{w})
	assertTempRecoveryBranch(t, repo, w, warnings)
	if exists(path) || len(res.Removed) != 1 || res.Removed[0] != path {
		t.Fatalf("checkout removal was not reported: %+v", res)
	}
	if len(res.Failed) != 1 || res.Failed[0].Path != path {
		t.Fatalf("registry write failure was not reported: %+v", res)
	}
	if rec := mustReadTempStore(t)[path]; rec.Identity != w.TempIdentity {
		t.Fatal("failed registry write changed record")
	}
}

func TestGCTempMixedCleanupSuccess(t *testing.T) {
	for _, failure := range []string{"removal", "remote"} {
		for _, failedFirst := range []bool{false, true} {
			t.Run(failure+"/failedFirst="+strconv.FormatBool(failedFirst), func(t *testing.T) {
				testGCTempMixedCleanup(t, failure, failedFirst)
			})
		}
	}
}

func testGCTempMixedCleanup(t *testing.T, failure string, failedFirst bool) {
	t.Helper()
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	seedTempRecord(t, linked, "feat", "1h", tempNow)
	failed := discovered(t, repo, linked)
	if failure == "removal" {
		mustGit(t, repo, "worktree", "lock", linked)
	} else {
		mustGit(t, origin, "config", "receive.denyDeletes", "true")
	}
	good := addCleanupTemp(t, repo, "tmp/good")
	selected := []worktree{good, failed}
	if failedFirst {
		selected = []worktree{failed, good}
	}
	res, warnings := runFailedTempGC(t, repo, selected)
	assertMixedTempResult(t, repo, good, failed, failure, res, warnings)
}

func assertMixedTempResult(t *testing.T, repo string, good, failed worktree, failure string, res gcResult, warnings string) {
	t.Helper()
	assertTempCleanupSucceeded(t, repo, good, res)
	assertTempRecoveryBranch(t, repo, failed, warnings)
	if len(res.Failed) != 1 || res.Failed[0].Path != failed.Path {
		t.Fatalf("cleanup failures: %+v", res.Failed)
	}
	if failure == "removal" {
		if !exists(failed.Path) {
			t.Fatal("failed removal lost checkout")
		}
		if _, ok := mustReadTempStore(t)[failed.Path]; !ok {
			t.Fatal("failed removal lost registry permission")
		}
	} else {
		assertRemovedTempRecord(t, failed.Path)
	}
}

func TestGCTempSharedUpstreamFailureRetainsEachBranch(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	seedTempRecord(t, linked, "feat", "1h", tempNow)
	second := addCleanupTemp(t, repo, "tmp/shared")
	mustGit(t, repo, "branch", "--set-upstream-to=origin/feat", second.Branch)
	selected := []worktree{discovered(t, repo, linked), discovered(t, repo, second.Path)}
	mustGit(t, origin, "config", "receive.denyDeletes", "true")
	res, warnings := runFailedTempGC(t, repo, selected)
	assertSharedTempFailure(t, repo, selected, res, warnings)
}

func assertSharedTempFailure(t *testing.T, repo string, selected []worktree, res gcResult, warnings string) {
	t.Helper()
	if len(res.Removed) != 2 || len(res.Failed) != 2 || len(res.DeletedBranches) != 0 {
		t.Fatalf("shared failure result: %+v", res)
	}
	if count := strings.Count(warnings, "deleting remote origin/feat"); count != 1 {
		t.Fatalf("remote delete attempted %d times: %s", count, warnings)
	}
	for i, w := range selected {
		assertTempRecoveryBranch(t, repo, w, warnings)
		assertRemovedTempRecord(t, w.Path)
		if exists(w.Path) || res.Failed[i].Path != w.Path || !strings.Contains(res.Failed[i].Error, "delete remote origin/feat") {
			t.Fatalf("checkout %s did not report shared failure: %+v", w.Path, res)
		}
	}
}
