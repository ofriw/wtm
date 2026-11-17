package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"wtm/internal/fs"
	"wtm/internal/git"
	. "wtm/internal/testutil"
)

func resolutionBranches(t *testing.T, checkout bool) string {
	t.Helper()
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	for i, branch := range []string{"review_x", "review-x", "tmp/review-x"} {
		if checkout {
			path := filepath.Join(filepath.Dir(repo), fmt.Sprintf("linked-%d", i))
			GitWorktreeAdd(t, repo, path, branch, "refs/heads/main")
		} else {
			GitTestBranch(t, repo, branch)
		}
	}
	return repo
}

func assertResolvedDelete(t *testing.T, checkout bool, input, want string) {
	t.Helper()
	repo := resolutionBranches(t, checkout)
	res := deleteResultOf(t, &globals{Root: repo, Yes: true}, input)
	if res.DeletedBranch != want {
		t.Fatalf("delete %q resolved %q, want %q", input, res.DeletedBranch, want)
	}
	for _, branch := range []string{"review_x", "review-x", "tmp/review-x"} {
		if git.RefExists(repo, "refs/heads/"+branch) != (branch != want) {
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
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	linked := filepath.Join(filepath.Dir(repo), "linked")
	GitWorktreeAdd(t, repo, linked, "tmp/review-x", "main")
	GitTestBranch(t, repo, "review-x")
	GitTestBranch(t, repo, "review_x")
	res := deleteResultOf(t, &globals{Root: repo, Yes: true}, input)
	if res.DeletedBranch != want || !fs.Exists(linked) || !git.RefExists(repo, "refs/heads/tmp/review-x") {
		t.Fatalf("orphan lost precedence over checkout alias: %+v", res)
	}
}

func TestDeleteOrphanWinsOverCheckoutAlias(t *testing.T) {
	t.Run("exact", func(t *testing.T) { assertOrphanPrecedence(t, "review_x", "review_x") })
	t.Run("normalized", func(t *testing.T) { assertOrphanPrecedence(t, "Review X", "review-x") })
}

func failTempDelete(t *testing.T, repo, linked, origin string) {
	t.Helper()
	MustGit(t, linked, "branch", "-m", "tmp/review")
	MustGit(t, origin, "config", "receive.denyDeletes", "true")
	if err := cmdDelete(&globals{Root: repo, Yes: true}, []string{"review"}); err == nil {
		t.Fatal("remote rejection must fail deletion")
	}
	if fs.Exists(linked) || !git.RefExists(repo, "refs/heads/tmp/review") {
		t.Fatal("failed deletion must remove checkout but preserve recovery branch")
	}
}

func TestDeleteBareTempNameRecoversRemoteFailure(t *testing.T) {
	Sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	failTempDelete(t, repo, linked, origin)
	MustGit(t, origin, "config", "receive.denyDeletes", "false")
	res := deleteResultOf(t, &globals{Root: repo, Yes: true}, "review")
	if res.DeletedBranch != "tmp/review" || git.RefExists(repo, "refs/heads/tmp/review") {
		t.Fatalf("bare-name retry did not delete recovery branch: %+v", res)
	}
	if RemoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("retry must delete the retained remote upstream")
	}
}

// TestAddDeleteRoundTripKeepsStoredSpelling pins that delete resolves the exact
// stored branch add attached, even when the request differs only by case.
func TestAddDeleteRoundTripKeepsStoredSpelling(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	GitTestBranch(t, repo, "Feature/under_score")
	mainTip := MustGit(t, repo, "rev-parse", "refs/heads/main")

	res := addResultOf(t, &globals{Root: repo}, "feature/under_score", "--no-index")
	if !res.Attached || res.Branch != "Feature/under_score" {
		t.Fatalf("attach = %+v, want Feature/under_score", res)
	}
	del := deleteResultOf(t, &globals{Root: repo, Yes: true}, "feature/under_score")
	if del.DeletedBranch != "Feature/under_score" {
		t.Fatalf("delete resolved %q, want Feature/under_score", del.DeletedBranch)
	}
	if fs.Exists(res.Path) || git.RefExists(repo, "refs/heads/Feature/under_score") {
		t.Fatal("round trip left the target checkout or branch")
	}
	if got := MustGit(t, repo, "rev-parse", "refs/heads/main"); got != mainTip {
		t.Fatalf("main moved from %s to %s", mainTip, got)
	}
}

// TestBranchOnlyDeleteRefusesLiveCheckout pins the last-resort guard: even when
// the ordinary worktree lookup is forced to miss, branch-only cleanup must not
// delete a ref a live checkout holds.
func TestBranchOnlyDeleteRefusesLiveCheckout(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	linked := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked"))
	GitWorktreeAdd(t, repo, linked, "Feature/under_score", "refs/heads/main")

	err := deleteOrphanFallback(&globals{Root: repo, Yes: true}, deleteOptions{branch: "feature/under_score"}, errNoWorktreeForBranch)
	if err == nil || !strings.Contains(err.Error(), "refusing branch-only delete") {
		t.Fatalf("fallback = %v, want a branch-only refusal", err)
	}
	if !fs.Exists(linked) || !git.RefExists(repo, "refs/heads/Feature/under_score") {
		t.Fatal("branch-only refusal damaged the live checkout or branch")
	}
}

// assertPromoteRefBesideAlias pins ref-first refusal and the alias hint for a
// live temporary branch stored with the given spelling.
func assertPromoteRefBesideAlias(t *testing.T, alias string) {
	t.Helper()
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	linked := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked"))
	GitWorktreeAdd(t, repo, linked, alias, "refs/heads/main")
	GitTestBranch(t, repo, "foo")
	err := cmdPromote(&globals{Root: repo}, []string{"foo"})
	if !errors.Is(err, errNoWorktreeForBranch) {
		t.Fatalf("promote foo = %v, want a missing-checkout refusal", err)
	}
	for _, want := range []string{"\"foo\"", fmt.Sprintf("%q", alias), linked, "wtm promote " + alias} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("promote foo error %q must name %q", err, want)
		}
	}
	if !git.RefExists(repo, "refs/heads/foo") || !git.RefExists(repo, "refs/heads/"+alias) || !fs.Exists(linked) {
		t.Fatal("refused promote must change nothing")
	}
	CaptureStdout(t, func() {
		if err := cmdPromote(&globals{Root: repo}, []string{alias}); err != nil {
			t.Fatalf("promote %s: %v", alias, err)
		}
	})
}

