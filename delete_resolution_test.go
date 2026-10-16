package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func resolutionBranches(t *testing.T, checkout bool) string {
	t.Helper()
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	for i, branch := range []string{"review_x", "review-x", "tmp/review-x"} {
		if checkout {
			path := filepath.Join(filepath.Dir(repo), fmt.Sprintf("linked-%d", i))
			gitWorktreeAdd(t, repo, path, branch, "refs/heads/main")
		} else {
			gitTestBranch(t, repo, branch)
		}
	}
	return repo
}

func assertResolvedDelete(t *testing.T, checkout bool, input, want string) {
	t.Helper()
	repo := resolutionBranches(t, checkout)
	res := deleteResultOf(t, &globals{root: repo, yes: true}, input)
	if res.DeletedBranch != want {
		t.Fatalf("delete %q resolved %q, want %q", input, res.DeletedBranch, want)
	}
	for _, branch := range []string{"review_x", "review-x", "tmp/review-x"} {
		if refExists(repo, "refs/heads/"+branch) != (branch != want) {
			t.Fatalf("delete %q changed the wrong branch: %s", input, branch)
		}
	}
}

func TestDeleteBranchResolutionPriority(t *testing.T) {
	for _, checkout := range []bool{true, false} {
		name := "orphan"
		if checkout {
			name = "checkout"
		}
		t.Run(name, func(t *testing.T) {
			t.Run("exact", func(t *testing.T) { assertResolvedDelete(t, checkout, "review_x", "review_x") })
			t.Run("normalized", func(t *testing.T) { assertResolvedDelete(t, checkout, "Review X", "review-x") })
		})
	}
}

func assertOrphanPrecedence(t *testing.T, input, want string) {
	t.Helper()
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := filepath.Join(filepath.Dir(repo), "linked")
	gitWorktreeAdd(t, repo, linked, "tmp/review-x", "main")
	gitTestBranch(t, repo, "review-x")
	gitTestBranch(t, repo, "review_x")
	res := deleteResultOf(t, &globals{root: repo, yes: true}, input)
	if res.DeletedBranch != want || !exists(linked) || !refExists(repo, "refs/heads/tmp/review-x") {
		t.Fatalf("orphan lost precedence over checkout alias: %+v", res)
	}
}

func TestDeleteOrphanWinsOverCheckoutAlias(t *testing.T) {
	t.Run("exact", func(t *testing.T) { assertOrphanPrecedence(t, "review_x", "review_x") })
	t.Run("normalized", func(t *testing.T) { assertOrphanPrecedence(t, "Review X", "review-x") })
}

func failTempDelete(t *testing.T, repo, linked, origin string) {
	t.Helper()
	mustGit(t, linked, "branch", "-m", "tmp/review")
	mustGit(t, origin, "config", "receive.denyDeletes", "true")
	if err := cmdDelete(&globals{root: repo, yes: true}, []string{"review"}); err == nil {
		t.Fatal("remote rejection must fail deletion")
	}
	if exists(linked) || !refExists(repo, "refs/heads/tmp/review") {
		t.Fatal("failed deletion must remove checkout but preserve recovery branch")
	}
}

func TestDeleteBareTempNameRecoversRemoteFailure(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	failTempDelete(t, repo, linked, origin)
	mustGit(t, origin, "config", "receive.denyDeletes", "false")
	res := deleteResultOf(t, &globals{root: repo, yes: true}, "review")
	if res.DeletedBranch != "tmp/review" || refExists(repo, "refs/heads/tmp/review") {
		t.Fatalf("bare-name retry did not delete recovery branch: %+v", res)
	}
	if remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("retry must delete the retained remote upstream")
	}
}
