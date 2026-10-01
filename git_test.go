package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// git_test.go — Tier-1 contracts for git.go. Every fixture drives the real git
// binary through the production git() wrapper; no mocks or stubs.

// gitTestCommit creates the initial commit every ref-seeding fixture needs.
func gitTestCommit(t *testing.T, repo string) {
	t.Helper()
	writeFile(t, filepath.Join(repo, "seed.txt"), "seed\n", 0o644)
	gitCommit(t, repo, "seed", "seed.txt")
}

// gitTestBranch points a local branch ref at HEAD.
func gitTestBranch(t *testing.T, repo, branch string) {
	t.Helper()
	mustGit(t, repo, "update-ref", "refs/heads/"+branch, "HEAD")
}

// gitTestRemoteHead makes refs/remotes/<remote>/HEAD a symref to
// refs/remotes/<remote>/<branch> using local refs only (no network).
func gitTestRemoteHead(t *testing.T, repo, remote, branch string) {
	t.Helper()
	target := "refs/remotes/" + remote + "/" + branch
	mustGit(t, repo, "update-ref", target, "HEAD")
	mustGit(t, repo, "symbolic-ref", "refs/remotes/"+remote+"/HEAD", target)
}

// gitTestRepoRoot fails the test on error so callers can compare paths.
func gitTestRepoRoot(t *testing.T, dir string) string {
	t.Helper()
	root, err := repoRoot(dir)
	if err != nil {
		t.Fatalf("repoRoot(%s): %v", dir, err)
	}
	return root
}

type parseCase struct {
	name string
	in   string
	want []gworktree
}

func parseWorktreeCases() []parseCase {
	return []parseCase{
		{
			name: "single main",
			in: `worktree /repo/main
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main
`,
			want: []gworktree{{Path: "/repo/main", Branch: "main", Main: true}},
		},
		{
			name: "main and linked, Main only on first",
			in: `worktree /repo/main
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main

worktree /repo/linked
HEAD 2222222222222222222222222222222222222222
branch refs/heads/feature
`,
			want: []gworktree{
				{Path: "/repo/main", Branch: "main", Main: true},
				{Path: "/repo/linked", Branch: "feature"},
			},
		},
		{
			name: "detached leaves Branch empty",
			in: `worktree /repo/detached
HEAD 3333333333333333333333333333333333333333
detached
`,
			want: []gworktree{{Path: "/repo/detached", Main: true}},
		},
		{
			name: "bare",
			in: `worktree /repo/bare
HEAD 4444444444444444444444444444444444444444
bare
`,
			want: []gworktree{{Path: "/repo/bare", Bare: true, Main: true}},
		},
		{
			name: "locked without reason",
			in: `worktree /repo/locked
HEAD 5555555555555555555555555555555555555555
branch refs/heads/locked
locked
`,
			want: []gworktree{{Path: "/repo/locked", Branch: "locked", Locked: true, Main: true}},
		},
		{
			name: "locked with reason",
			in: `worktree /repo/locked
HEAD 5555555555555555555555555555555555555555
branch refs/heads/locked
locked user reason
`,
			want: []gworktree{{Path: "/repo/locked", Branch: "locked", Locked: true, Main: true}},
		},
		{
			name: "prunable",
			in: `worktree /repo/prunable
HEAD 6666666666666666666666666666666666666666
branch refs/heads/dead
prunable gitdir file points to non-existent location
`,
			want: []gworktree{{Path: "/repo/prunable", Branch: "dead", Prunable: true, Main: true}},
		},
		{
			name: "CRLF line endings",
			in:   "worktree /repo/main\r\nHEAD 1\r\nbranch refs/heads/main\r\n\r\nworktree /repo/linked\r\nHEAD 2\r\nbranch refs/heads/x\r\n",
			want: []gworktree{
				{Path: "/repo/main", Branch: "main", Main: true},
				{Path: "/repo/linked", Branch: "x"},
			},
		},
		{
			name: "leading, trailing and extra blank lines",
			in: `

worktree /repo/main
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main


`,
			want: []gworktree{{Path: "/repo/main", Branch: "main", Main: true}},
		},
		{
			name: "path containing spaces",
			in: `worktree /repo/my main
HEAD 1111111111111111111111111111111111111111
branch refs/heads/main
`,
			want: []gworktree{{Path: "/repo/my main", Branch: "main", Main: true}},
		},
		{
			name: "empty input yields nil",
			in:   "",
			want: nil,
		},
	}
}

