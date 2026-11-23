package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/harness"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
	"wtm/internal/ui"
)

// add_test.go — Tier-1 contracts for add.go. Fixtures that exercise indexing
// install a stub chunkhound on PATH; the rest skip it with --no-index so
// runIndex never reaches a real provider.

func addTestRepo(t *testing.T) string {
	t.Helper()
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	return repo
}

// addResultOf drives cmdAdd with --json and decodes the machine result.
func addResultOf(t *testing.T, g *globals, args ...string) addResult {
	t.Helper()
	g.JSON = true
	var err error
	out := CaptureStdout(t, func() { err = cmdAdd(g, args) })
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
// is the main worktree's directory name (InitRepo names it "repo").
func addTarget(repo, branch string) string {
	return CanonicalPath(filepath.Join(filepath.Dir(repo), "repo-"+core.Slugify(branch)))
}

// TestIndexSeedSkipsWithoutChunkHound pins ChunkHound's optionality: a binary
// missing from PATH skips indexing instead of failing, and warns only when the
// project actually has a ChunkHound workspace worth refreshing.
func TestIndexSeedSkipsWithoutChunkHound(t *testing.T) {
	Sandbox(t)
	t.Setenv("PATH", t.TempDir()) // real LookPath; no chunkhound anywhere

	t.Run("silent without a workspace", func(t *testing.T) {
		var indexed bool
		var err error
		errOut := CaptureStderr(t, func() { indexed, err = indexSeed(t.TempDir(), false) })
		if err != nil || indexed {
			t.Fatalf("indexSeed = (%v, %v), want (false, nil)", indexed, err)
		}
		if errOut != "" {
			t.Fatalf("absence without a workspace must be silent, got %q", errOut)
		}
	})

	t.Run("warns when a workspace would have been indexed", func(t *testing.T) {
		dir := t.TempDir()
		WriteFile(t, filepath.Join(dir, harness.ChunkhoundConfigFile), `{"llm":{"provider":"openai"}}`, 0o644)
		var indexed bool
		var err error
		errOut := CaptureStderr(t, func() { indexed, err = indexSeed(dir, false) })
		if err != nil || indexed {
			t.Fatalf("indexSeed = (%v, %v), want (false, nil)", indexed, err)
		}
		if !strings.Contains(errOut, "chunkhound not on PATH") {
			t.Fatalf("workspace without a binary must warn, got %q", errOut)
		}
	})

	t.Run("--no-index stays silent even with a workspace", func(t *testing.T) {
		dir := t.TempDir()
		WriteFile(t, harness.ChunkhoundDBPath(dir), "DB", 0o644)
		var indexed bool
		var err error
		errOut := CaptureStderr(t, func() { indexed, err = indexSeed(dir, true) })
		if err != nil || indexed || errOut != "" {
			t.Fatalf("noIndex = (%v, %v, %q), want (false, nil, \"\")", indexed, err, errOut)
		}
	})
}

// TestWarnIfChunkHoundExpected pins the warn routing directly: silence
// without a workspace, a warning when a workspace goes unindexed.
func TestWarnIfChunkHoundExpected(t *testing.T) {
	if out := CaptureStderr(t, func() { warnIfChunkHoundExpected(t.TempDir()) }); out != "" {
		t.Fatalf("no workspace must stay silent, got %q", out)
	}
	dir := t.TempDir()
	WriteFile(t, filepath.Join(dir, harness.ChunkhoundConfigFile), `{}`, 0o644)
	if out := CaptureStderr(t, func() { warnIfChunkHoundExpected(dir) }); !strings.Contains(out, "chunkhound not on PATH") {
		t.Fatalf("workspace must warn, got %q", out)
	}
}

func TestAddDefaults(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	target := addTarget(repo, "feature-x")
	res := addResultOf(t, &globals{Root: repo}, "feature-x", "--no-index")
	if res.Path != target || res.Branch != "feature-x" {
		t.Fatalf("path/branch = (%q,%q), want (%q,feature-x)", res.Path, res.Branch, target)
	}
	if res.Base != "refs/heads/main" {
		t.Fatalf("base = %q, want refs/heads/main", res.Base)
	}
	if res.Source != CanonicalPath(repo) {
		t.Fatalf("source = %q, want %q", res.Source, CanonicalPath(repo))
	}
	if res.Indexed {
		t.Fatal("--no-index must report Indexed=false")
	}
	if !fs.Exists(filepath.Join(target, ".git")) || !git.RefExists(repo, "refs/heads/feature-x") {
		t.Fatal("worktree checkout and branch must both exist")
	}
}

func TestAddNormalizesBranch(t *testing.T) {
	Sandbox(t)
	cases := []struct{ in, branch string }{
		{"Feature Login", "feature-login"},
		{"feature/Login", "feature/login"},
		{"fix__double_underscore", "fix-double-underscore"},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			repo := addTestRepo(t)
			target := addTarget(repo, tc.branch)
			res := addResultOf(t, &globals{Root: repo}, tc.in, "--no-index")
			if res.Branch != tc.branch || res.Path != target {
				t.Fatalf("normalize %q = branch %q path %q, want %q %q", tc.in, res.Branch, res.Path, tc.branch, target)
			}
			if !git.RefExists(repo, "refs/heads/"+tc.branch) {
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
		got, err := core.NormalizeBranch(in)
		if err != nil || got != want {
			t.Fatalf("core.NormalizeBranch(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
	invalid := []string{"", "@", "feature/", "feature//x", "feature/a.lock", "..", "feature@{x}", "feat@{upstream}", strings.Repeat("a", core.MaxBranchLen+1)}
	for _, in := range invalid {
		if got, err := core.NormalizeBranch(in); err == nil {
			t.Fatalf("core.NormalizeBranch(%q) = %q, want error", in, got)
		}
	}
}

func TestAddStartPointPositional(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	MustGit(t, repo, "checkout", "-q", "-b", "topic")
	WriteFile(t, filepath.Join(repo, "topic.txt"), "topic", 0o644)
	GitCommit(t, repo, "topic commit", "topic.txt")
	MustGit(t, repo, "checkout", "-q", "main")
	target := addTarget(repo, "from-topic")
	res := addResultOf(t, &globals{Root: repo}, "from-topic", "refs/heads/topic", "--no-index")
	if res.Base != "refs/heads/topic" {
		t.Fatalf("base = %q, want refs/heads/topic", res.Base)
	}
	if got, want := MustGit(t, target, "rev-parse", "HEAD"), MustGit(t, repo, "rev-parse", "refs/heads/topic"); got != want {
		t.Fatalf("start-point not honored: HEAD=%s want %s", got, want)
	}
}

// TestAddStartPointInvalidUsesUsageInBothModes pins one exit contract: an
// unresolved user-supplied start-point is a usage mistake before any write,
// whether the branch is new or already exists.
func TestAddStartPointInvalidUsesUsageInBothModes(t *testing.T) {
	for _, tc := range []struct {
		Name, branch string
		attach       bool
	}{
		{"create", "feature/new", false},
		{"attach", "feature/existing", true},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			Sandbox(t)
			repo := addTestRepo(t)
			if tc.attach {
				GitTestBranch(t, repo, tc.branch)
			}
			err := addWorktree(&globals{Root: repo}, tc.branch, addOptions{start: "no-such-ref", noIndex: true})
			if !isUsage(err) || !strings.Contains(err.Error(), "is not a commit") {
				t.Fatalf("add %s bad start-point = %v, want a usage refusal", tc.Name, err)
			}
			if fs.Exists(addTarget(repo, tc.branch)) || git.RefExists(repo, "refs/heads/"+tc.branch) != tc.attach {
				t.Fatal("rejected start-point must write nothing")
			}
		})
	}
}

func TestAddFromSource(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	src := CanonicalPath(filepath.Join(filepath.Dir(repo), "src"))
	GitWorktreeAdd(t, repo, src, "src-branch", "refs/heads/main")
	WriteFile(t, filepath.Join(src, harness.ChunkhoundConfigFile), `{"src":true}`, 0o644)
	target := addTarget(repo, "wt-from")
	res := addResultOf(t, &globals{Root: repo}, "wt-from", "--from", src, "--no-index")
	if res.Source != src {
		t.Fatalf("source = %q, want --from worktree %q", res.Source, src)
	}
	if b, _ := os.ReadFile(filepath.Join(target, harness.ChunkhoundConfigFile)); string(b) != `{"src":true}` {
		t.Fatalf("harness not seeded from --from source: %q", b)
	}
}

// TestAddSeedsFromStartPointWorktree pins the seed-source contract: the harness
// — gitignored db/agent/mcp state — comes from the worktree at the start-point, so
// the copied index matches the new checkout and indexing is a no-op. The
// start-point branch must win over the main worktree even when both share a
// commit (the branch is created from main).
func TestAddSeedsFromStartPointWorktree(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	base := CanonicalPath(filepath.Join(filepath.Dir(repo), "wt-base"))
	GitWorktreeAdd(t, repo, base, "base-branch", "refs/heads/main")
	WriteFile(t, filepath.Join(base, harness.ChunkhoundConfigFile), `{"from":"base"}`, 0o644)

	target := addTarget(repo, "wt-child")
	res := addResultOf(t, &globals{Root: repo}, "wt-child", "refs/heads/base-branch", "--no-index")
	if res.Source != base {
		t.Fatalf("source = %q, want start-point worktree %q", res.Source, base)
	}
	if b, _ := os.ReadFile(filepath.Join(target, harness.ChunkhoundConfigFile)); string(b) != `{"from":"base"}` {
		t.Fatalf("harness not seeded from start-point worktree: %q", b)
	}
}

// TestAddStartPointWithoutWorktreeWarns pins the honest fallback: a start-point
// checked out nowhere has no faithful index, so the main worktree is used and
// the mismatch is disclosed rather than silently copied.
func TestAddStartPointWithoutWorktreeWarns(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	MustGit(t, repo, "checkout", "-q", "-b", "topic")
	WriteFile(t, filepath.Join(repo, "topic.txt"), "topic", 0o644)
	GitCommit(t, repo, "topic commit", "topic.txt")
	MustGit(t, repo, "checkout", "-q", "main")

	var res addResult
	errOut := CaptureStderr(t, func() {
		res = addResultOf(t, &globals{Root: repo}, "wt-fallback", "refs/heads/topic", "--no-index")
	})
	if res.Source != CanonicalPath(repo) {
		t.Fatalf("source = %q, want main worktree fallback %q", res.Source, CanonicalPath(repo))
	}
	if !strings.Contains(errOut, "no usable worktree for refs/heads/topic") {
		t.Fatalf("fallback must be disclosed, got stderr %q", errOut)
	}
	if strings.Contains(errOut, "re-embed") || strings.Contains(errOut, "cannot match") {
		t.Fatalf("--no-index must not claim re-embedding, got stderr %q", errOut)
	}
}

// TestAddAttachFallbackWarnsWithoutReembed pins the corrected attach wording:
// a branch ref is expected to have no checkout, and --no-index cannot re-embed.
func TestAddAttachFallbackWarnsWithoutReembed(t *testing.T) {
	Sandbox(t)
	repo, _ := attachRepo(t)

	var res addResult
	errOut := CaptureStderr(t, func() {
		res = addResultOf(t, &globals{Root: repo}, "feature/attach", "--no-index")
	})
	if res.Source != CanonicalPath(repo) {
		t.Fatalf("source = %q, want main worktree %q", res.Source, CanonicalPath(repo))
	}
	if !strings.Contains(errOut, "branch refs/heads/feature/attach is not checked out") {
		t.Fatalf("attach fallback must name the branch ref, got %q", errOut)
	}
	if strings.Contains(errOut, "no usable worktree") {
		t.Fatalf("attach must not use the create-path wording, got %q", errOut)
	}
	if strings.Contains(errOut, "re-embed") || strings.Contains(errOut, "cannot match") {
		t.Fatalf("--no-index must not claim re-embedding, got stderr %q", errOut)
	}
}

// stubChunkHound puts a successful indexer first on PATH. The real git and the
// rest of PATH stay available; this is a real process, not an in-process mock.
func stubChunkHound(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a POSIX chunkhound stub")
	}
	dir := t.TempDir()
	WriteFile(t, filepath.Join(dir, "chunkhound"), "#!/bin/sh\nexit 0\n", 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestAddAttachStaleOnlyWhenSkippedWithCopiedIndex pins the new index gate: a
// refreshed index is never claimed stale, and a skipped refresh needs a copied
// database before the warning may claim one.
func TestAddAttachStaleOnlyWhenSkippedWithCopiedIndex(t *testing.T) {
	for _, tc := range []struct {
		Name        string
		noIndex     bool
		withDB      bool
		withDBDir   bool
		wantIndexed bool
		wantStale   bool
	}{
		{"refreshed with copied index", false, true, false, true, false},
		{"skipped with copied index", true, true, false, false, true},
		{"skipped without copied index", true, false, false, false, false},
		{"skipped with empty db dir", true, false, true, false, false},
	} {
		t.Run(tc.Name, func(t *testing.T) {
			Sandbox(t)
			repo, _ := attachRepo(t)
			if tc.withDB {
				WriteFile(t, harness.ChunkhoundDBPath(repo), "DB", 0o644)
			}
			if tc.withDBDir {
				WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), "{}", 0o644)
				if err := os.MkdirAll(harness.ChunkhoundDBDir(repo), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			// --no-index never runs the indexer, so only a refreshed case needs
			// the stub. Other cases prove the flag without any indexer on PATH.
			if !tc.noIndex {
				stubChunkHound(t)
			}

			args := []string{"feature/attach"}
			if tc.noIndex {
				args = append(args, "--no-index")
			}
			var res addResult
			errOut := CaptureStderr(t, func() {
				res = addResultOf(t, &globals{Root: repo}, args...)
			})
			if res.Indexed != tc.wantIndexed {
				t.Fatalf("indexed = %v, want %v", res.Indexed, tc.wantIndexed)
			}
			if !strings.Contains(errOut, "branch refs/heads/feature/attach is not checked out") {
				t.Fatalf("attach fallback must name the branch ref, got %q", errOut)
			}
			if strings.Contains(errOut, "no usable worktree") {
				t.Fatalf("attach must not use the create-path wording, got %q", errOut)
			}
			if tc.noIndex && strings.Contains(errOut, "chunkhound not on PATH") {
				t.Fatalf("--no-index must not consult the indexer, got %q", errOut)
			}
			if got := strings.Contains(errOut, "index is stale"); got != tc.wantStale {
				t.Fatalf("stale claim = %v, want %v; stderr %q", got, tc.wantStale, errOut)
			}
			if strings.Contains(errOut, "cannot match") || strings.Contains(errOut, "re-embed") {
				t.Fatalf("old re-embed wording must be gone, got %q", errOut)
			}
		})
	}
}

// TestAddAttachEqualTreeDoesNotClaimStaleIndex pins the tree gate on attach: an
// empty commit keeps the tree equal, so a copied index is still current even
// when --no-index skipped a refresh.
func TestAddAttachEqualTreeDoesNotClaimStaleIndex(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	tree := MustGit(t, repo, "rev-parse", "HEAD^{tree}")
	empty := MustGit(t, repo, "commit-tree", tree, "-p", "HEAD", "-m", "empty")
	MustGit(t, repo, "update-ref", "refs/heads/feature/equal", empty)
	WriteFile(t, harness.ChunkhoundDBPath(repo), "DB", 0o644)

	var res addResult
	errOut := CaptureStderr(t, func() {
		res = addResultOf(t, &globals{Root: repo}, "feature/equal", "--no-index")
	})
	if !res.Attached {
		t.Fatalf("attach = %+v, want attached", res)
	}
	if !strings.Contains(errOut, "branch refs/heads/feature/equal is not checked out") {
		t.Fatalf("attach fallback must name the branch ref, got %q", errOut)
	}
	if strings.Contains(errOut, "index is stale") {
		t.Fatalf("equal trees must not claim a stale index, got %q", errOut)
	}
}

// TestWarnSeedSourceText pins the one emitter's exact text: create fallback is
// tree-gated, attach fallback keeps its own wording, and only a skipped refresh
// over a differing tree claims staleness.
func TestWarnSeedSourceText(t *testing.T) {
	src := t.TempDir()
	fallback := "no usable worktree for refs/heads/x; seeding from " + CanonicalPath(src)
	attach := "branch refs/heads/x is not checked out; seeding from " + CanonicalPath(src)
	stale := "; the copied ChunkHound index is stale: indexing was skipped"
	cases := []struct {
		Name    string
		source  seedSource
		attach  bool
		noIndex bool
		hadDB   bool
		want    string
	}{
		{"create fallback", seedSource{path: src, fallback: true, note: fallback, differs: true}, false, false, false, fallback},
		{"create fallback equal trees", seedSource{path: src, fallback: true}, false, false, false, ""},
		{"default source", seedSource{path: src, note: "seeding from " + src, differs: true}, false, false, false, "seeding from " + src},
		{"attach fallback", seedSource{path: src, fallback: true}, true, false, false, attach},
		{"attach overrides tree note", seedSource{path: src, fallback: true, note: fallback, differs: true}, true, false, false, attach},
		{"attach equal trees skipped", seedSource{path: src, fallback: true}, true, true, true, attach},
		{"attach differing trees skipped", seedSource{path: src, fallback: true, note: fallback, differs: true}, true, true, true, attach + stale},
		{"create stale", seedSource{path: src, fallback: true, note: fallback, differs: true}, false, true, true, fallback + stale},
		{"refreshed index", seedSource{path: src, fallback: true, note: fallback, differs: true}, false, false, true, fallback},
		{"skipped without copied index", seedSource{path: src, fallback: true, note: fallback, differs: true}, false, true, false, fallback},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			out := CaptureStderr(t, func() {
				warnSeedSource(tc.source, "refs/heads/x", tc.attach, tc.noIndex, tc.hadDB)
			})
			if tc.want == "" {
				if out != "" {
					t.Fatalf("warning = %q, want silence", out)
				}
				return
			}
			if out != ui.WarnPrefix+tc.want+"\n" {
				t.Fatalf("warning = %q, want %q", out, ui.WarnPrefix+tc.want)
			}
		})
	}
}

// TestSeedNoticeRejectsOptionLikeBase pins that an option-like base is treated
// as a revision, never as a rev-parse option.
func TestSeedNoticeRejectsOptionLikeBase(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	for _, base := range []string{"-x", "--all", "--", "--end-of-options"} {
		if note, differs := seedNotice(repo, repo, base, ""); note != "" || differs {
			t.Fatalf("seedNotice(%q) = %q, differs %v, want silence", base, note, differs)
		}
	}
}

// TestResolveStartPointRejectsOptionLikeValue pins the same terminator on the
// user-supplied start-point path.
func TestResolveStartPointRejectsOptionLikeValue(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	for _, start := range []string{"-x", "--all", "--", "--end-of-options"} {
		if _, err := resolveStartPoint(repo, start); err == nil {
			t.Fatalf("resolveStartPoint(%q) succeeded, want an error", start)
		}
	}
}

func TestAddFromMissing(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	missing := filepath.Join(filepath.Dir(repo), "missing-src")
	target := addTarget(repo, "wt-missing")
	err := addWorktree(&globals{Root: repo}, "wt-missing", addOptions{from: missing, noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("add --from missing = %v, want not-found error", err)
	}
	if fs.Exists(target) {
		t.Fatalf("missing --from must not create %s", target)
	}
}

func TestAddNoTrack(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	target := addTarget(repo, "wt-notrack")
	res := addResultOf(t, &core.Globals{Root: repo}, "wt-notrack", "--no-index")
	if _, err := git.Run(target, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Fatal("--no-track must leave no upstream")
	}
	if _, err := git.Run(target, "config", "--get", "branch."+res.Branch+".remote"); err == nil {
		t.Fatal("--no-track must leave no branch remote config")
	}
}

func TestAddExistingPathFails(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	target := CanonicalPath(filepath.Join(filepath.Dir(repo), "taken"))
	WriteFile(t, filepath.Join(target, "keep.txt"), "keep", 0o644)
	err := addWorktree(&globals{Root: repo}, "taken", addOptions{path: target, noIndex: true})
	if err == nil {
		t.Fatal("add into an existing non-empty path must fail")
	}
	if !fs.Exists(filepath.Join(target, "keep.txt")) {
		t.Fatal("existing target content must survive")
	}
	if wts, err := git.Worktrees(repo); err != nil || len(wts) != 1 {
		t.Fatalf("failed add must leave only the main worktree, got %v (err %v)", wts, err)
	}
	// WHY: git creates the branch ref BEFORE validating the path, so a failed
	// add leaves a dangling branch that add.go does not roll back. Pin reality.
	if !git.RefExists(repo, "refs/heads/taken") {
		t.Fatal("git is expected to create the branch before failing on the path")
	}
}

// TestAddPathOverride pins the explicit-directory escape hatch.
func TestAddPathOverride(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	target := CanonicalPath(filepath.Join(filepath.Dir(repo), "custom-dir"))
	res := addResultOf(t, &globals{Root: repo}, "feature/override", "--path", target, "--no-index")
	if res.Path != target || res.Branch != "feature/override" {
		t.Fatalf("path/branch = (%q,%q), want (%q,feature/override)", res.Path, res.Branch, target)
	}
}

// TestAddRefusesNestedPath pins the nesting guard: a worktree inside another
// breaks IDE watchers, linters and test runners, so it is rejected.
func TestAddRefusesNestedPath(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	nested := filepath.Join(repo, "nested")
	err := addWorktree(&globals{Root: repo}, "nested", addOptions{path: nested, noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "refusing to create a worktree inside") {
		t.Fatalf("nested add = %v, want refusal", err)
	}
	if fs.Exists(nested) {
		t.Fatal("nested worktree must not be created")
	}
}

// attachRepo returns a repo and tip for feature/attach: one commit ahead of
// main, checked out nowhere, with a configured upstream.
func attachRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := addTestRepo(t)
	MustGit(t, repo, "checkout", "-q", "-b", "feature/attach")
	WriteFile(t, filepath.Join(repo, "attach.txt"), "attach\n", 0o644)
	GitCommit(t, repo, "attach commit", "attach.txt")
	MustGit(t, repo, "checkout", "-q", "main")
	MustGit(t, repo, "config", "branch.feature/attach.remote", "origin")
	MustGit(t, repo, "config", "branch.feature/attach.merge", "refs/heads/feature/attach")
	return repo, MustGit(t, repo, "rev-parse", "refs/heads/feature/attach")
}

// TestAddAttachesExistingBranch pins the contract: adopt the branch, never move it, seed the harness.
func TestAddAttachesExistingBranch(t *testing.T) {
	Sandbox(t)
	repo, tip := attachRepo(t)
	WriteFile(t, filepath.Join(repo, ".mcp.json"), `{"mcpServers":{}}`, 0o644)
	target := addTarget(repo, "feature/attach")

	res := addResultOf(t, &globals{Root: repo}, "feature/attach", "--no-index")
	if !res.Attached || res.Branch != "feature/attach" || res.Path != target {
		t.Fatalf("attach = %+v, want feature/attach at %s", res, target)
	}
	if res.Base != "refs/heads/feature/attach" {
		t.Fatalf("base = %q, want the branch ref", res.Base)
	}
	if got := MustGit(t, target, "rev-parse", "HEAD"); got != tip {
		t.Fatalf("checkout HEAD = %s, want branch tip %s", got, tip)
	}
	if got := MustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved to %s, want %s", got, tip)
	}
	if got := MustGit(t, target, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/attach" {
		t.Fatalf("checkout is not on the existing branch: %q", got)
	}
	if !fs.Exists(filepath.Join(target, ".mcp.json")) {
		t.Fatal("attach must seed the agent harness like a new branch")
	}
	if !fs.Exists(filepath.Join(target, "attach.txt")) {
		t.Fatal("attach must check out the branch tree")
	}
	// Attachment must write no branch config: the upstream survives because
	// worktree add runs without -b/--track.
	if got, err := git.Run(repo, "config", "--get", "branch.feature/attach.remote"); err != nil || got != "origin" {
		t.Fatalf("upstream remote = (%q,%v), want origin", got, err)
	}
	if got, err := git.Run(repo, "config", "--get", "branch.feature/attach.merge"); err != nil || got != "refs/heads/feature/attach" {
		t.Fatalf("upstream merge = (%q,%v), want refs/heads/feature/attach", got, err)
	}
}

// TestAddAttachRejectsCheckedOutBranch pins that an occupied branch names its worktree and writes nothing.
func TestAddAttachRejectsCheckedOutBranch(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	busy := CanonicalPath(filepath.Join(filepath.Dir(repo), "busy"))
	GitWorktreeAdd(t, repo, busy, "feature/busy", "refs/heads/main")

	err := addWorktree(&globals{Root: repo}, "feature/busy", addOptions{noIndex: true})
	if err == nil || isUsage(err) || !strings.Contains(err.Error(), "already checked out") || !strings.Contains(err.Error(), busy) {
		t.Fatalf("add on a checked-out branch = %v, want a non-usage refusal naming %s", err, busy)
	}
	if err := addWorktree(&globals{Root: repo}, "main", addOptions{noIndex: true}); err == nil || isUsage(err) || !strings.Contains(err.Error(), "at the main worktree") {
		t.Fatalf("add on the main worktree's branch = %v, want non-usage main-worktree guidance", err)
	}
	if fs.Exists(addTarget(repo, "feature/busy")) {
		t.Fatal("rejected attach must not create a checkout")
	}
}

// TestAddAttachStartPoint pins the start-point contract: the tip attaches; a
// different commit or unresolvable ref is refused without writes.
func TestAddAttachStartPoint(t *testing.T) {
	Sandbox(t)
	cases := []struct {
		Name    string
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
		t.Run(tc.Name, func(t *testing.T) {
			repo, tip := attachRepo(t)
			start := tc.start
			if start == "tip" {
				start = tip
			}
			err := addWorktree(&globals{Root: repo}, "feature/attach", addOptions{start: start, noIndex: true})
			if !tc.refusal {
				if err != nil {
					t.Fatalf("attach via %s: %v", start, err)
				}
				target := addTarget(repo, "feature/attach")
				if got := MustGit(t, target, "rev-parse", "HEAD"); got != tip {
					t.Fatalf("attach via %s HEAD = %s, want %s", start, got, tip)
				}
				if got := MustGit(t, target, "rev-parse", "--abbrev-ref", "HEAD"); got != "feature/attach" {
					t.Fatalf("attach via %s checked out %q, want the existing branch", start, got)
				}
				return
			}
			if err == nil || isUsage(err) != tc.usage || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("start-point %q = %v, want usage=%v naming %q", start, err, tc.usage, tc.want)
			}
			if got := MustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
				t.Fatalf("branch moved to %s, want %s", got, tip)
			}
			if fs.Exists(addTarget(repo, "feature/attach")) {
				t.Fatal("rejected attach must not create a checkout")
			}
		})
	}
}

