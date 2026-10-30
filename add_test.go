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
// — gitignored db/agent/mcp state — comes from the worktree at the start-point, so
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

// attachRepo returns a repo and tip for feature/attach: one commit ahead of
// main, checked out nowhere, with a configured upstream.
func attachRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := addTestRepo(t)
	mustGit(t, repo, "checkout", "-q", "-b", "feature/attach")
	writeFile(t, filepath.Join(repo, "attach.txt"), "attach\n", 0o644)
	gitCommit(t, repo, "attach commit", "attach.txt")
	mustGit(t, repo, "checkout", "-q", "main")
	mustGit(t, repo, "config", "branch.feature/attach.remote", "origin")
	mustGit(t, repo, "config", "branch.feature/attach.merge", "refs/heads/feature/attach")
	return repo, mustGit(t, repo, "rev-parse", "refs/heads/feature/attach")
}

// TestAddAttachesExistingBranch pins the contract: adopt the branch, never move it, seed the harness.
func TestAddAttachesExistingBranch(t *testing.T) {
	sandbox(t)
	repo, tip := attachRepo(t)
	writeFile(t, filepath.Join(repo, ".mcp.json"), `{"mcpServers":{}}`, 0o644)
	target := addTarget(repo, "feature/attach")

	res := addResultOf(t, &globals{root: repo}, "feature/attach", "--no-index")
	if res.Branch != "feature/attach" || res.Path != target {
		t.Fatalf("attach = %+v, want feature/attach at %s", res, target)
	}
	if res.Base != "refs/heads/feature/attach" {
		t.Fatalf("base = %q, want the branch ref", res.Base)
	}
	if got := mustGit(t, target, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("checkout HEAD = %s, want branch tip %s", got, tip)
	}
	if got := mustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved to %s, want %s", got, tip)
	}
	if got := mustGit(t, target, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/attach" {
		t.Fatalf("checkout is not on the existing branch: %q", got)
	}
	if !exists(filepath.Join(target, ".mcp.json")) {
		t.Fatal("attach must seed the agent harness like a new branch")
	}
	if !exists(filepath.Join(target, "attach.txt")) {
		t.Fatal("attach must check out the branch tree")
	}
	// Attachment must write no branch config: the upstream survives because
	// worktree add runs without -b/--track.
	if got, err := git(repo, "config", "--get", "branch.feature/attach.remote"); err != nil || got != "origin" {
		t.Fatalf("upstream remote = (%q,%v), want origin", got, err)
	}
	if got, err := git(repo, "config", "--get", "branch.feature/attach.merge"); err != nil || got != "refs/heads/feature/attach" {
		t.Fatalf("upstream merge = (%q,%v), want refs/heads/feature/attach", got, err)
	}
}

// TestAddAttachRejectsCheckedOutBranch pins that an occupied branch names its worktree and writes nothing.
func TestAddAttachRejectsCheckedOutBranch(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	busy := canonical(filepath.Join(filepath.Dir(repo), "busy"))
	gitWorktreeAdd(t, repo, busy, "feature/busy", "refs/heads/main")

	err := addWorktree(&globals{root: repo}, "feature/busy", addOptions{noIndex: true})
	if err == nil || isUsage(err) || !strings.Contains(err.Error(), "already checked out") || !strings.Contains(err.Error(), busy) {
		t.Fatalf("add on a checked-out branch = %v, want a non-usage refusal naming %s", err, busy)
	}
	if err := addWorktree(&globals{root: repo}, "main", addOptions{noIndex: true}); err == nil || isUsage(err) || !strings.Contains(err.Error(), "at the main worktree") {
		t.Fatalf("add on the main worktree's branch = %v, want non-usage main-worktree guidance", err)
	}
	if exists(addTarget(repo, "feature/busy")) {
		t.Fatal("rejected attach must not create a checkout")
	}
}