func TestParseWorktrees(t *testing.T) {
	for _, tc := range parseWorktreeCases() {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseWorktrees(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("parseWorktrees(%q) = %#v, want %#v", tc.in, got, tc.want)
			}
		})
	}
}

type defaultBranchCase struct {
	name       string
	seed       func(t *testing.T, repo string)
	wantName   string
	wantRemote string
}

func defaultBranchCases() []defaultBranchCase {
	return []defaultBranchCase{
		{"origin HEAD symref", func(t *testing.T, r string) { gitTestRemoteHead(t, r, "origin", "trunk") }, "trunk", "origin"},
		{"non-origin remote", func(t *testing.T, r string) { gitTestRemoteHead(t, r, "upstream", "release") }, "release", "upstream"},
		{"origin preferred over other remotes", func(t *testing.T, r string) {
			gitTestRemoteHead(t, r, "origin", "main")
			gitTestRemoteHead(t, r, "upstream", "release")
		}, "main", "origin"},
		{"local main", func(t *testing.T, r string) { gitTestBranch(t, r, "main") }, "main", ""},
		{"local master beats develop and trunk", func(t *testing.T, r string) {
			gitTestBranch(t, r, "master")
			gitTestBranch(t, r, "develop")
			gitTestBranch(t, r, "trunk")
		}, "master", ""},
		{"local develop beats trunk", func(t *testing.T, r string) {
			gitTestBranch(t, r, "develop")
			gitTestBranch(t, r, "trunk")
		}, "develop", ""},
		{"local trunk", func(t *testing.T, r string) { gitTestBranch(t, r, "trunk") }, "trunk", ""},
		{"init.defaultBranch config", func(t *testing.T, r string) {
			mustGit(t, r, "config", "init.defaultBranch", "custom")
		}, "custom", ""},
		{"main worktree branch fallback", nil, "work", ""},
		{"no candidate falls back to main", func(t *testing.T, r string) {
			mustGit(t, r, "checkout", "-q", "--detach")
		}, "main", ""},
	}
}

func TestDefaultBranchPrecedence(t *testing.T) {
	for _, tc := range defaultBranchCases() {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t, "work")
			gitTestCommit(t, repo)
			if tc.seed != nil {
				tc.seed(t, repo)
			}
			name, remote := defaultBranch(repo)
			if name != tc.wantName || remote != tc.wantRemote {
				t.Fatalf("defaultBranch() = (%q,%q), want (%q,%q)", name, remote, tc.wantName, tc.wantRemote)
			}
		})
	}
}