// TestPromoteRefBesideLiveTempAlias pins the ref-first choice: a bare ref with
// no checkout wins, so promote refuses instead of retargeting the live alias.
func TestPromoteRefBesideLiveTempAlias(t *testing.T) {
	assertPromoteRefBesideAlias(t, "tmp/foo")
}

// TestPromoteRefBesideMixedCaseAlias pins the alias hint to Git's stored
// spelling: the refusal names the live tmp/Foo checkout even when the bare ref
// is lowercase.
func TestPromoteRefBesideMixedCaseAlias(t *testing.T) {
	assertPromoteRefBesideAlias(t, "tmp/Foo")
}

// TestPromoteResolvesStoredSpelling pins that promote addresses the same stored
// branch add attaches, not the caller's case-different spelling.
func TestPromoteResolvesStoredSpelling(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	linked := CanonicalPath(filepath.Join(filepath.Dir(repo), "linked"))
	GitWorktreeAdd(t, repo, linked, "Feature/under_score", "refs/heads/main")

	out := CaptureStdout(t, func() {
		if err := cmdPromote(&globals{Root: repo, JSON: true}, []string{"feature/under_score"}); err != nil {
			t.Fatal(err)
		}
	})
	var res promoteResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatal(err)
	}
	if res.Path != linked || res.Branch != "Feature/under_score" || res.Promoted {
		t.Fatalf("promote = %+v, want the stored permanent branch", res)
	}
}
