package main

import (
	"encoding/json"
	"errors"
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

// addTarget is the derived sibling directory for a branch: the project prefix
// is the main worktree's directory name (initRepo names it "repo").
func addTarget(repo, branch string) string {
	return canonical(filepath.Join(filepath.Dir(repo), "repo-"+slugify(branch)))
}

// TestIndexSeedSkipsWithoutChunkHound pins ChunkHound's optionality: a binary
// missing from PATH skips indexing instead of failing, and warns only when the
// project actually has a ChunkHound workspace worth refreshing.
func TestIndexSeedSkipsWithoutChunkHound(t *testing.T) {
	sandbox(t)
	t.Setenv("PATH", t.TempDir()) // real LookPath; no chunkhound anywhere

	t.Run("silent without a workspace", func(t *testing.T) {
		var indexed bool
		var err error
		errOut := captureStderr(t, func() { indexed, err = indexSeed(t.TempDir(), false) })
		if err != nil || indexed {
			t.Fatalf("indexSeed = (%v, %v), want (false, nil)", indexed, err)
		}
		if errOut != "" {
			t.Fatalf("absence without a workspace must be silent, got %q", errOut)
		}
	})

	t.Run("warns when a workspace would have been indexed", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, filepath.Join(dir, chunkhoundConfigFile), `{"llm":{"provider":"openai"}}`, 0o644)
		var indexed bool
		var err error
		errOut := captureStderr(t, func() { indexed, err = indexSeed(dir, false) })
		if err != nil || indexed {
			t.Fatalf("indexSeed = (%v, %v), want (false, nil)", indexed, err)
		}
		if !strings.Contains(errOut, "chunkhound not on PATH") {
			t.Fatalf("workspace without a binary must warn, got %q", errOut)
		}
	})

	t.Run("--no-index stays silent even with a workspace", func(t *testing.T) {
		dir := t.TempDir()
		writeFile(t, chunkhoundDBPath(dir), "DB", 0o644)
		var indexed bool
		var err error
		errOut := captureStderr(t, func() { indexed, err = indexSeed(dir, true) })
		if err != nil || indexed || errOut != "" {
			t.Fatalf("noIndex = (%v, %v, %q), want (false, nil, \"\")", indexed, err, errOut)
		}
	})
}

// TestWarnIfChunkHoundExpected pins the warn routing directly: silence
// without a workspace, a warning when a workspace goes unindexed.
func TestWarnIfChunkHoundExpected(t *testing.T) {
	if out := captureStderr(t, func() { warnIfChunkHoundExpected(t.TempDir()) }); out != "" {
		t.Fatalf("no workspace must stay silent, got %q", out)
	}
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, chunkhoundConfigFile), `{}`, 0o644)
	if out := captureStderr(t, func() { warnIfChunkHoundExpected(dir) }); !strings.Contains(out, "chunkhound not on PATH") {
		t.Fatalf("workspace must warn, got %q", out)
	}
}

func TestAddDefaults(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := addTarget(repo, "feature-x")
	res := addResultOf(t, &globals{root: repo}, "feature-x", "--no-index")
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

func TestAddNormalizesBranch(t *testing.T) {
	sandbox(t)
	cases := []struct{ in, branch string }{
		{"Feature Login", "feature-login"},
		{"feature/Login", "feature/login"},
		{"fix__double_underscore", "fix-double-underscore"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			repo := addTestRepo(t)
			target := addTarget(repo, tc.branch)
			res := addResultOf(t, &globals{root: repo}, tc.in, "--no-index")
			if res.Branch != tc.branch || res.Path != target {
				t.Fatalf("normalize %q = branch %q path %q, want %q %q", tc.in, res.Branch, res.Path, tc.branch, target)
			}
			if !refExists(repo, "refs/heads/"+tc.branch) {
				t.Fatalf("branch %q not created", tc.branch)
			}
		})
	}
}