// TestAddAttachRejectsOccupiedPath pins that an occupied target keeps its content and the branch stays put.
func TestAddAttachRejectsOccupiedPath(t *testing.T) {
	Sandbox(t)
	repo, tip := attachRepo(t)
	target := CanonicalPath(filepath.Join(filepath.Dir(repo), "taken-attach"))
	WriteFile(t, filepath.Join(target, "keep.txt"), "keep", 0o644)

	if err := addWorktree(&globals{Root: repo}, "feature/attach", addOptions{path: target, noIndex: true}); err == nil {
		t.Fatal("attach into an occupied path must fail")
	}
	if !fs.Exists(filepath.Join(target, "keep.txt")) {
		t.Fatal("occupied path content must survive")
	}
	if got := MustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved to %s, want %s", got, tip)
	}
	if wts, err := git.Worktrees(repo); err != nil || len(wts) != 1 {
		t.Fatalf("failed attach must leave only the main worktree, got %v (err %v)", wts, err)
	}
}

// TestAddAttachFailedSeedKeepsBranch pins that a seed failure never deletes an adopted branch.
func TestAddAttachFailedSeedKeepsBranch(t *testing.T) {
	Sandbox(t)
	repo, tip := attachRepo(t)
	WriteFile(t, filepath.Join(harness.ChunkhoundDBDir(repo), harness.RootGuardName), "{not json", 0o644)

	err := addWorktree(&globals{Root: repo}, "feature/attach", addOptions{noIndex: true})
	if err == nil || !strings.Contains(err.Error(), "existing branch was left untouched") {
		t.Fatalf("seed failure = %v, want the branch-untouched note", err)
	}
	if got := MustGit(t, repo, "rev-parse", "refs/heads/feature/attach"); got != tip {
		t.Fatalf("branch moved or was deleted: %s, want %s", got, tip)
	}
}