func TestRemoteDefaultRef(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, repo string)
		want string
	}{
		{"no remote HEAD", nil, ""},
		{"origin HEAD", func(t *testing.T, r string) { gitTestRemoteHead(t, r, "origin", "main") }, "refs/remotes/origin/main"},
		{"non-origin HEAD", func(t *testing.T, r string) { gitTestRemoteHead(t, r, "upstream", "release") }, "refs/remotes/upstream/release"},
		{"origin HEAD wins over later remotes", func(t *testing.T, r string) {
			gitTestRemoteHead(t, r, "origin", "main")
			gitTestRemoteHead(t, r, "upstream", "release")
		}, "refs/remotes/origin/main"},
		{"non-symbolic HEAD is ignored", func(t *testing.T, r string) {
			mustGit(t, r, "update-ref", "refs/remotes/origin/HEAD", "HEAD")
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t, "work")
			gitTestCommit(t, repo)
			if tc.seed != nil {
				tc.seed(t, repo)
			}
			if got := remoteDefaultRef(repo); got != tc.want {
				t.Fatalf("remoteDefaultRef() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestBaseRefFor(t *testing.T) {
	cases := []struct {
		name string
		seed func(t *testing.T, repo string)
		want string
	}{
		{"remote-tracking preferred over local", func(t *testing.T, r string) {
			gitTestRemoteHead(t, r, "origin", "main")
			gitTestBranch(t, r, "main")
		}, "refs/remotes/origin/main"},
		{"local branch without remote", func(t *testing.T, r string) { gitTestBranch(t, r, "main") }, "refs/heads/main"},
		{"non-origin remote-tracking", func(t *testing.T, r string) { gitTestRemoteHead(t, r, "upstream", "trunk") }, "refs/remotes/upstream/trunk"},
		{"literal name when nothing resolves", func(t *testing.T, r string) {
			mustGit(t, r, "checkout", "-q", "--detach")
		}, "main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t, "work")
			gitTestCommit(t, repo)
			tc.seed(t, repo)
			if got := baseRefFor(repo); got != tc.want {
				t.Fatalf("baseRefFor() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRefExists(t *testing.T) {
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	mustGit(t, repo, "tag", "v1")
	gitTestRemoteHead(t, repo, "origin", "main")

	cases := []struct {
		name string
		ref  string
		want bool
	}{
		{"local branch", "refs/heads/main", true},
		{"tag", "refs/tags/v1", true},
		{"HEAD", "HEAD", true},
		{"remote-tracking", "refs/remotes/origin/main", true},
		{"missing branch", "refs/heads/nope", false},
		{"missing tag", "refs/tags/nope", false},
	}
	for _, tc := range cases {
		if got := refExists(repo, tc.ref); got != tc.want {
			t.Fatalf("refExists(%s) = %v, want %v", tc.ref, got, tc.want)
		}
	}
	if refExists(t.TempDir(), "HEAD") {
		t.Fatal("refExists must be false outside a repository")
	}
}

func TestIsIgnored(t *testing.T) {
	repo := initRepo(t, "main")
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\nbuild/\n", 0o644)

	cases := []struct {
		rel  string
		want bool
	}{
		{"ignored.txt", true},
		{"build/out.o", true},
		{"other.txt", false},
	}
	for _, tc := range cases {
		if got := isIgnored(repo, tc.rel); got != tc.want {
			t.Fatalf("isIgnored(%q) = %v, want %v", tc.rel, got, tc.want)
		}
	}
	if isIgnored(t.TempDir(), "x") {
		t.Fatal("isIgnored must be false outside a repository")
	}
}

func TestWorktreeAddThenRemove(t *testing.T) {
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	wt := filepath.Join(filepath.Dir(repo), "linked")

	if err := worktreeAdd(repo, wt, "feature", "refs/heads/main"); err != nil {
		t.Fatalf("worktreeAdd: %v", err)
	}
	info, err := os.Stat(filepath.Join(wt, ".git"))
	if err != nil {
		t.Fatalf("linked .git: %v", err)
	}
	if info.IsDir() {
		t.Fatal("linked worktree .git must be a file, not a directory")
	}
	if !refExists(repo, "refs/heads/feature") {
		t.Fatal("feature branch was not created")
	}
	if _, err := git(wt, "rev-parse", "--abbrev-ref", "@{upstream}"); err == nil {
		t.Fatal("--no-track must leave no upstream")
	}
	if err := worktreeRemove(repo, wt); err != nil {
		t.Fatalf("worktreeRemove: %v", err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree path still present: %v", err)
	}
}

// worktreeDirty must mirror git's removal predicate: ignored files do not block
// `worktree remove`, non-ignored untracked files do.
func TestWorktreeDirty(t *testing.T) {
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	if worktreeDirty(repo) {
		t.Fatal("fresh repo must be clean")
	}
	writeFile(t, filepath.Join(repo, ".gitignore"), "ignored.txt\n", 0o644)
	gitCommit(t, repo, "ignore", ".gitignore")
	writeFile(t, filepath.Join(repo, "ignored.txt"), "x", 0o644)
	if worktreeDirty(repo) {
		t.Fatal("ignored file must not count as dirty")
	}
	writeFile(t, filepath.Join(repo, "untracked.txt"), "x", 0o644)
	if !worktreeDirty(repo) {
		t.Fatal("untracked non-ignored file must count as dirty")
	}
}

func TestRepoRootFromLinkedWorktree(t *testing.T) {
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	linked := filepath.Join(filepath.Dir(repo), "linked")
	gitWorktreeAdd(t, repo, linked, "feature", "refs/heads/main")

	want := canonical(repo)
	if got := canonical(gitTestRepoRoot(t, repo)); got != want {
		t.Fatalf("repoRoot(main) = %q, want %q", got, want)
	}
	if got := canonical(gitTestRepoRoot(t, linked)); got != want {
		t.Fatalf("repoRoot(linked) = %q, want %q", got, want)
	}
}

func TestGitErrorIsExplicit(t *testing.T) {
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)

	out, err := git(repo, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil || out != "main" {
		t.Fatalf("git rev-parse = (%q,%v), want (main,nil)", out, err)
	}
	_, err = git(repo, "rev-parse", "--verify", "refs/heads/missing")
	if err == nil {
		t.Fatal("expected error for missing ref")
	}
	if !strings.Contains(err.Error(), "git rev-parse") {
		t.Fatalf("error must name the command: %v", err)
	}
	_, err = git(repo, "definitely-not-a-subcommand")
	if err == nil || !strings.Contains(err.Error(), "definitely-not-a-subcommand") {
		t.Fatalf("error must quote the failing invocation: %v", err)
	}
}
