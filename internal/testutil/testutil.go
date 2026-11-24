package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGitDate pins commit identity/time so worktreeLastUsed is reproducible.
const TestGitDate = "2020-01-01T00:00:00+00:00"

var (
	wtmBinPath  string
	wtmBuildErr error
	wtmBuildDir string
	buildOnce   sync.Once
	// buildEnvErr retains the cause when `go env` fails, so a binary build
	// needing the cache overrides fails loudly instead of writing into a
	// sandboxed HOME.
	buildEnvErr error
)

// CanonicalPath resolves symlinks on the deepest existing ancestor, mirroring
// the production fs helper without importing it (testutil must stay a leaf).
func CanonicalPath(p string) string {
	p = filepath.Clean(p)
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	dir, base := filepath.Split(p)
	if dir == "" || dir == p {
		return p
	}
	return filepath.Join(CanonicalPath(filepath.Clean(dir)), base)
}

// gitCmd runs git in dir via the real binary, returning trimmed stdout.
func gitCmd(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(string(out)), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// buildEnv pins the Go caches outside any sandboxed HOME: tests redirect HOME
// before the first RunWTM, so a lazy build must not write into the sandbox
// (it would corrupt module-cache permissions and break TempDir cleanup).
var buildEnv []string

func init() {
	// -json keeps the paths intact when a cache dir contains a space; the
	// space-split form silently dropped the overrides in that case.
	out, err := exec.Command("go", "env", "-json", "GOCACHE", "GOMODCACHE").Output()
	if err != nil {
		buildEnvErr = fmt.Errorf("go env: %w", err)
		return
	}
	var env struct {
		GOCACHE    string `json:"GOCACHE"`
		GOMODCACHE string `json:"GOMODCACHE"`
	}
	if json.Unmarshal(out, &env) != nil || env.GOCACHE == "" || env.GOMODCACHE == "" {
		buildEnvErr = fmt.Errorf("go env returned no GOCACHE/GOMODCACHE")
		return
	}
	buildEnv = append(os.Environ(), "GOCACHE="+env.GOCACHE, "GOMODCACHE="+env.GOMODCACHE)
}

// BuildWTM returns the prebuilt wtm binary path, failing if the build failed.
func BuildWTM(t *testing.T) string {
	t.Helper()
	buildOnce.Do(buildWTMOnce)
	if wtmBuildErr != nil {
		t.Fatal(wtmBuildErr)
	}
	return wtmBinPath
}

func buildWTMOnce() {
	if buildEnvErr != nil {
		wtmBuildErr = buildEnvErr
		return
	}
	wtmBuildDir, wtmBuildErr = os.MkdirTemp("", "wtm-bin-")
	if wtmBuildErr != nil {
		return
	}
	name := "wtm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	wtmBinPath = filepath.Join(wtmBuildDir, name)
	cmd := exec.Command("go", "build", "-o", wtmBinPath, "wtm")
	if buildEnv != nil {
		cmd.Env = buildEnv
	}
	if out, err := cmd.CombinedOutput(); err != nil {
		wtmBuildErr = fmt.Errorf("go build: %w\n%s", err, out)
	}
}

// CleanupWTM removes the built wtm binary temp dir for this process, if a
// build ran. Every consumer TestMain must call it after m.Run().
func CleanupWTM() {
	if wtmBuildDir != "" {
		_ = os.RemoveAll(wtmBuildDir)
	}
}

// MustGit runs git through the production wrapper and fails the test on error.
func MustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitCmd(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// gitWorktrees returns git's worktree listing with all paths normalized to
// forward slashes, matching the format required for cross-platform comparisons.
func gitWorktrees(t *testing.T, repo string) string {
	t.Helper()
	return filepath.ToSlash(MustGit(t, repo, "worktree", "list", "--porcelain"))
}

// worktreeListed reports whether git's listing holds wt, and returns the
// listing for failure messages. CanonicalPath(wt) resolves Windows 8.3 aliases
// (RUNNER~1) and casing, so a caller may pass any spelling of an existing path;
// git itself always emits the long canonical form.
func worktreeListed(t *testing.T, repo, wt string) (bool, string) {
	t.Helper()
	list := gitWorktrees(t, repo)
	return strings.Contains(list, filepath.ToSlash(CanonicalPath(wt))), list
}

// AssertWorktreeListed asserts that git worktree list registers wt.
func AssertWorktreeListed(t *testing.T, repo, wt string) {
	t.Helper()
	if listed, list := worktreeListed(t, repo, wt); !listed {
		t.Fatalf("git worktree list lost %s:\n%s", wt, list)
	}
}

// AssertWorktreeNotListed asserts that git worktree list does not register wt.
func AssertWorktreeNotListed(t *testing.T, repo, wt string) {
	t.Helper()
	if listed, list := worktreeListed(t, repo, wt); listed {
		t.Fatalf("git worktree list still has %s:\n%s", wt, list)
	}
}

// AssertErrorNamesPath pins that err names path. Both sides compare canonically
// and in one separator spelling: git emits forward slashes on Windows, while
// t.TempDir paths use backslashes.
func AssertErrorNamesPath(t *testing.T, what string, err error, path string) {
	t.Helper()
	if err == nil || !strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(CanonicalPath(path))) {
		t.Fatalf("%s (want %s): %v", what, path, err)
	}
}

// SetHomeEnv points os.UserHomeDir at home on every OS: it reads HOME on Unix
// and USERPROFILE on Windows, so tests must set both or the rewrite is a no-op.
func SetHomeEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(home)
		t.Setenv("HOMEDRIVE", vol)
		t.Setenv("HOMEPATH", strings.TrimPrefix(home, vol))
	}
}