// TestRollbackAddBranchOwnership pins the rollback boundary: wtm deletes only the branches it created.
func TestRollbackAddBranchOwnership(t *testing.T) {
	Sandbox(t)
	cases := []struct {
		Name     string
		attached bool
	}{
		{"attached branch survives", true},
		{"created branch removed", false},
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			repo := addTestRepo(t)
			path := CanonicalPath(filepath.Join(filepath.Dir(repo), "wt"))
			GitWorktreeAdd(t, repo, path, "feature/keep", "refs/heads/main")
			err := rollbackAdd(repo, path, "feature/keep", tc.attached, errors.New("seed failed"))
			if err == nil || fs.Exists(path) {
				t.Fatalf("rollback = %v, want the seed error with a removed checkout", err)
			}
			if kept := git.RefExists(repo, "refs/heads/feature/keep"); kept != tc.attached {
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
			Sandbox(t)
			repo := addTestRepo(t)
			GitTestBranch(t, repo, tc.branch)
			if tc.tag {
				MustGit(t, repo, "tag", tc.branch)
			}
			target := addTarget(repo, tc.branch)

			var res addResult
			if warn := CaptureStderr(t, func() { res = addResultOf(t, &globals{Root: repo}, tc.request, "--no-index") }); warn != "" {
				t.Fatalf("attaching %q must not warn, got %q", tc.branch, warn)
			}
			if !res.Attached || res.Branch != tc.branch || res.Path != target {
				t.Fatalf("attach %q = %+v, want %s at %s", tc.request, res, tc.branch, target)
			}
			if got := MustGit(t, res.Path, "rev-parse", "--symbolic-full-name", "HEAD"); got != "refs/heads/"+tc.branch {
				t.Fatalf("checkout branch = %q, want refs/heads/%s", got, tc.branch)
			}
		})
	}
}

