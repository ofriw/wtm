package main

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// delete_test.go — Tier-1 contracts for delete.go: branch-addressed reclamation
// that runs the gc removal path regardless of the unused TTL, then removes the
// local branch the command names. Fixtures drive real git only.

func deleteResultOf(t *testing.T, g *globals, args ...string) gcResult {
	t.Helper()
	g.json = true
	var err error
	out := captureStdout(t, func() { err = cmdDelete(g, args) })
	if err != nil {
		t.Fatalf("cmdDelete(%v): %v", args, err)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("delete JSON %q: %v", out, err)
	}
	return res
}

// TestDeleteActiveWorktree pins the unconditional contract: an ACTIVE worktree
// (recent activity) is removed, its local branch deleted, sessions purged.
func TestDeleteActiveWorktree(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feature/x", "refs/heads/main")
	session := mkSession(t, linked, "linked-s", tempNow) // fresh → ACTIVE

	res := deleteResultOf(t, &globals{root: repo, yes: true}, "feature/x")
	if res.DeletedBranch != "feature/x" {
		t.Fatalf("DeletedBranch = %q, want feature/x", res.DeletedBranch)
	}
	if res.SessionsPurged != 1 {
		t.Fatalf("SessionsPurged = %d, want 1", res.SessionsPurged)
	}
	if exists(linked) {
		t.Fatalf("worktree survived delete: %s", linked)
	}
	if refExists(repo, "refs/heads/feature/x") {
		t.Fatal("local branch survived delete")
	}
	if exists(session) {
		t.Fatal("sessions must be purged by default")
	}
}

func TestDeleteKeepsSessions(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feat", "refs/heads/main")
	session := mkSession(t, linked, "linked-s", tempNow)

	res := deleteResultOf(t, &globals{root: repo, yes: true}, "feat", "--keep-sessions")
	if !res.KeptSessions {
		t.Fatal("--keep-sessions must report the keep")
	}
	if !exists(session) {
		t.Fatal("--keep-sessions must preserve history")
	}
	if refExists(repo, "refs/heads/feat") {
		t.Fatal("delete still removes the local branch")
	}
}

// TestDeleteRemote pins that delete removes the remote upstream by default and
// --keep-remote preserves it, on a real local bare origin.
func TestDeleteRemote(t *testing.T) {
	t.Run("default deletes remote", func(t *testing.T) {
		sandbox(t)
		repo, _, origin := gcFixtureWithUpstream(t)
		res := deleteResultOf(t, &globals{root: repo, yes: true}, "feat")
		if len(res.RemoteDeleted) != 1 || res.RemoteDeleted[0] != "origin/feat" {
			t.Fatalf("RemoteDeleted = %v, want [origin/feat]", res.RemoteDeleted)
		}
		if remoteHasBranch(t, repo, origin, "feat") {
			t.Fatal("remote branch survived delete")
		}
	})
	t.Run("keep-remote preserves remote", func(t *testing.T) {
		sandbox(t)
		repo, _, origin := gcFixtureWithUpstream(t)
		res := deleteResultOf(t, &globals{root: repo, yes: true}, "feat", "--keep-remote")
		if !res.KeptRemote || len(res.RemoteDeleted) != 0 {
			t.Fatalf("KeptRemote=%v RemoteDeleted=%v, want true/[]", res.KeptRemote, res.RemoteDeleted)
		}
		if !remoteHasBranch(t, repo, origin, "feat") {
			t.Fatal("--keep-remote must preserve the remote branch")
		}
	})
}

func TestDeleteRefusesMain(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	err := cmdDelete(&globals{root: repo, yes: true}, []string{"main"})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete the main worktree") {
		t.Fatalf("delete main = %v, want refusal", err)
	}
}

func TestDeleteUnknownBranch(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	err := cmdDelete(&globals{root: repo, yes: true}, []string{"no-such-branch"})
	if err == nil || !strings.Contains(err.Error(), "no worktree has branch") {
		t.Fatalf("delete unknown = %v, want not-found", err)
	}
}

// TestDeleteRequiresYesNonInteractive pins the consent contract: a destructive
// delete offline cannot prompt, so it demands --yes.
func TestDeleteRequiresYesNonInteractive(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feature/x", "refs/heads/main")
	err := cmdDelete(&globals{root: repo}, []string{"feature/x"})
	var ue usageError
	if !errors.As(err, &ue) {
		t.Fatalf("non-TTY delete = %v, want usageError", err)
	}
}

func TestDeleteRefusesDefaultBranchLinkedWorktree(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	gitTestBranch(t, repo, "other")
	mustGit(t, repo, "checkout", "-q", "other")
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked-main"))
	mustGit(t, repo, "worktree", "add", linked, "main")
	err := cmdDelete(&globals{root: repo, yes: true}, []string{"main"})
	if err == nil || !strings.Contains(err.Error(), "refusing to delete default branch") {
		t.Fatalf("delete linked default branch = %v, want refusal", err)
	}
	if !exists(linked) {
		t.Fatal("linked worktree must survive")
	}
	if !refExists(repo, "refs/heads/main") {
		t.Fatal("default branch must survive")
	}
}

func TestDeleteDisclosesKeptBranchOnRemoteFailure(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, origin, "config", "receive.denyDeletes", "true")
	var err error
	errOut := captureStderr(t, func() {
		err = cmdDelete(&globals{root: repo, yes: true}, []string{"feat"})
	})
	if err == nil {
		t.Fatal("delete must fail when remote deletion fails")
	}
	if exists(linked) {
		t.Fatal("worktree must be removed")
	}
	if !refExists(repo, "refs/heads/feat") {
		t.Fatal("local branch must be kept when remote fails")
	}
	if !strings.Contains(errOut, "kept local branch feat") {
		t.Fatalf("stderr %q must disclose kept local branch", errOut)
	}
}

