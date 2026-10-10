package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func staleCheckout(t *testing.T, change string) (string, string, worktree) {
	t.Helper()
	repo, path, old := identityCheckout(t)
	path = canonical(path)
	switch change {
	case "branch":
		mustGit(t, path, "switch", "-c", "permanent")
	case "detached":
		mustGit(t, path, "checkout", "--detach")
	case "identity":
		token, err := tempIdentityPath(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(token, []byte(fixtureTempIdentity), 0600); err != nil {
			t.Fatal(err)
		}
	case "recreated":
		mustGit(t, repo, "worktree", "remove", path)
		mustGit(t, repo, "worktree", "add", path, old.Branch)
	}
	return repo, path, discovered(t, repo, path)
}

func TestStaleTempExplicitCleanup(t *testing.T) {
	for _, change := range []string{"branch", "identity", "recreated", "detached"} {
		for _, command := range []string{"gc", "delete"} {
			if change == "detached" && command == "delete" {
				continue
			}
			t.Run(change+"/"+command, func(t *testing.T) {
				repo, path, w := staleCheckout(t, change)
				if w.Temp {
					t.Fatal("stale checkout inherited temp rules")
				}
				res := runStaleCleanup(t, command, repo, path, w.Branch)
				if len(res.Removed) != 1 || len(res.Failed) != 0 || exists(path) {
					t.Fatalf("explicit cleanup failed: %+v", res)
				}
				assertRemovedTempRecord(t, path)
				if command == "gc" && !refExists(repo, "refs/heads/tmp/test") {
					t.Fatal("permanent cleanup deleted former temp recovery branch")
				}
			})
		}
	}
}

func runStaleCleanup(t *testing.T, command, repo, path, branch string) gcResult {
	t.Helper()
	var err error
	out := captureStdout(t, func() {
		g := &globals{root: repo, json: true, yes: true}
		if command == "delete" {
			err = cmdDelete(g, []string{branch})
		} else {
			err = cmdGC(g, []string{"--path", path})
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	return res
}

func TestStaleTempPromotion(t *testing.T) {
	for _, change := range []string{"branch", "identity", "recreated", "detached"} {
		t.Run(change, func(t *testing.T) {
			repo, path, w := staleCheckout(t, change)
			if promoted, err := promoteTempRecord(path); promoted || err != nil {
				t.Fatalf("already permanent promotion = %v, %v", promoted, err)
			}
			assertRemovedTempRecord(t, path)
			assertPromotionState(t, path, w.Branch)
			if !refExists(repo, "refs/heads/tmp/test") {
				t.Fatal("promotion deleted former temp branch")
			}
		})
	}
}

func TestPermanentRemovalRejectsLateTempRegistration(t *testing.T) {
	repo, path, old := identityCheckout(t)
	path = canonical(path)
	if _, err := promoteTempRecord(path); err != nil {
		t.Fatal(err)
	}
	w := discovered(t, repo, path)
	seedTempRecord(t, path, old.Branch, "1h", tempTestTime)
	want := mustReadTempStore(t)
	if removed, _, err := removeVerifiedWorktree(repo, w, false, tempNow); removed || err == nil {
		t.Fatalf("late registration removal = %v, %v", removed, err)
	}
	assertTempStore(t, want)
	if !exists(path) {
		t.Fatal("late registered checkout removed")
	}
}

func TestPermanentRemovalReconcilesStaleTarget(t *testing.T) {
	for _, change := range []string{"branch", "identity", "recreated", "detached"} {
		t.Run(change, func(t *testing.T) {
			repo, path, w := staleCheckout(t, change)
			if removed, _, err := removeVerifiedWorktree(repo, w, false, tempNow); !removed || err != nil {
				t.Fatalf("stale target removal = %v, %v", removed, err)
			}
			assertRemovedTempRecord(t, path)
		})
	}
}

func TestStaleTempGitFailurePreservesCheckout(t *testing.T) {
	repo, path, w := staleCheckout(t, "branch")
	want := mustReadTempStore(t)
	if err := os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if branch, err := gitCurrentBranch(path); err == nil {
		t.Fatalf("Git failure classified as branch %q", branch)
	}
	if promoted, err := promoteTempRecord(path); promoted || err == nil {
		t.Fatalf("failed Git promotion = %v, %v", promoted, err)
	}
	if removed, _, err := removeVerifiedWorktree(repo, w, false, tempNow); removed || err == nil {
		t.Fatalf("failed Git removal = %v, %v", removed, err)
	}
	assertTempStore(t, want)
	if !exists(path) {
		t.Fatal("Git failure removed checkout")
	}
}

func TestPermanentRemovalStaleRecordWriteFailure(t *testing.T) {
	repo, path, w := staleCheckout(t, "branch")
	restrictRegistryWrites(t)
	want := mustReadTempStore(t)
	if removed, _, err := removeVerifiedWorktree(repo, w, false, tempNow); removed || err == nil {
		t.Fatalf("registry failure removal = %v, %v", removed, err)
	}
	assertTempStore(t, want)
	if !exists(path) {
		t.Fatal("registry failure removed checkout")
	}
}