// TestMatchLocalBranch pins exact-first, then a unique case fold; several case variants are ambiguous.
func TestMatchLocalBranch(t *testing.T) {
	refs := map[string]bool{"Feature/X": true, "main": true}
	if got, err := core.MatchLocalBranch(refs, "Feature/X"); err != nil || got.Name != "Feature/X" || !got.Exists {
		t.Fatalf("exact = (%+v,%v), want Feature/X", got, err)
	}
	if got, err := core.MatchLocalBranch(refs, "feature/x"); err != nil || got.Name != "Feature/X" || !got.Exists {
		t.Fatalf("case-fold = (%+v,%v), want Feature/X", got, err)
	}
	if got, err := core.MatchLocalBranch(refs, "fix/x"); err != nil || got.Name != "fix/x" || got.Exists {
		t.Fatalf("missing = (%+v,%v), want an absent fix/x", got, err)
	}
	twoVariants := map[string]bool{"Feature/X": true, "feature/x": true}
	if _, err := core.MatchLocalBranch(twoVariants, "FEATURE/X"); !errors.Is(err, core.ErrAmbiguousBranch) || !isUsage(err) {
		t.Fatalf("ambiguous case variants = %v, want the typed usage error", err)
	}
	if got, err := core.MatchLocalBranch(twoVariants, "Feature/X"); err != nil || got.Name != "Feature/X" {
		t.Fatalf("exact spelling must beat ambiguity: (%+v,%v)", got, err)
	}
}

