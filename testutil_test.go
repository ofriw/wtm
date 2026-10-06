package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// testutil_test.go — hermetic fixtures shared by every Tier-1/Tier-3 test.
// No mocks: helpers drive the real git binary, the real filesystem and the
// real wtm binary. Tests must not call t.Parallel when they mutate env
// (t.Setenv/t.Chdir forbid it).

// testGitDate pins commit identity/time so worktreeLastUsed is reproducible.
const testGitDate = "2020-01-01T00:00:00+00:00"

var (
	wtmBinPath  string
	wtmBuildErr error
	wtmBuildDir string
)

// TestMain builds the wtm binary ONCE before any test can sandbox HOME (a
// sandboxed HOME would relocate GOMODCACHE into a read-only temp module cache
// and break TempDir cleanup). Tier-1 tests simply ignore the binary.
func TestMain(m *testing.M) {
	buildWTMOnce()
	code := m.Run()
	if wtmBuildDir != "" {
		_ = os.RemoveAll(wtmBuildDir)
	}
	os.Exit(code)
}

func buildWTMOnce() {
	wtmBuildDir, wtmBuildErr = os.MkdirTemp("", "wtm-bin-")
	if wtmBuildErr != nil {
		return
	}
	name := "wtm"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	wtmBinPath = filepath.Join(wtmBuildDir, name)
	if out, err := exec.Command("go", "build", "-o", wtmBinPath, ".").CombinedOutput(); err != nil {
		wtmBuildErr = fmt.Errorf("go build: %w\n%s", err, out)
	}
}

// buildWTM returns the prebuilt wtm binary path, failing if the build failed.
func buildWTM(t *testing.T) string {
	t.Helper()
	if wtmBuildErr != nil {
		t.Fatal(wtmBuildErr)
	}
	return wtmBinPath
}

// mustGit runs git through the production wrapper and fails the test on error.
func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := git(dir, args...)
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return out
}

// gitWorktrees returns git's worktree listing with all paths normalized to
// forward slashes, matching the format required for cross-platform comparisons.
func gitWorktrees(t *testing.T, repo string) string {
	t.Helper()
	return filepath.ToSlash(mustGit(t, repo, "worktree", "list", "--porcelain"))
}

// assertWorktreeListed asserts that git worktree list registers wt.
func assertWorktreeListed(t *testing.T, repo, wt string) {
	t.Helper()
	if list := gitWorktrees(t, repo); !strings.Contains(list, filepath.ToSlash(wt)) {
		t.Fatalf("git worktree list lost %s:\n%s", wt, list)
	}
}

// assertWorktreeNotListed asserts that git worktree list does not register wt.
func assertWorktreeNotListed(t *testing.T, repo, wt string) {
	t.Helper()
	if list := gitWorktrees(t, repo); strings.Contains(list, filepath.ToSlash(wt)) {
		t.Fatalf("git worktree list still has %s:\n%s", wt, list)
	}
}

// setHomeEnv points os.UserHomeDir at home on every OS: it reads HOME on Unix
// and USERPROFILE on Windows, so tests must set both or the rewrite is a no-op.
func setHomeEnv(t *testing.T, home string) {
	t.Helper()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(home)
		t.Setenv("HOMEDRIVE", vol)
		t.Setenv("HOMEPATH", strings.TrimPrefix(home, vol))
	}
}