// TestNormalizeBranch pins the branch contract: taxonomy slashes survive,
// messy separators collapse, and invalid ref shapes are rejected.
func TestNormalizeBranch(t *testing.T) {
	valid := map[string]string{
		"feature/login-fix":   "feature/login-fix",
		"Feature Login Fix":   "feature-login-fix",
		"fix__double":         "fix-double",
		"release/1.2.0":       "release/1.2.0",
		"feature/@scope/auth": "feature/@scope/auth",
	}
	for in, want := range valid {
		got, err := normalizeBranch(in)
		if err != nil || got != want {
			t.Fatalf("normalizeBranch(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	invalid := []string{"", "@", "feature/", "feature//x", "feature/a.lock", "..", "feature@{x}", "feat@{upstream}", strings.Repeat("a", maxBranchLen+1)}
	for _, in := range invalid {
		if got, err := normalizeBranch(in); err == nil {
			t.Fatalf("normalizeBranch(%q) = %q, want error", in, got)
		}
	}
}

func TestAddStartPointPositional(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	mustGit(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, filepath.Join(repo, "topic.txt"), "topic", 0o644)
	gitCommit(t, repo, "topic commit", "topic.txt")
	mustGit(t, repo, "checkout", "-q", "main")
	target := addTarget(repo, "from-topic")
	res := addResultOf(t, &globals{root: repo}, "from-topic", "refs/heads/topic", "--no-index")
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
	target := addTarget(repo, "wt-from")
	res := addResultOf(t, &globals{root: repo}, "wt-from", "--from", src, "--no-index")
	if res.Source != src {
		t.Fatalf("source = %q, want --from worktree %q", res.Source, src)
	}
	if b, _ := os.ReadFile(filepath.Join(target, chunkhoundConfigFile)); string(b) != `{"src":true}` {
		t.Fatalf("harness not seeded from --from source: %q", b)
	}
}

// TestAddSeedsFromStartPointWorktree pins the seed-source contract: the harness
// — gitignored db/pi/mcp state — comes from the worktree at the start-point, so
// the copied index matches the new checkout and indexing is a no-op. The
// start-point branch must win over the main worktree even when both share a
// commit (the branch is created from main).
func TestAddSeedsFromStartPointWorktree(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	base := canonical(filepath.Join(filepath.Dir(repo), "wt-base"))
	gitWorktreeAdd(t, repo, base, "base-branch", "refs/heads/main")
	writeFile(t, filepath.Join(base, chunkhoundConfigFile), `{"from":"base"}`, 0o644)

	target := addTarget(repo, "wt-child")
	res := addResultOf(t, &globals{root: repo}, "wt-child", "refs/heads/base-branch", "--no-index")
	if res.Source != base {
		t.Fatalf("source = %q, want start-point worktree %q", res.Source, base)
	}
	if b, _ := os.ReadFile(filepath.Join(target, chunkhoundConfigFile)); string(b) != `{"from":"base"}` {
		t.Fatalf("harness not seeded from start-point worktree: %q", b)
	}
}

// TestAddStartPointWithoutWorktreeWarns pins the honest fallback: a start-point
// checked out nowhere has no faithful index, so the main worktree is used and
// the mismatch is disclosed rather than silently copied.
func TestAddStartPointWithoutWorktreeWarns(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	mustGit(t, repo, "checkout", "-q", "-b", "topic")
	writeFile(t, filepath.Join(repo, "topic.txt"), "topic", 0o644)
	gitCommit(t, repo, "topic commit", "topic.txt")
	mustGit(t, repo, "checkout", "-q", "main")

	var res addResult
	errOut := captureStderr(t, func() {
		res = addResultOf(t, &globals{root: repo}, "wt-fallback", "refs/heads/topic", "--no-index")
	})
	if res.Source != canonical(repo) {
		t.Fatalf("source = %q, want main worktree fallback %q", res.Source, canonical(repo))
	}
	if !strings.Contains(errOut, "no worktree checked out at refs/heads/topic") {
		t.Fatalf("fallback must be disclosed, got stderr %q", errOut)
	}
}

func TestAddFromMissing(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	missing := filepath.Join(filepath.Dir(repo), "missing-src")
	target := addTarget(repo, "wt-missing")
	err := addWorktree(&globals{root: repo}, "wt-missing", addOptions{from: missing, noIndex: true})
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
	target := addTarget(repo, "wt-notrack")
	res := addResultOf(t, &globals{root: repo}, "wt-notrack", "--no-index")
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
	err := addWorktree(&globals{root: repo}, "taken", addOptions{path: target, noIndex: true})
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

// TestAddPathOverride pins the explicit-directory escape hatch.
func TestAddPathOverride(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "custom-dir"))
	res := addResultOf(t, &globals{root: repo}, "feature/override", "--path", target, "--no-index")
	if res.Path != target || res.Branch != "feature/override" {
		t.Fatalf("path/branch = (%q,%q), want (%q,feature/override)", res.Path, res.Branch, target)
	}
}

// TestAddRefusesNestedPath pins the nesting guard: a worktree inside another
// breaks IDE watchers, linters and test runners, so it is rejected.
func TestAddRefusesNestedPath(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	nested := filepath.Join(repo, "nested")
	err := addWorktree(&globals{root: repo}, "nested", addOptions{path: nested, noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to create a worktree inside") {
		t.Fatalf("nested add = %v, want refusal", err)
	}
	if exists(nested) {
		t.Fatal("nested worktree must not be created")
	}
}

func TestAddBranchCollisionFails(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	gitTestBranch(t, repo, "existing-branch")
	err := addWorktree(&globals{root: repo}, "existing-branch", addOptions{noIndex: true})
	if err == nil {
		t.Fatal("add with an existing local branch must fail")
	}
}

// TestAddMissingBranchUsage pins that the branch is required.
func TestAddMissingBranchUsage(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	err := cmdAdd(&globals{root: repo}, nil)
	var ue usageError
	if !errors.As(err, &ue) {
		t.Fatalf("missing branch = %v, want usageError", err)
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
	target := addTarget(repo, "wt-badguard")
	err := addWorktree(&globals{root: repo}, "wt-badguard", addOptions{noIndex: true})
	if err == nil {
		t.Fatal("malformed root guard must surface an error")
	}
	if !strings.Contains(err.Error(), "partial worktree kept") {
		t.Fatalf("error = %v, want the partial-worktree-kept contract", err)
	}
	if !exists(target) {
		t.Fatalf("dirty partial worktree must be kept: %s", target)
	}
	if !refExists(repo, "refs/heads/wt-badguard") {
		t.Fatal("branch must remain after a kept partial worktree")
	}
}

func TestAddHumanOutputAnnotatesOrigin(t *testing.T) {
	sandbox(t)
	t.Run("local base", func(t *testing.T) {
		repo := addTestRepo(t)
		var err error
		out := captureStdout(t, func() { err = cmdAdd(&globals{root: repo}, []string{"wt-local", "--no-index"}) })
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
		var err error
		out := captureStdout(t, func() { err = cmdAdd(&globals{root: repo}, []string{"wt-remote", "--no-index"}) })
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		if !strings.Contains(out, "refs/remotes/origin/main (remote)") {
			t.Fatalf("human output must annotate remote base: %q", out)
		}
		res := addResultOf(t, &globals{root: repo}, "wt-remote-json", "--no-index")
		if res.Base != "refs/remotes/origin/main" {
			t.Fatalf("--json must keep the raw base ref, got %q", res.Base)
		}
	})
}

// TestAddDerivedTargetRefusesNesting pins that the derived sibling path runs the
// same nesting guard as --path: with the current worktree itself nested,
// locationBase would otherwise place the new sibling inside another worktree.
func TestAddDerivedTargetRefusesNesting(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	outer := canonical(filepath.Join(filepath.Dir(repo), "outer"))
	gitWorktreeAdd(t, repo, outer, "outer", "refs/heads/main")
	nested := canonical(filepath.Join(outer, "nested"))
	gitWorktreeAdd(t, repo, nested, "nested", "refs/heads/main")
	t.Chdir(nested)

	wts, err := worktrees(repo)
	if err != nil {
		t.Fatal(err)
	}
	_, err = resolveAddTarget(repo, wts, "feature/x", "")
	if !isUsage(err) || !strings.Contains(err.Error(), "refusing to create a worktree inside") {
		t.Fatalf("derived target under a nested worktree = %v, want nesting refusal", err)
	}
}
