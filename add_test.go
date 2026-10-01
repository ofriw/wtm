package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// add_test.go — Tier-1 contracts for add.go. Every fixture passes --no-index
// (or addOptions.noIndex) so runIndex never shells out to chunkhound.

func addTestRepo(t *testing.T) string {
	t.Helper()
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	return repo
}

// addResultOf drives cmdAdd with --json and decodes the machine result.
func addResultOf(t *testing.T, g *globals, args ...string) addResult {
	t.Helper()
	g.json = true
	var err error
	out := captureStdout(t, func() { err = cmdAdd(g, args) })
	if err != nil {
		t.Fatalf("cmdAdd(%v): %v", args, err)
	}
	var res addResult
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("add JSON %q: %v", out, err)
	}
	return res
}

func TestAddDefaults(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "feature-x"))
	res := addResultOf(t, &globals{root: repo}, target, "--no-index")
	if res.Path != target || res.Branch != "feature-x" {
		t.Fatalf("path/branch = (%q,%q), want (%q,feature-x)", res.Path, res.Branch, target)
	}
	if res.Base != "refs/heads/main" {
		t.Fatalf("base = %q, want refs/heads/main", res.Base)
	}
	if res.Source != canonical(repo) {
		t.Fatalf("source = %q, want %q", res.Source, canonical(repo))
	}
	if res.Indexed {
		t.Fatal("--no-index must report Indexed=false")
	}
	if !exists(filepath.Join(target, ".git")) || !refExists(repo, "refs/heads/feature-x") {
		t.Fatal("worktree checkout and branch must both exist")
	}
}

func TestAddBranchAliases(t *testing.T) {
	sandbox(t)
	for _, flag := range []string{"-b", "--branch"} {
		t.Run(flag, func(t *testing.T) {
			repo := addTestRepo(t)
			target := canonical(filepath.Join(filepath.Dir(repo), "wt"+strings.TrimLeft(flag, "-")))
			res := addResultOf(t, &globals{root: repo}, target, flag, "aliased", "--no-index")
			if res.Branch != "aliased" || !refExists(repo, "refs/heads/aliased") {
				t.Fatalf("%s did not set branch aliased: %+v", flag, res)
			}
		})
	}
}

func TestAddStartPointPositional(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	mustGit(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, filepath.Join(repo, "topic.txt"), "topic", 0o644)
	gitCommit(t, repo, "topic commit", "topic.txt")
	mustGit(t, repo, "checkout", "-q", "main")
	target := canonical(filepath.Join(filepath.Dir(repo), "wt-topic"))
	res := addResultOf(t, &globals{root: repo}, target, "refs/heads/topic", "--no-index")
	if res.Base != "refs/heads/topic" {
		t.Fatalf("base = %q, want refs/heads/topic", res.Base)
	}
	if got, want := mustGit(t, target, "rev-parse", "HEAD"), mustGit(t, repo, "rev-parse", "refs/heads/topic"); got != want {
		t.Fatalf("start-point not honored: HEAD=%s want %s", got, want)
	}
}

func TestAddFromSource(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	src := canonical(filepath.Join(filepath.Dir(repo), "src"))
	gitWorktreeAdd(t, repo, src, "src-branch", "refs/heads/main")
	writeFile(t, filepath.Join(src, chunkhoundConfigFile), `{"src":true}`, 0o644)
	target := canonical(filepath.Join(filepath.Dir(repo), "wt-from"))
	res := addResultOf(t, &globals{root: repo}, target, "--from", src, "--no-index")
	if res.Source != src {
		t.Fatalf("source = %q, want --from worktree %q", res.Source, src)
	}
	if b, _ := os.ReadFile(filepath.Join(target, chunkhoundConfigFile)); string(b) != `{"src":true}` {
		t.Fatalf("harness not seeded from --from source: %q", b)
	}
}