// initRepo creates a repo named "repo" inside a per-test temp dir so sibling
// worktrees live under the same cleaned-up tree.
func initRepo(t *testing.T, branch string) string {
	t.Helper()
	t.Setenv("GIT_AUTHOR_NAME", "wtm")
	t.Setenv("GIT_AUTHOR_EMAIL", "wtm@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "wtm")
	t.Setenv("GIT_COMMITTER_EMAIL", "wtm@example.com")
	t.Setenv("GIT_AUTHOR_DATE", testGitDate)
	t.Setenv("GIT_COMMITTER_DATE", testGitDate)
	// Neutralize host git config so user/system state cannot leak into assertions.
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "empty-gitconfig"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	mustGit(t, repo, "init", "-q", "-b", branch)
	mustGit(t, repo, "config", "user.name", "wtm")
	mustGit(t, repo, "config", "user.email", "wtm@example.com")
	mustGit(t, repo, "config", "core.autocrlf", "false")
	mustGit(t, repo, "config", "core.filemode", "false")
	mustGit(t, repo, "config", "commit.gpgsign", "false")
	return repo
}

func gitCommit(t *testing.T, repo, msg string, paths ...string) {
	t.Helper()
	mustGit(t, repo, append([]string{"add", "--"}, paths...)...)
	mustGit(t, repo, "commit", "-q", "-m", msg)
}

func gitWorktreeAdd(t *testing.T, repo, path, branch, start string) {
	t.Helper()
	mustGit(t, repo, "worktree", "add", "-b", branch, path, start)
}

type sandboxEnv struct {
	Home      string
	AgentDir  string
	ClaudeDir string
}

// sandbox isolates HOME/agent dirs and pins locale/timezone. On Windows
// os.UserHomeDir reads USERPROFILE/HOMEDRIVE.
func sandbox(t *testing.T) sandboxEnv {
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
	setHomeEnv(t, home)
	t.Setenv("PI_CODING_AGENT_DIR", agent)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("TZ", "UTC")
	t.Setenv("LC_ALL", "C")
	return sandboxEnv{Home: home, AgentDir: agent, ClaudeDir: claude}
}

// settingsFile is the single SSOT for where cmdConfig persists settings.
func settingsFile(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(home, ".wtm", "settings.json")
}

// setTTL seeds settings directly, bypassing the CLI for fixture speed.
func setTTL(t *testing.T, ttl string) {
	t.Helper()
	writeFile(t, settingsFile(t), fmt.Sprintf("{\"unusedTTL\":%q}\n", ttl), 0o600)
}

func writeFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

// mkSession writes a Pi session header pointing at wtPath and pins its mtime.
func mkSession(t *testing.T, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	return mkAgentSession(t, piAgent.configDir(), "sessions", wtPath, dirName, mtime)
}

// mkClaudeSession writes Claude metadata before the cwd-bearing conversation
// record, matching transcripts where the first record cannot identify an owner.
func mkClaudeSession(t *testing.T, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	return mkAgentSession(t, claudeAgent.configDir(), "projects", wtPath, dirName, mtime)
}

func claudeTranscript(wtPath string) string {
	return "{\"type\":\"queue-operation\",\"operation\":\"dequeue\",\"cwd\":null}\n" +
		fmt.Sprintf("{\"type\":\"user\",\"cwd\":%q,\"message\":{\"role\":\"user\",\"content\":\"seed\"}}\n", wtPath)
}

// mkAgentSession writes one JSONL session header under the harness session
// subdir, pins its mtime, and returns the file path.
func mkAgentSession(t *testing.T, configDir, subdir, wtPath, dirName string, mtime time.Time) string {
	t.Helper()
	p := filepath.Join(configDir, subdir, dirName, dirName+".jsonl")
	line := fmt.Sprintf("{\"id\":%q,\"cwd\":%q}\n", dirName, wtPath)
	if subdir == claudeAgent.sessionSubdir {
		line = claudeTranscript(wtPath)
	}
	writeFile(t, p, line, 0o644)
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

func captureStdout(t *testing.T, fn func()) string { return capture(t, &os.Stdout, fn) }
func captureStderr(t *testing.T, fn func()) string { return capture(t, &os.Stderr, fn) }

// mustSymlink skips when the host (or git on Windows) cannot create symlinks.
func mustSymlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
}

// runWTM execs the real binary in dir with extra env, returning stdout, stderr
// and the exit code.
func runWTM(t *testing.T, dir string, env []string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildWTM(t), args...)
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