// TestAddAttachStartPoint pins the start-point contract: the tip attaches; a
// different commit or unresolvable ref is refused without writes.
func TestAddAttachStartPoint(t *testing.T) {
	sandbox(t)
	cases := []struct {
		name    string
		start   string // "tip" resolves to the branch commit
		refusal bool
		usage   bool
		want    string
	}{
		{"tip ref", "refs/heads/feature/attach", false, false, ""},
		{"tip sha", "tip", false, false, ""},
		{"mismatch", "refs/heads/main", true, false, "does not match existing branch"},
		{"invalid", "no-such-ref", true, true, "is not a commit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo, tip := attachRepo(t)
			start := tc.start
			if start == "tip" {
				start = tip
			}
			err := addWorktree(&globals{root: repo}, "feature/attach", addOptions{start: start, noIndex: true})
			if !tc.refusal {
				if err != nil {
					t.Fatalf("attach via %s: %v", start, err)
				}
				target := addTarget(repo, "feature/attach")
				if got := mustGit(t, target, "rev-parse", "HEAD"); got != tip {
					t.Fatalf("attach via %s HEAD = %s, want %s", start, got, tip)
				}
				if got := mustGit(t, target, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/attach" {
					t.Fatalf("attach via %s checked out %q, want the existing branch", start, got)
				}
				return
			}
			if err == nil || isUsage(err) != tc.usage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("start-point %q = %v, want usage=%v naming %q", start, err, tc.usage, tc.want)
			}
			if got := mustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
				t.Fatalf("branch moved to %s, want %s", got, tip)
			}
			if exists(addTarget(repo, "feature/attach")) {
				t.Fatal("rejected attach must not create a checkout")
			}
		})
	}
}

// TestAddAttachRejectsOccupiedPath pins that an occupied target keeps its content and the branch stays put.
func TestAddAttachRejectsOccupiedPath(t *testing.T) {
	sandbox(t)
	repo, tip := attachRepo(t)
	target := canonical(filepath.Join(filepath.Dir(repo), "taken-attach"))
	writeFile(t, filepath.Join(target, "keep.txt"), "keep", 0o644)

	if err := addWorktree(&globals{root: repo}, "feature/attach", addOptions{path: target, noIndex: true}); err == nil {
		t.Fatal("attach into an occupied path must fail")
	}
	if !exists(filepath.Join(target, "keep.txt")) {
		t.Fatal("occupied path content must survive")
	}
	if got := mustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved to %s, want %s", got, tip)
	}
	if wts, err := worktrees(repo); err != nil || len(wts) != 1 {
		t.Fatalf("failed attach must leave only the main worktree, got %v (err %v)", wts, err)
	}
}

// TestAddAttachFailedSeedKeepsBranch pins that a seed failure never deletes an adopted branch.
func TestAddAttachFailedSeedKeepsBranch(t *testing.T) {
	sandbox(t)
	repo, tip := attachRepo(t)
	writeFile(t, filepath.Join(chunkhoundDBDir(repo), rootGuardName), "{not json", 0o644)

	err := addWorktree(&globals{root: repo}, "feature/attach", addOptions{noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "existing branch was left untouched") {
		t.Fatalf("seed failure = %v, want the branch-untouched note", err)
	}
	if got := mustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved or was deleted: %s, want %s", got, tip)
	}
}

// TestRollbackAddBranchOwnership pins the rollback boundary: wtm deletes only the branches it created.
func TestRollbackAddBranchOwnership(t *testing.T) {
	sandbox(t)
	cases := []struct {
		name     string
		attached bool
	}{
		{"attached branch survives", true},
		{"created branch removed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := addTestRepo(t)
			path := canonical(filepath.Join(filepath.Dir(repo), "wt"))
			gitWorktreeAdd(t, repo, path, "feature/keep", "refs/heads/main")
			err := rollbackAdd(repo, path, "feature/keep", tc.attached, errors.New("seed failed"))
			if err == nil || exists(path) {
				t.Fatalf("rollback = %v, want the seed error with a removed checkout", err)
			}
			if kept := refExists(repo, "refs/heads/feature/keep"); kept != tc.attached {
				t.Fatalf("branch survived = %v, want %v", kept, tc.attached)
			}
			if tc.attached && !strings.Contains(err.Error(), "left untouched") {
				t.Fatalf("rollback = %v, want the branch-untouched note", err)
			}
		})
	}
}