// InitRepo creates a repo named "repo" inside a per-test temp dir so sibling
// worktrees live under the same cleaned-up tree.
func InitRepo(t *testing.T, branch string) string {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "wtm")
	t.Setenv("GIT_AUTHOR_EMAIL", "wtm@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "wtm")
	t.Setenv("GIT_COMMITTER_EMAIL", "wtm@example.com")
	t.Setenv("GIT_AUTHOR_DATE", TestGitDate)
	t.Setenv("GIT_COMMITTER_DATE", TestGitDate)
	// Neutralize host git config so user/system state cannot leak into assertions.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty-gitconfig"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	MustGit(t, repo, "init", "-q", "-b", branch)
	MustGit(t, repo, "config", "user.name", "wtm")
	MustGit(t, repo, "config", "user.email", "wtm@example.com")
	MustGit(t, repo, "config", "core.autocrlf", "false")
	MustGit(t, repo, "config", "core.filemode", "false")
	MustGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

// GitCommit commits the given paths in repo.
func GitCommit(t *testing.T, repo, msg string, paths ...string) {
	t.Helper()
	MustGit(t, repo, append([]string{"add", "--"}, paths...)...)
	MustGit(t, repo, "commit", "-q", "-m", msg)
}

// GitWorktreeAdd adds a worktree in repo at path/branch/start.
func GitWorktreeAdd(t *testing.T, repo, path, branch, start string) {
	t.Helper()
	MustGit(t, repo, "worktree", "add", "-b", branch, path, start)
}

// sandboxEnv carries the sandboxed home/agent dirs.
type sandboxEnv struct {
	Home      string
	AgentDir  string
	ClaudeDir string
}

// Sandbox isolates HOME/agent dirs and pins locale/timezone. On Windows
// os.UserHomeDir reads USERPROFILE/HOMEDRIVE.
func Sandbox(t *testing.T) sandboxEnv {
	t.Helper()
	home := t.TempDir()
	agent := filepath.Join(home, "pi-agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(home, "claude")
	if err := os.MkdirAll(filepath.Join(claude, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	SetHomeEnv(t, home)
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("TZ", "UTC")
	t.Setenv("LC_ALL", "C")
	return sandboxEnv{Home: home, AgentDir: agent, ClaudeDir: claude}
}

// SettingsFile is the single SSOT for where config persists settings.
func SettingsFile(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".wtm", "settings.json")
}

// SetTTL seeds settings directly, bypassing the CLI for fixture speed.
func SetTTL(t *testing.T, ttl string) {
	t.Helper()
	WriteFile(t, SettingsFile(t), fmt.Sprintf("{\"unusedTTL\":%q}\n", ttl), 0o600)
}

// WriteFile writes path with content and mode, creating parents.
func WriteFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// ClaudeTranscript renders a Claude session transcript with a cwd-bearing
// conversation record.
func ClaudeTranscript(wtPath string) string {
	return "{\"type\":\"queue-operation\",\"operation\":\"dequeue\",\"cwd\":null}\n" +
		fmt.Sprintf("{\"type\":\"user\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"seed\"}}\n", wtPath)
}

// mkAgentSession writes one JSONL session header under a harness session
// subdir, pins its mtime, and returns the file path. Callers pass the harness
// config dir (e.g. harness.PiAgent.ConfigDir()) so this helper stays leaf.
func mkAgentSession(t *testing.T, configDir, subdir, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(configDir, subdir, dirName, dirName+".jsonl")
	line := fmt.Sprintf("{\"id\":%q,\"cwd\":%q}\n", dirName, wtPath)
	if subdir == "projects" {
		line = ClaudeTranscript(wtPath)
	}
	WriteFile(t, p, line, 0o644)
	if err := os.Chtimes(p, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return p
}

// capture redirects *target (os.Stdout/os.Stderr) for the duration of fn and
// returns everything written; a reader goroutine prevents pipe-buffer deadlock.
// Cleanup runs in a nested defer so t.Fatal/panic cannot leave the process
// with a redirected stdout/stderr.
func capture(t *testing.T, target **os.File, fn func()) string {
	t.Helper()
	old := *target
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	*target = w
	defer func() {
		*target = old
		_ = w.Close()
	}()
	done := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	var out string
	func() {
		defer func() {
			_ = w.Close()
			out = <-done
			_ = r.Close()
		}()
		fn()
	}()
	return out
}

// CaptureStdout redirects os.Stdout for fn.
func CaptureStdout(t *testing.T, fn func()) string { return capture(t, &os.Stdout, fn) }

// CaptureStderr redirects os.Stderr for fn.
func CaptureStderr(t *testing.T, fn func()) string { return capture(t, &os.Stderr, fn) }

// MustSymlink skips when the host (or git on Windows) cannot create symlinks.
func MustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

// RunWTM execs the real binary in dir with extra env, returning stdout, stderr
// and the exit code.
func RunWTM(t *testing.T, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(BuildWTM(t), args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	rc := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("run wtm %v: %v", args, err)
		}
		rc = exit.ExitCode()
	}
	return out.String(), errOut.String(), rc
}

// GitTestCommit creates the initial commit every ref-seeding fixture needs.
func GitTestCommit(t *testing.T, repo string) {
	t.Helper()
	WriteFile(t, filepath.Join(repo, "seed.txt"), "seed\n", 0o644)
	GitCommit(t, repo, "seed", "seed.txt")
}

// GitTestBranch points a local branch ref at HEAD.
func GitTestBranch(t *testing.T, repo, branch string) {
	t.Helper()
	MustGit(t, repo, "update-ref", "refs/heads/"+branch, "HEAD")
}

// GitTestUpstreamFixture creates a repo with a bare origin and pushes main.
func GitTestUpstreamFixture(t *testing.T) (repo, origin string) {
	t.Helper()
	repo = InitRepo(t, "main")
	GitTestCommit(t, repo)
	origin = filepath.Join(filepath.Dir(repo), "origin.git")
	MustGit(t, repo, "init", "-q", "--bare", "-b", "main", origin)
	MustGit(t, repo, "remote", "add", "origin", origin)
	MustGit(t, repo, "push", "-q", "origin", "main")
	return repo, origin
}

// GitTestRemoteHead makes refs/remotes/<remote>/HEAD a symref to
// refs/remotes/<remote>/<branch> using local refs only (no network).
func GitTestRemoteHead(t *testing.T, repo, remote, branch string) {
	t.Helper()
	target := "refs/remotes/" + remote + "/" + branch
	MustGit(t, repo, "update-ref", target, "HEAD")
	MustGit(t, repo, "symbolic-ref", "refs/remotes/"+remote+"/HEAD", target)
}

// RemoteHasBranch reports whether a branch still exists on a remote, checked
// from workDir so a local bare origin needs no network.
func RemoteHasBranch(t *testing.T, workDir, remote, branch string) bool {
	t.Helper()
	out, err := gitCmd(workDir, "ls-remote", "--heads", remote, "refs/heads/"+branch)
	if err != nil {
		t.Fatalf("ls-remote: %v", err)
	}
	return out != ""
}

// SetMtime pins path's mtime to when.
func SetMtime(t *testing.T, path string, when time.Time) {
	t.Helper()
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
}

// AgentConfigDir mirrors the harness's config-dir resolution (env override,
// else home + rel) without importing the harness package. A harness-side test
// pins that it stays equal to the agents' ConfigDir.
func AgentConfigDir(env string, rel ...string) string {
	if d := os.Getenv(env); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	parts := append([]string{home}, rel...)
	return filepath.Join(parts...)
}

// MkSession writes a Pi session header pointing at wtPath and pins its mtime.
func MkSession(t *testing.T, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	return mkAgentSession(t, AgentConfigDir("PI_CODING_AGENT_DIR", ".pi", "agent"), "sessions", wtPath, dirName, mtime)
}

// MkClaudeSession writes Claude metadata before the cwd-bearing conversation
// record, matching transcripts where the first record cannot identify an owner.
func MkClaudeSession(t *testing.T, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	return mkAgentSession(t, AgentConfigDir("CLAUDE_CONFIG_DIR", ".claude"), "projects", wtPath, dirName, mtime)
}

// GitAdminDir resolves the per-worktree private admin dir ($GIT_DIR), mirroring
// the production git.PrivateDir.
func GitAdminDir(t *testing.T, dir string) string {
	t.Helper()
	out, err := gitCmd(dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// BackdateActivityIndex pins the private index mtime; returns the index path.
func BackdateActivityIndex(t *testing.T, path string, when time.Time) string {
	t.Helper()
	index := filepath.Join(GitAdminDir(t, path), "index")
	SetMtime(t, index, when)
	return index
}

// BackdateGitActivity pins both the index and the checkout .git backlink.
func BackdateGitActivity(t *testing.T, path string, when time.Time) {
	t.Helper()
	BackdateActivityIndex(t, path, when)
	SetMtime(t, filepath.Join(path, ".git"), when)
}

// TempTestTime and TempNow pin the temp fixtures' clock.
var (
	TempTestTime = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	TempNow      = TempTestTime.Add(2 * time.Hour)
)

// NativeRepo creates a seeded repo with a single commit.
func NativeRepo(t *testing.T) string {
	t.Helper()
	repo := InitRepo(t, "main")
	WriteFile(t, filepath.Join(repo, "README.md"), "hello\n", 0o644)
	GitCommit(t, repo, "seed", "README.md")
	return repo
}