// TestAddTempRefusesExistingBranch pins that --temp never adopts a branch: a
// pre-existing tmp/ branch would otherwise fall under gc's reclaim rules.
func TestAddTempRefusesExistingBranch(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	GitTestBranch(t, repo, "tmp/keep")
	target := addTarget(repo, "tmp/keep")

	for _, args := range [][]string{
		{"tmp/keep", "--temp", "--no-index"},
		{"tmp/keep", "--ttl", "30m", "--no-index"},
	} {
		err := cmdAdd(&globals{Root: repo}, args)
		if err == nil || isUsage(err) || !strings.Contains(err.Error(), "refusing --temp for existing branch") {
			t.Fatalf("cmdAdd(%v) = %v, want a non-usage temp refusal", args, err)
		}
		if fs.Exists(target) {
			t.Fatalf("refused temp add created %s", target)
		}
	}
}

// TestAttachedTempBranchIsPermanent pins the safety rule behind the --temp
// refusal: an existing tmp/ branch attaches as a permanent checkout. The 2h
// backdate is past the default 1h temp TTL but inside the 90d permanent TTL, so
// gc --all would collect a regression that treats the branch as temporary.
func TestAttachedTempBranchIsPermanent(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	GitTestBranch(t, repo, "tmp/keep")

	res := addResultOf(t, &globals{Root: repo}, "tmp/keep", "--no-index")
	if !res.Attached || res.Temp || res.TTL != "" || res.ExpiresAt != nil {
		t.Fatalf("attach = %+v, want an attached permanent checkout", res)
	}
	if _, ok := mustReadTempStore(t)[res.Path]; ok {
		t.Fatalf("attached %s got a temp record", res.Path)
	}

	out := CaptureStdout(t, func() {
		if err := core.CmdStatus(&globals{Root: repo, JSON: true}, nil, statusHooks()); err != nil {
			t.Fatal(err)
		}
	})
	var rows []core.JSONWorktree
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range rows {
		if row.Path == res.Path {
			found = true
			if row.Temp || row.ExpiresAt != nil {
				t.Fatalf("status row = %+v, want permanent", row)
			}
		}
	}
	if !found {
		t.Fatalf("status lost %s", res.Path)
	}

	out = CaptureStdout(t, func() {
		if err := core.CmdStatus(&globals{Root: repo}, nil, statusHooks()); err != nil {
			t.Fatal(err)
		}
	})
	line := statusLine(out, res.Path)
	if fields := strings.Fields(strings.TrimPrefix(line, res.Path)); len(fields) < 5 || fields[4] != "-" {
		t.Fatalf("status line %q must show TEMP - for the attached branch", line)
	}

	BackdateGitActivity(t, res.Path, time.Now().Add(-2*time.Hour))
	CaptureStdout(t, func() {
		if err := cmdGC(&globals{Root: repo, Yes: true}, []string{"--all"}); err != nil {
			t.Fatal(err)
		}
	})
	if !fs.Exists(res.Path) || !git.RefExists(repo, "refs/heads/tmp/keep") {
		t.Fatal("gc collected an attached tmp/ branch that must be permanent")
	}
}