// TestAddAttachKeepsStoredSpelling pins request-first adoption: any spelling
// attaches git's stored branch, never a duplicate. Covers names outside the
// creation alphabet and a same-named tag that degrades %(refname:short).
func TestAddAttachKeepsStoredSpelling(t *testing.T) {
	cases := []struct {
		branch, request string
		tag             bool
	}{
		{"Feature/X", "Feature/X", false},
		{"Feature/X", "feature/x", false},
		{"Feature/under_score", "feature/under_score", false},
		{"Feature/C++", "Feature/C++", false},
		{"Feature/tagged", "Feature/tagged", true},
	}
	for _, tc := range cases {
		t.Run(tc.request, func(t *testing.T) {
			sandbox(t)
			repo := addTestRepo(t)
			gitTestBranch(t, repo, tc.branch)
			if tc.tag {
				mustGit(t, repo, "tag", tc.branch)
			}
			target := addTarget(repo, tc.branch)

			var res addResult
			if warn := captureStderr(t, func() { res = addResultOf(t, &globals{root: repo}, tc.request, "--no-index") }); warn != "" {
				t.Fatalf("attaching %q must not warn, got %q", tc.branch, warn)
			}
			if res.Branch != tc.branch || res.Path != target {
				t.Fatalf("attach %q = %+v, want %s at %s", tc.request, res, tc.branch, target)
			}
			if got := mustGit(t, res.Path, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/"+tc.branch {
				t.Fatalf("checkout branch = %q, want refs/heads/%s", got, tc.branch)
			}
		})
	}
}

// TestMatchLocalBranch pins exact-first, then a unique case fold; several case variants are ambiguous.
func TestMatchLocalBranch(t *testing.T) {
	refs := map[string]bool{"Feature/X": true, "main": true}
	if got, err := matchLocalBranch(refs, "Feature/X"); err != nil || got.name != "Feature/X" || !got.exists {
		t.Fatalf("exact = (%+v,%v), want Feature/X", got, err)
	}
	if got, err := matchLocalBranch(refs, "feature/x"); err != nil || got.name != "Feature/X" || !got.exists {
		t.Fatalf("case-fold = (%+v,%v), want Feature/X", got, err)
	}
	if got, err := matchLocalBranch(refs, "fix/x"); err != nil || got.name != "fix/x" || got.exists {
		t.Fatalf("missing = (%+v,%v), want an absent fix/x", got, err)
	}
	twoVariants := map[string]bool{"Feature/X": true, "feature/x": true}
	if _, err := matchLocalBranch(twoVariants, "FEATURE/X"); err == nil {
		t.Fatal("ambiguous case variants must be rejected")
	}
	if got, err := matchLocalBranch(twoVariants, "Feature/X"); err != nil || got.name != "Feature/X" {
		t.Fatalf("exact spelling must beat ambiguity: (%+v,%v)", got, err)
	}
}

// TestAddTempRefusesExistingBranch pins that --temp never adopts a branch: a
// pre-existing tmp/ branch would otherwise fall under gc's reclaim rules.
func TestAddTempRefusesExistingBranch(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	gitTestBranch(t, repo, "tmp/keep")
	target := addTarget(repo, "tmp/keep")

	for _, args := range [][]string{
		{"tmp/keep", "--temp", "--no-index"},
		{"tmp/keep", "--ttl", "30m", "--no-index"},
	} {
		err := cmdAdd(&globals{root: repo}, args)
		if err == nil || isUsage(err) || !strings.Contains(err.Error(), "refusing --temp for existing branch") {
			t.Fatalf("cmdAdd(%v) = %v, want a non-usage temp refusal", args, err)
		}
		if exists(target) {
			t.Fatalf("refused temp add created %s", target)
		}
	}
}