func TestAddFromMissing(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	missing := filepath.Join(filepath.Dir(repo), "missing-src")
	target := filepath.Join(filepath.Dir(repo), "wt-missing")
	err := addWorktree(&globals{root: repo}, target, addOptions{from: missing, noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("add --from missing = %v, want not-found error", err)
	}
	if exists(target) {
		t.Fatalf("missing --from must not create %s", target)
	}
}

func TestAddNoTrack(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "wt-notrack"))
	res := addResultOf(t, &globals{root: repo}, target, "--no-index")
	if _, err := git(target, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Fatal("--no-track must leave no upstream")
	}
	if _, err := git(target, "config", "--get", "branch."+res.Branch+".remote"); err == nil {
		t.Fatal("--no-track must leave no branch remote config")
	}
}

func TestAddExistingPathFails(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "taken"))
	writeFile(t, filepath.Join(target, "keep.txt"), "keep", 0o644)
	err := addWorktree(&globals{root: repo}, target, addOptions{noIndex: true})
	if err == nil {
		t.Fatal("add into an existing non-empty path must fail")
	}
	if !exists(filepath.Join(target, "keep.txt")) {
		t.Fatal("existing target content must survive")
	}
	if wts, err := worktrees(repo); err != nil || len(wts) != 1 {
		t.Fatalf("failed add must leave only the main worktree, got %v (err %v)", wts, err)
	}
	// WHY: git creates the branch ref BEFORE validating the path, so a failed
	// add leaves a dangling branch that add.go does not roll back. Pin reality.
	if !refExists(repo, "refs/heads/taken") {
		t.Fatal("git is expected to create the branch before failing on the path")
	}
}

// TestSeedBadRootGuardKeepsPartialWorktree pins the observed contract: a
// malformed root guard is reached AFTER copyChunkHound, so the checkout is
// dirty and add.go deliberately keeps it. (The approved plan described a clean
// rollback; the code is the SSOT and the clean path is unreachable here.)
func TestSeedBadRootGuardKeepsPartialWorktree(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	writeFile(t, filepath.Join(chunkhoundDBDir(repo), rootGuardName), "{not json", 0o644)
	target := canonical(filepath.Join(filepath.Dir(repo), "wt-badguard"))
	err := addWorktree(&globals{root: repo}, target, addOptions{noIndex: true})
	if err == nil {
		t.Fatal("malformed root guard must surface an error")
	}
	if !strings.Contains(err.Error(), "partial worktree kept") {
		t.Fatalf("error = %v, want the partial-worktree-kept contract", err)
	}
	if !exists(target) {
		t.Fatalf("dirty partial worktree must be kept: %s", target)
	}
	if !refExists(repo, "refs/heads/"+filepath.Base(target)) {
		t.Fatal("branch must remain after a kept partial worktree")
	}
}

func TestAddHumanOutputAnnotatesOrigin(t *testing.T) {
	sandbox(t)
	t.Run("local base", func(t *testing.T) {
		repo := addTestRepo(t)
		target := canonical(filepath.Join(filepath.Dir(repo), "wt-local"))
		var err error
		out := captureStdout(t, func() { err = cmdAdd(&globals{root: repo}, []string{target, "--no-index"}) })
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		if !strings.Contains(out, "refs/heads/main (local)") {
			t.Fatalf("human output must annotate local base: %q", out)
		}
	})
	t.Run("remote base", func(t *testing.T) {
		repo := addTestRepo(t)
		gitTestRemoteHead(t, repo, "origin", "main")
		target := canonical(filepath.Join(filepath.Dir(repo), "wt-remote"))
		var err error
		out := captureStdout(t, func() { err = cmdAdd(&globals{root: repo}, []string{target, "--no-index"}) })
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		if !strings.Contains(out, "refs/remotes/origin/main (remote)") {
			t.Fatalf("human output must annotate remote base: %q", out)
		}
		jsonTarget := canonical(filepath.Join(filepath.Dir(repo), "wt-remote-json"))
		res := addResultOf(t, &globals{root: repo}, jsonTarget, "--no-index")
		if res.Base != "refs/remotes/origin/main" {
			t.Fatalf("--json must keep the raw base ref, got %q", res.Base)
		}
	})
}