func statusLine(out, path string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, path) {
			return line
		}
	}
	return ""
}

// TestResolveLocalBranch pins raw exact, raw case-fold, normalization, and the
// address-mode tmp/ alias. --temp uses only its tmp/ identity.
func TestResolveLocalBranch(t *testing.T) {
	refs := map[string]bool{"Feature/X": true, "Feature/under_score": true, "Feature/C++": true}
	cases := []struct {
		raw, want string
		mode      branchLookupMode
		Exists    bool
	}{
		{"Feature/X", "Feature/X", core.BranchForAdd, true},
		{"feature/x", "Feature/X", core.BranchForAdd, true},
		{"feature/under_score", "Feature/under_score", core.BranchForAdd, true},
		{"feature/c++", "Feature/C++", core.BranchForAdd, true},
		{"Feature Login", "feature-login", core.BranchForAdd, false},
		{"x", "tmp/x", core.BranchForTempAdd, false},
		{"TMP/X", "tmp/x", core.BranchForTempAdd, false},
	}
	for _, tc := range cases {
		got, err := core.ResolveLocalBranch(refs, tc.raw, tc.mode)
		if err != nil || got.Name != tc.want || got.Exists != tc.Exists {
			t.Fatalf("core.ResolveLocalBranch(%q,%v) = (%+v,%v), want %q exists=%v", tc.raw, tc.mode, got, err, tc.want, tc.Exists)
		}
	}

	addressRefs := map[string]bool{"Feature/under_score": true, "review-x": true, "tmp/keep": true}
	for _, tc := range []struct {
		raw, want string
		Exists    bool
	}{
		{"feature/under_score", "Feature/under_score", true},
		{"Review X", "review-x", true},
		{"keep", "tmp/keep", true},
		{"missing", "missing", false},
	} {
		got, err := core.ResolveLocalBranch(addressRefs, tc.raw, core.BranchForAddress)
		if err != nil || got.Name != tc.want || got.Exists != tc.Exists {
			t.Fatalf("core.ResolveLocalBranch(%q,address) = (%+v,%v), want %q exists=%v", tc.raw, got, err, tc.want, tc.Exists)
		}
	}

	if _, err := core.ResolveLocalBranch(nil, "", core.BranchForAdd); err == nil {
		t.Fatal("empty branch must be rejected")
	}
	if _, err := core.ResolveLocalBranch(nil, "feature/@{x}", core.BranchForAdd); err == nil {
		t.Fatal("invalid ref must be rejected")
	}
}

// TestAddMissingBranchUsage pins that the branch is required.
func TestAddMissingBranchUsage(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	err := cmdAdd(&globals{Root: repo}, nil)
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
	Sandbox(t)
	repo := addTestRepo(t)
	WriteFile(t, filepath.Join(harness.ChunkhoundDBDir(repo), harness.RootGuardName), "{not json", 0o644)
	target := addTarget(repo, "wt-badguard")
	err := addWorktree(&globals{Root: repo}, "wt-badguard", addOptions{noIndex: true})
	if err == nil {
		t.Fatal("malformed root guard must surface an error")
	}
	if !strings.Contains(err.Error(), "partial worktree kept") {
		t.Fatalf("error = %v, want the partial-worktree-kept contract", err)
	}
	if !fs.Exists(target) {
		t.Fatalf("dirty partial worktree must be kept: %s", target)
	}
	if !git.RefExists(repo, "refs/heads/wt-badguard") {
		t.Fatal("branch must remain after a kept partial worktree")
	}
}