// TestRequestedBranch pins raw exact, raw case-fold, then normalization; --temp uses only its tmp/ identity.
func TestRequestedBranch(t *testing.T) {
	refs := map[string]bool{"Feature/X": true, "Feature/under_score": true, "Feature/C++": true}
	cases := []struct {
		raw, want string
		temp      bool
		exists    bool
	}{
		{"Feature/X", "Feature/X", false, true},
		{"feature/x", "Feature/X", false, true},
		{"feature/under_score", "Feature/under_score", false, true},
		{"feature/c++", "Feature/C++", false, true},
		{"Feature Login", "feature-login", false, false},
		{"x", "tmp/x", true, false},
		{"TMP/X", "tmp/x", true, false},
	}
	for _, tc := range cases {
		got, err := requestedBranch(refs, tc.raw, tc.temp)
		if err != nil || got.name != tc.want || got.exists != tc.exists {
			t.Fatalf("requestedBranch(%q,%v) = (%+v,%v), want %q exists=%v", tc.raw, tc.temp, got, err, tc.want, tc.exists)
		}
	}
	if _, err := requestedBranch(nil, "", false); err == nil {
		t.Fatal("empty branch must be rejected")
	}
	if _, err := requestedBranch(nil, "feature/@{x}", false); err == nil {
		t.Fatal("invalid ref must be rejected")
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
	t.Run("origin base wins over earlier-sorting stale remote", func(t *testing.T) {
		repo := addTestRepo(t)                       // commit A on main
		gitTestRemoteHead(t, repo, "fork-5", "main") // fork-5/main -> A (stale)
		writeFile(t, filepath.Join(repo, "next.txt"), "next\n", 0o644)
		gitCommit(t, repo, "next", "next.txt") // commit B on main
		gitTestRemoteHead(t, repo, "origin", "main")
		res := addResultOf(t, &globals{root: repo}, "wt-remote-order", "--no-index")
		if res.Base != "refs/remotes/origin/main" {
			t.Fatalf("base = %q, want refs/remotes/origin/main", res.Base)
		}
		want := mustGit(t, repo, "rev-parse", "refs/remotes/origin/main")
		if got := mustGit(t, repo, "rev-parse", "wt-remote-order"); got != want {
			t.Fatalf("branch tip = %s, want %s (origin/main)", got, want)
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

// TestAddTTLImpliesTemp pins that --ttl alone opts into a temp worktree: a TTL
// without --temp would otherwise be silently ignored.
func TestAddTTLImpliesTemp(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	res := addResultOf(t, &globals{root: repo}, "wt-ttl", "--no-index", "--ttl", "30m")
	if !res.Temp || res.Branch != "tmp/wt-ttl" || res.TTL != "30m0s" {
		t.Fatalf("--ttl add = %+v, want tmp/wt-ttl with 30m", res)
	}
	if res.ExpiresAt == nil {
		t.Fatal("--ttl implies temp and must report an expiry")
	}
}

// TestTempBranchPrefix pins the tmp/ namespace and its idempotence: an
// already-temp name must not double-prefix.
func TestTempBranchPrefix(t *testing.T) {
	cases := map[string]string{
		"x":         "tmp/x",
		"tmp/x":     "tmp/x",
		"Tmp/X":     "tmp/x",
		"tmp/tmp/x": "tmp/tmp/x",
	}
	for in, want := range cases {
		got, err := tempBranch(in)
		if err != nil || got != want {
			t.Fatalf("tempBranch(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
}

func TestAddTempBranchIdempotent(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	res := addResultOf(t, &globals{root: repo}, "tmp/already", "--temp", "--no-index")
	if res.Branch != "tmp/already" {
		t.Fatalf("branch = %q, want tmp/already (no double prefix)", res.Branch)
	}
}

// TestAddTempWritesRecord pins the store write: a successful temp add records
// the canonical checkout path and the default window, last, after seeding.
func TestAddTempWritesRecord(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	target := addTarget(repo, "tmp/wt-rec")
	res := addResultOf(t, &globals{root: repo}, "wt-rec", "--temp", "--no-index")
	if !res.Temp || res.Path != target {
		t.Fatalf("add = %+v, want temp at %s", res, target)
	}
	rec, ok := mustReadTempStore(t)[target]
	if !ok {
		t.Fatalf("no temp record for %s", target)
	}
	if rec.Branch != "tmp/wt-rec" || rec.TTL != defaultTempTTL.String() {
		t.Fatalf("record = %+v, want tmp/wt-rec with %s", rec, defaultTempTTL)
	}
	if rec.CreatedAt.IsZero() {
		t.Fatal("record createdAt must be set")
	}
}

// TestAddTempFailedSeedLeavesNoRecord pins the write-last ordering: a seed that
// fails must leave a durable (permanent) checkout, never a tracked temp.
func TestAddTempFailedSeedLeavesNoRecord(t *testing.T) {
	sandbox(t)
	repo := addTestRepo(t)
	writeFile(t, filepath.Join(chunkhoundDBDir(repo), rootGuardName), "{not json", 0o644)
	target := addTarget(repo, "tmp/wt-badtemp")
	err := addWorktree(&globals{root: repo}, "wt-badtemp", addOptions{temp: true, ttl: "30m", noIndex: true})
	if err == nil {
		t.Fatal("malformed root guard must fail the temp add")
	}
	if _, ok := mustReadTempStore(t)[target]; ok {
		t.Fatalf("failed seed must not record a temp for %s", target)
	}
}