func TestDeleteNormalizesInput(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "feature-x", "refs/heads/main")
	res := deleteResultOf(t, &globals{root: repo, yes: true}, "Feature X")
	if res.DeletedBranch != "feature-x" {
		t.Fatalf("DeletedBranch = %q, want feature-x", res.DeletedBranch)
	}
	if exists(linked) {
		t.Fatal("worktree must be deleted")
	}
}

func TestDeleteMatchesTempPrefix(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := canonical(filepath.Join(filepath.Dir(repo), "linked"))
	gitWorktreeAdd(t, repo, linked, "tmp/review", "refs/heads/main")
	res := deleteResultOf(t, &globals{root: repo, yes: true}, "review")
	if res.DeletedBranch != "tmp/review" {
		t.Fatalf("DeletedBranch = %q, want tmp/review", res.DeletedBranch)
	}
	if exists(linked) || refExists(repo, "refs/heads/tmp/review") {
		t.Fatal("temp checkout or branch survived delete")
	}
}

func TestDeletePromptTitle(t *testing.T) {
	w := worktree{Path: "/path/to/wt", Branch: "feat"}
	if got := deletePromptTitle(w, 0); got != "Delete worktree /path/to/wt, local branch feat?" {
		t.Fatalf("deletePromptTitle(0) = %q", got)
	}
	if got := deletePromptTitle(w, 2); got != "Delete worktree /path/to/wt, local branch feat, and 2 remote branch(es)?" {
		t.Fatalf("deletePromptTitle(2) = %q", got)
	}
}

// TestDeleteOrphanRetry pins the branch-only retry: a first delete whose remote
// step fails removes the worktree but keeps the local branch; once the remote
// cause is fixed, the retry reclaims the branch and deletes the upstream.
func TestDeleteOrphanRetry(t *testing.T) {
	sandbox(t)
	repo, linked, origin := gcFixtureWithUpstream(t)
	mustGit(t, origin, "config", "receive.denyDeletes", "true")

	var firstErr error
	errOut := captureStderr(t, func() {
		captureStdout(t, func() {
			firstErr = cmdDelete(&globals{root: repo, yes: true}, []string{"feat"})
		})
	})
	if firstErr == nil {
		t.Fatal("first delete must fail on the rejected remote delete")
	}
	if !strings.Contains(errOut, "kept local branch feat") {
		t.Fatalf("first delete stderr %q must disclose the kept branch", errOut)
	}
	if exists(linked) {
		t.Fatal("worktree must be gone after the first delete")
	}
	if !refExists(repo, "refs/heads/feat") {
		t.Fatal("local branch must survive the failed remote delete")
	}

	mustGit(t, origin, "config", "receive.denyDeletes", "false")
	res := deleteResultOf(t, &globals{root: repo, yes: true}, "feat")
	if res.DeletedBranch != "feat" {
		t.Fatalf("retry DeletedBranch = %q, want feat", res.DeletedBranch)
	}
	if refExists(repo, "refs/heads/feat") {
		t.Fatal("retry must delete the local branch")
	}
	if remoteHasBranch(t, repo, origin, "feat") {
		t.Fatal("retry must delete the remote upstream")
	}
}

// TestDeleteBranchWithoutWorktree pins the branch-only case: a local branch with
// no worktree is removable, and the disclosure says so without the old false
// "already removed" claim.
func TestDeleteBranchWithoutWorktree(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	gitTestBranch(t, repo, "orphan")

	var err error
	errOut := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmdDelete(&globals{root: repo, yes: true}, []string{"orphan"})
		})
	})
	if err != nil {
		t.Fatalf("delete branch without worktree: %v", err)
	}
	if refExists(repo, "refs/heads/orphan") {
		t.Fatal("branch-only delete must remove the local branch")
	}
	if !strings.Contains(errOut, `no worktree for branch "orphan"`) {
		t.Fatalf("stderr %q must disclose the branch-only cleanup", errOut)
	}
	if strings.Contains(errOut, "already removed") {
		t.Fatalf("stderr %q must not claim the worktree was already removed", errOut)
	}
}

// TestDeleteDirtyWarnsOnce pins a single pre-consent disclosure: confirmDelete
// warns, and the removal path must not warn again.
func TestDeleteDirtyWarnsOnce(t *testing.T) {
	sandbox(t)
	repo, linked, _ := gcFixtureWithUpstream(t)
	writeFile(t, filepath.Join(linked, "untracked.txt"), "x", 0o644)

	var err error
	errOut := captureStderr(t, func() {
		captureStdout(t, func() {
			err = cmdDelete(&globals{root: repo, yes: true}, []string{"feat"})
		})
	})
	if err != nil {
		t.Fatalf("delete dirty: %v", err)
	}
	if got := strings.Count(errOut, "has uncommitted changes"); got != 1 {
		t.Fatalf("dirty warnings = %d (%q), want exactly 1", got, errOut)
	}
}

// TestOrphanBranchNameSkipsOptionLikeRaw pins that a leading-hyphen argument is
// never treated as a raw ref name, which git could read as an option.
func TestOrphanBranchNameSkipsOptionLikeRaw(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	if _, ok := orphanBranchName(repo, "-D"); ok {
		t.Fatal("option-like argument must not resolve to a branch")
	}
}