func TestAddHumanOutputAnnotatesOrigin(t *testing.T) {
	Sandbox(t)
	t.Run("local base", func(t *testing.T) {
		repo := addTestRepo(t)
		var err error
		out := CaptureStdout(t, func() { err = cmdAdd(&globals{Root: repo}, []string{"wt-local", "--no-index"}) })
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		if !strings.Contains(out, "refs/heads/main (local)") {
			t.Fatalf("human output must annotate local base: %q", out)
		}
	})
	t.Run("remote base", func(t *testing.T) {
		repo := addTestRepo(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		var err error
		out := CaptureStdout(t, func() { err = cmdAdd(&globals{Root: repo}, []string{"wt-remote", "--no-index"}) })
		if err != nil {
			t.Fatalf("cmdAdd: %v", err)
		}
		if !strings.Contains(out, "refs/remotes/origin/main (remote)") {
			t.Fatalf("human output must annotate remote base: %q", out)
		}
		res := addResultOf(t, &globals{Root: repo}, "wt-remote-json", "--no-index")
		if res.Base != "refs/remotes/origin/main" {
			t.Fatalf("--json must keep the raw base ref, got %q", res.Base)
		}
	})
	t.Run("origin base wins over earlier-sorting stale remote", func(t *testing.T) {
		repo := addTestRepo(t)                       // commit A on main
		GitTestRemoteHead(t, repo, "fork-5", "main") // fork-5/main -> A (stale)
		WriteFile(t, filepath.Join(repo, "next.txt"), "next\n", 0o644)
		GitCommit(t, repo, "next", "next.txt") // commit B on main
		GitTestRemoteHead(t, repo, "origin", "main")
		res := addResultOf(t, &globals{Root: repo}, "wt-remote-order", "--no-index")
		if res.Base != "refs/remotes/origin/main" {
			t.Fatalf("base = %q, want refs/remotes/origin/main", res.Base)
		}
		want := MustGit(t, repo, "rev-parse", "refs/remotes/origin/main")
		if got := MustGit(t, repo, "rev-parse", "wt-remote-order"); got != want {
			t.Fatalf("branch tip = %s, want %s (origin/main)", got, want)
		}
	})
}

// TestAddDerivedTargetRefusesNesting pins that the derived sibling path runs the
// same nesting guard as --path: with the current worktree itself nested,
// locationBase would otherwise place the new sibling inside another worktree.
func TestAddDerivedTargetRefusesNesting(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	outer := CanonicalPath(filepath.Join(filepath.Dir(repo), "outer"))
	GitWorktreeAdd(t, repo, outer, "outer", "refs/heads/main")
	nested := CanonicalPath(filepath.Join(outer, "nested"))
	GitWorktreeAdd(t, repo, nested, "nested", "refs/heads/main")
	t.Chdir(nested)

	wts, err := git.Worktrees(repo)
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
	Sandbox(t)
	repo := addTestRepo(t)
	res := addResultOf(t, &globals{Root: repo}, "wt-ttl", "--no-index", "--ttl", "30m")
	if !res.Temp || res.Branch != "tmp/wt-ttl" || res.TTL != "30m0s" {
		t.Fatalf("--ttl add = %+v, want tmp/wt-ttl with 30m", res)
	}
	if res.ExpiresAt == nil {
		t.Fatal("--ttl implies temp and must report an expiry")
	}
}

func linkedMainFixture(t *testing.T) (repo, linked string) {
	t.Helper()
	repo = InitRepo(t, "work")
	GitTestCommit(t, repo)
	GitTestBranch(t, repo, "main")
	linked = CanonicalPath(filepath.Join(filepath.Dir(repo), "wt-main"))
	MustGit(t, repo, "worktree", "add", "-q", linked, "main")
	WriteFile(t, filepath.Join(linked, harness.ChunkhoundConfigFile), `{"from":"main-linked"}`, 0o644)
	return repo, linked
}

func advanceWorktree(t *testing.T, dir, name, content, msg string) {
	t.Helper()
	WriteFile(t, filepath.Join(dir, name), content, 0o644)
	MustGit(t, dir, "add", "--", name)
	MustGit(t, dir, "commit", "-q", "-m", msg)
}

func assertAddSeed(t *testing.T, res addResult, base, src, marker string) {
	t.Helper()
	if res.Base != base || CanonicalPath(res.Source) != CanonicalPath(src) {
		t.Fatalf("seed = (%q,%q), want (%q,%q)", res.Base, res.Source, base, src)
	}
	b, err := os.ReadFile(filepath.Join(res.Path, harness.ChunkhoundConfigFile))
	if err != nil || string(b) != marker {
		t.Fatalf("seeded marker = %q (%v), want %q", b, err, marker)
	}
}

func addCapturingStderr(t *testing.T, repo string, args ...string) (addResult, string) {
	t.Helper()
	var res addResult
	errOut := CaptureStderr(t, func() { res = addResultOf(t, &globals{Root: repo}, args...) })
	return res, errOut
}

func assertSeedWarning(t *testing.T, errOut string, notice, stale bool) {
	t.Helper()
	gotNotice := strings.Contains(errOut, "seeding from") || strings.Contains(errOut, "no usable worktree for")
	gotStale := strings.Contains(errOut, "copied ChunkHound index")
	if gotNotice != notice || gotStale != stale {
		t.Fatalf("seed warning = (notice %v, stale %v), want (%v, %v): %q", gotNotice, gotStale, notice, stale, errOut)
	}
}

func TestAddDefaultSourceFollowsCheckedOutDefaultBranch(t *testing.T) {
	t.Run("linked default without remote", func(t *testing.T) {
		Sandbox(t)
		repo, linked := linkedMainFixture(t)
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/heads/main", linked, `{"from":"main-linked"}`)
		assertSeedWarning(t, errOut, false, false)
	})
	t.Run("linked default in sync with origin", func(t *testing.T) {
		Sandbox(t)
		repo, linked := linkedMainFixture(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", linked, `{"from":"main-linked"}`)
		assertSeedWarning(t, errOut, false, false)
	})
	t.Run("linked default ahead of origin", func(t *testing.T) {
		Sandbox(t)
		repo, linked := linkedMainFixture(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		WriteFile(t, harness.ChunkhoundDBPath(linked), "DB", 0o644)
		advanceWorktree(t, linked, "local.txt", "local", "local main")
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", linked, `{"from":"main-linked"}`)
		assertSeedWarning(t, errOut, true, true)
		if got, want := MustGit(t, repo, "rev-parse", res.Branch), MustGit(t, repo, "rev-parse", "refs/remotes/origin/main"); got != want {
			t.Fatalf("branch tip = %s, want origin/main %s", got, want)
		}
	})
	t.Run("origin ahead of linked default", func(t *testing.T) {
		Sandbox(t)
		repo, linked := linkedMainFixture(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		tree := MustGit(t, repo, "rev-parse", "HEAD^{tree}")
		remote := MustGit(t, repo, "commit-tree", tree, "-p", "HEAD", "-m", "remote main")
		MustGit(t, repo, "update-ref", "refs/remotes/origin/main", remote)
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", linked, `{"from":"main-linked"}`)
		assertSeedWarning(t, errOut, false, false)
		if got := MustGit(t, repo, "rev-parse", res.Branch); got != remote {
			t.Fatalf("branch tip = %s, want origin/main %s", got, remote)
		}
	})
	t.Run("default at primary diverged", func(t *testing.T) {
		Sandbox(t)
		repo := addTestRepo(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		advanceWorktree(t, repo, "local.txt", "local", "local main")
		WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), `{"from":"central"}`, 0o644)
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", repo, `{"from":"central"}`)
		assertSeedWarning(t, errOut, true, false)
	})
	t.Run("default checked out nowhere", func(t *testing.T) {
		Sandbox(t)
		repo := InitRepo(t, "work")
		GitTestCommit(t, repo)
		GitTestBranch(t, repo, "main")
		GitTestRemoteHead(t, repo, "origin", "main")
		WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), `{"from":"central"}`, 0o644)
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", repo, `{"from":"central"}`)
		assertSeedWarning(t, errOut, false, false)
	})
	t.Run("from overrides the default checkout", func(t *testing.T) {
		Sandbox(t)
		repo, linked := linkedMainFixture(t)
		GitTestRemoteHead(t, repo, "origin", "main")
		advanceWorktree(t, linked, "local.txt", "local", "local main")
		from := CanonicalPath(filepath.Join(filepath.Dir(repo), "from-src"))
		if err := os.MkdirAll(from, 0o755); err != nil {
			t.Fatal(err)
		}
		WriteFile(t, filepath.Join(from, harness.ChunkhoundConfigFile), `{"from":"explicit"}`, 0o644)
		res, errOut := addCapturingStderr(t, repo, "feature/child", "--from", from, "--no-index")
		assertAddSeed(t, res, "refs/remotes/origin/main", from, `{"from":"explicit"}`)
		assertSeedWarning(t, errOut, false, false)
	})
	t.Run("offline linked worktree falls back to primary", func(t *testing.T) {
		for _, start := range []string{"", "refs/heads/main"} {
			for _, lock := range []bool{false, true} {
				Sandbox(t)
				repo, linked := linkedMainFixture(t)
				advanceWorktree(t, linked, "gone.txt", "gone", "main moved")
				if lock {
					MustGit(t, repo, "worktree", "lock", linked)
				}
				if err := os.RemoveAll(linked); err != nil {
					t.Fatal(err)
				}
				WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), `{"from":"central"}`, 0o644)
				args := []string{"feature/child", "--no-index"}
				if start != "" {
					args = []string{"feature/child", start, "--no-index"}
				}
				res, errOut := addCapturingStderr(t, repo, args...)
				assertAddSeed(t, res, "refs/heads/main", repo, `{"from":"central"}`)
				if !strings.Contains(errOut, "no usable worktree for refs/heads/main") {
					t.Fatalf("offline (locked %v) at start %q must be disclosed, not seeded: %q", lock, start, errOut)
				}
			}
		}
	})
}

// TestAddSkipsWorktreeWithoutGitLink pins the seed safety gate: a linked
// worktree that survives on disk but lost its .git backlink is not a source.
func TestAddSkipsWorktreeWithoutGitLink(t *testing.T) {
	for _, start := range []string{"", "refs/heads/main"} {
		for _, lock := range []bool{false, true} {
			t.Run(fmt.Sprintf("start=%q/locked=%v", start, lock), func(t *testing.T) {
				Sandbox(t)
				repo, linked := linkedMainFixture(t)
				advanceWorktree(t, linked, "gone.txt", "gone", "main moved")
				if lock {
					MustGit(t, repo, "worktree", "lock", linked)
				}
				if err := os.Remove(filepath.Join(linked, ".git")); err != nil {
					t.Fatal(err)
				}
				WriteFile(t, filepath.Join(repo, harness.ChunkhoundConfigFile), `{"from":"central"}`, 0o644)
				args := []string{"feature/child", "--no-index"}
				if start != "" {
					args = []string{"feature/child", start, "--no-index"}
				}
				res, errOut := addCapturingStderr(t, repo, args...)
				assertAddSeed(t, res, "refs/heads/main", repo, `{"from":"central"}`)
				if !strings.Contains(errOut, "no usable worktree for refs/heads/main") {
					t.Fatalf("missing .git link (locked %v) at start %q must be disclosed, not seeded: %q", lock, start, errOut)
				}
			})
		}
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
		got, err := core.TempBranch(in)
		if err != nil || got != want {
			t.Fatalf("core.TempBranch(%q) = (%q,%v), want %q", in, got, err, want)
		}
	}
}

func TestAddTempBranchIdempotent(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	res := addResultOf(t, &globals{Root: repo}, "tmp/already", "--temp", "--no-index")
	if res.Branch != "tmp/already" {
		t.Fatalf("branch = %q, want tmp/already (no double prefix)", res.Branch)
	}
}

// TestAddTempWritesRecord pins the store write: a successful temp add records
// the Canonical checkout path and the default window, last, after seeding.
func TestAddTempWritesRecord(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	target := addTarget(repo, "tmp/wt-rec")
	res := addResultOf(t, &globals{Root: repo}, "wt-rec", "--temp", "--no-index")
	if !res.Temp || res.Path != target {
		t.Fatalf("add = %+v, want temp at %s", res, target)
	}
	rec, ok := mustReadTempStore(t)[target]
	if !ok {
		t.Fatalf("no temp record for %s", target)
	}
	if rec.Branch != "tmp/wt-rec" || rec.TTL != temp.DefaultTempTTL().String() {
		t.Fatalf("record = %+v, want tmp/wt-rec with %s", rec, temp.DefaultTempTTL())
	}
	if rec.CreatedAt.IsZero() {
		t.Fatal("record createdAt must be set")
	}
}

// TestAddTempFailedSeedLeavesNoRecord pins the write-last ordering: a seed that
// fails must leave a durable (permanent) checkout, never a tracked temp.
func TestAddTempFailedSeedLeavesNoRecord(t *testing.T) {
	Sandbox(t)
	repo := addTestRepo(t)
	WriteFile(t, filepath.Join(harness.ChunkhoundDBDir(repo), harness.RootGuardName), "{not json", 0o644)
	target := addTarget(repo, "tmp/wt-badtemp")
	err := addWorktree(&globals{Root: repo}, "wt-badtemp", addOptions{temp: true, ttl: "30m", noIndex: true})
	if err == nil {
		t.Fatal("malformed root guard must fail the temp add")
	}
	if _, ok := mustReadTempStore(t)[target]; ok {
		t.Fatalf("failed seed must not record a temp for %s", target)
	}
}
