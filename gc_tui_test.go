//go:build !windows

// The interactive gc contract needs a dual PTY (creack/pty), which cannot be
// opened on windows CI, so it is verified on unix only.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

// gc_tui_test.go — Tier-2 e2e of the interactive gc TUI, the one interactive
// contract with no other coverage. huh forms read os.Stdin and render to os.Stderr, so
// the real binary gets stdin/stdout on one pty (making interactive() true) and
// stderr on a second pty: every side is a genuine terminal, yet TUI frames,
// "aborted" and stdout report lines land on separately pollable streams.
// huh's own WithInput/WithOutput seam is unusable here: ui.go's multiSelect and
// confirm never expose it, and tests must exercise the shipped binary anyway.

// gcTUIStep sends `send` once `want` has appeared in the stderr stream; "" for
// want sends immediately. Keystrokes are always polled in, never timed.
type gcTUIStep struct {
	want string
	send string
}

// ttyPoll accumulates one pty end's bytes for polling; a concurrent reader is
// mandatory, or the child blocks once the pty buffer fills.
type ttyPoll struct {
	mu  sync.Mutex
	buf []byte
}

func (p *ttyPoll) append(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
}

// text returns the ANSI-stripped bytes so matches ignore cursor juggling.
func (p *ttyPoll) text() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return ansi.Strip(string(p.buf))
}

// raw returns the bytes unstripped; assertions about the escape sequences
// themselves must use this, since text() erases what they check.
func (p *ttyPoll) raw() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return string(p.buf)
}

// waitContains polls until sub appears or the child exits, so a hung TUI is a
// diagnosable timeout instead of a stuck test.
func (p *ttyPoll) waitContains(sub string, timeout time.Duration, childDone <-chan struct{}) error {
	deadline := time.After(timeout)
	for {
		if strings.Contains(p.text(), sub) {
			return nil
		}
		select {
		case <-childDone:
			return fmt.Errorf("child exited before %q appeared; output so far:\n%s", sub, p.text())
		case <-deadline:
			return fmt.Errorf("timeout waiting for %q; output so far:\n%s", sub, p.text())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// gcTUIEnv scrubs ambient terminal state so the TUI render is deterministic.
func gcTTYEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		switch k {
		case "COLUMNS", "LINES", "TERM", "COLORTERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "TERM=xterm")
}

// gcTTYWidth is the default pty width: wide enough that every form line stays
// unwrapped, so polled titles are contiguous in the stripped stream.
const gcTTYWidth = 200

// openTTY hands back one pty pair at the requested column count.
func openTTY(t *testing.T, cols int) (master, slave *os.File) {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		if errors.Is(err, syscall.EPERM) || errors.Is(err, os.ErrPermission) || strings.Contains(err.Error(), "operation not permitted") {
			t.Skipf("pty.Open: %v (skipping PTY test in restricted environment)", err)
		}
		t.Fatalf("pty.Open: %v", err)
	}
	t.Cleanup(func() { _ = master.Close(); _ = slave.Close() })
	if err := pty.Setsize(master, &pty.Winsize{Rows: 50, Cols: uint16(cols)}); err != nil {
		t.Fatalf("pty.Setsize: %v", err)
	}
	return master, slave
}

// driveGCTTY writes each step's keystrokes only after its trigger text has
// rendered, so a key can never race the form that must consume it.
func driveGCTTY(master *os.File, poll *ttyPoll, steps []gcTUIStep, childDone <-chan struct{}) error {
	for i, step := range steps {
		if err := poll.waitContains(step.want, 15*time.Second, childDone); err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		if _, err := master.Write([]byte(step.send)); err != nil {
			return fmt.Errorf("step %d: send %q: %w", i, step.send, err)
		}
	}
	return nil
}

// drainPTY copies one pty master into a poll buffer until the child exits
// (EOF or EIO on macOS/Linux); without a concurrent reader the child blocks
// on a full pty buffer.
func drainPTY(master *os.File, sink func([]byte), wg *sync.WaitGroup) {
	defer wg.Done()
	buf := make([]byte, 4096)
	for {
		n, err := master.Read(buf)
		if n > 0 {
			sink(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// runGCTTY runs the real `wtm gc` behind ptys, replays steps, and returns the
// child's stdout, stderr and exit code. Extra args are appended after "gc".
func runGCTTY(t *testing.T, repo string, cols int, steps []gcTUIStep, args ...string) (stdout, stderr string, rc int) {
	t.Helper()

	masterIn, slaveIn := openTTY(t, cols)
	masterErr, slaveErr := openTTY(t, cols)
	argv := append([]string{buildWTM(t), "gc"}, args...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repo
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slaveIn, slaveIn, slaveErr
	cmd.Env = gcTTYEnv()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wtm gc: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	_ = slaveIn.Close()
	_ = slaveErr.Close()

	outStream, errStream := &ttyPoll{}, &ttyPoll{}
	var wg sync.WaitGroup
	wg.Add(2)
	go drainPTY(masterIn, outStream.append, &wg)
	go drainPTY(masterErr, errStream.append, &wg)

	waitCh, childDone := waitGCTTY(cmd)
	driveCh := make(chan error, 1)
	go func() { driveCh <- driveGCTTY(masterIn, errStream, steps, childDone) }()

	waitErr := awaitGCTTY(t, cmd, waitCh, driveCh, outStream, errStream)
	if driveErr := <-driveCh; driveErr != nil {
		t.Fatalf("keystroke drive failed: %v\nstdout:\n%s\nstderr:\n%s", driveErr, outStream.text(), errStream.text())
	}
	wg.Wait()
	// The tty layer translates LF to CRLF; assertions match on logical lines.
	return crToLF(outStream.text()), crToLF(errStream.text()), exitCode(t, "wtm gc", waitErr)
}

// crToLF turns every CR into LF. The tty layer translates LF to CRLF, and the
// renderer rewrites lines in place with CR; treating CR as a line break keeps
// same-line repaints as separate logical lines so width assertions stay honest.
func crToLF(s string) string { return strings.ReplaceAll(s, "\r", "\n") }

// awaitGCTTY bounds the whole TUI session with a watchdog: a form that never
// yields must fail the test with both streams attached, not hang CI.
func awaitGCTTY(t *testing.T, cmd *exec.Cmd, waitCh <-chan error, driveCh <-chan error, out, errS *ttyPoll) error {
	t.Helper()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(60 * time.Second):
		_ = cmd.Process.Kill()
		<-driveCh
		t.Fatalf("gc TUI did not finish in 60s\nstdout:\n%s\nstderr:\n%s", out.text(), errS.text())
		return nil // unreachable
	}
}

// waitGCTTY wraps cmd.Wait: exactly one call, with completion signalled for
// the keystroke driver's polling loop.
func waitGCTTY(cmd *exec.Cmd) (<-chan error, <-chan struct{}) {
	waitCh := make(chan error, 1)
	childDone := make(chan struct{})
	go func() {
		waitCh <- cmd.Wait()
		close(childDone)
	}()
	return waitCh, childDone
}

// exitCode maps a command's Wait error to the process exit code.
func exitCode(t *testing.T, what string, err error) int {
	t.Helper()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("wait %s: %v", what, err)
	}
	return exit.ExitCode()
}

// gcTUIFixture seeds a repo whose only gc candidate is UNUSED wt1: a stale
// session, a backdated creation baseline, no untracked files. Main is ACTIVE
// and must never be offered by the picker.
func gcTUIFixture(t *testing.T) (repo, wt1, session string) {
	t.Helper()
	sandbox(t)
	repo = initRepo(t, "main")
	writeFile(t, filepath.Join(repo, "README.md"), "hello\n", 0o644)
	gitCommit(t, repo, "seed", "README.md")
	wt1 = filepath.Join(filepath.Dir(repo), "wt1")
	gitWorktreeAdd(t, repo, wt1, "wt1", "main")
	session = mkSession(t, wt1, "s-wt1", gcTUISeedTime())
	if err := os.Chtimes(filepath.Join(wt1, ".git"), gcTUISeedTime(), gcTUISeedTime()); err != nil {
		t.Fatal(err)
	}
	return repo, wt1, session
}

// gcTUISeedTime converts the fixed fixture date; panicking is fine for a
// constant literal that cannot fail.
func gcTUISeedTime() time.Time {
	when, err := time.Parse(time.RFC3339, testGitDate)
	if err != nil {
		panic(err)
	}
	return when
}

// TestGCTUIDefaultNothingSelected pins the default-off contract: enter on the
// untouched picker selects nothing, the confirm is skipped, nothing is removed
// and wtm reports "nothing to do" with exit 0.
func TestGCTUIDefaultNothingSelected(t *testing.T) {
	repo, wt1, session := gcTUIFixture(t)

	stdout, stderr, rc := runGCTTY(t, repo, gcTTYWidth, []gcTUIStep{
		{want: "Select worktrees to remove", send: "\r"},
	})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	if !strings.Contains(stderr, "Select worktrees to remove") {
		t.Errorf("picker never rendered; stderr:\n%s", stderr)
	}
	if strings.Contains(stderr, "Remove 1 worktree(s)?") {
		t.Errorf("empty selection must skip the confirm; stderr:\n%s", stderr)
	}
	if strings.Contains(stdout, "removed") || strings.Contains(stderr, "aborted") {
		t.Errorf("default-off must remove nothing; stdout=%q stderr=%q", stdout, stderr)
	}
	if !strings.Contains(stdout, "nothing to do") {
		t.Errorf("stdout = %q, want the nothing-to-do report", stdout)
	}
	assertWorktreeIntact(t, wt1, session)
	assertWorktreeListed(t, repo, wt1)
}

// TestGCTUIToggleAcceptRemoves pins the space-toggle flow: toggling wt1 and
// accepting the confirm removes the worktree, prunes and purges its session.
func TestGCTUIToggleAcceptRemoves(t *testing.T) {
	repo, wt1, session := gcTUIFixture(t)

	// wtm prints canonical paths; macOS mounts /var/folders via /private/var.
	canonical, err := filepath.EvalSymlinks(wt1)
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr, rc := runGCTTY(t, repo, gcTTYWidth, []gcTUIStep{
		{want: "Select worktrees to remove", send: " \r"},
		{want: "Remove 1 worktree(s)?", send: "y"},
	})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstdout: %s\nstderr: %s", rc, stdout, stderr)
	}
	if !strings.Contains(stdout, "removed "+canonical) {
		t.Errorf("stdout = %q, want the removed line for %s", stdout, canonical)
	}
	if _, err := os.Stat(wt1); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("worktree %s must be gone after accept (stat err %v)", wt1, err)
	}
	if _, err := os.Stat(session); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("session %s must be purged on accept (stat err %v)", session, err)
	}
	for _, want := range []string{"removing worktrees", "pruning", "✓ done"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing removal progress %q; stderr:\n%s", want, stderr)
		}
	}
	assertWorktreeNotListed(t, repo, wt1)
}

// TestGCTUIDeclineAborts pins Fix A end to end: declining prints "aborted" on
// stderr, exits 0 with nothing on stdout, and keeps the worktree.
func TestGCTUIDeclineAborts(t *testing.T) {
	repo, wt1, session := gcTUIFixture(t)

	stdout, stderr, rc := runGCTTY(t, repo, gcTTYWidth, []gcTUIStep{
		{want: "Select worktrees to remove", send: " \r"},
		{want: "Remove 1 worktree(s)?", send: "n"},
	})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (decline is not an error)", rc)
	}
	if !strings.Contains(stderr, "aborted") {
		t.Errorf("stderr = %q, want the aborted notice", stderr)
	}
	if strings.Contains(stdout, "removed") {
		t.Errorf("stdout = %q, want no removal on decline", stdout)
	}
	assertWorktreeIntact(t, wt1, session)
	assertWorktreeListed(t, repo, wt1)
}

// TestGCPickerNarrowWidth pins the fit contract: at 80 columns every rendered
// TUI line must stay within the pty, or lipgloss wraps it and the grid columns
// visibly misalign.
func TestGCPickerNarrowWidth(t *testing.T) {
	repo, _, _ := gcTUIFixture(t)

	_, stderr, rc := runGCTTY(t, repo, 80, []gcTUIStep{
		{want: "Select worktrees to remove", send: "\r"},
	})
	if rc != 0 {
		t.Fatalf("rc = %d, want 0; stderr:\n%s", rc, stderr)
	}
	assertLinesFit(t, stderr, 80)
}

// assertLinesFit fails on the first content line wider than cols. Trailing
// spaces are ignored: the frame pads to width, and bubbletea's erase sequence
// (cursor-up followed by a space) leaks one stray space into the stripped
// stream at the capture boundary.
func assertLinesFit(t *testing.T, s string, cols int) {
	t.Helper()
	for _, line := range strings.Split(s, "\n") {
		if w := dispWidth(strings.TrimRight(line, " ")); w > cols {
			t.Fatalf("line is %d cells, exceeds %d:\n%q", w, cols, line)
		}
	}
}

func assertWorktreeIntact(t *testing.T, wt, session string) {
	t.Helper()
	if _, err := os.Stat(wt); err != nil {
		t.Errorf("worktree %s must survive: %v", wt, err)
	}
	if _, err := os.Stat(session); err != nil {
		t.Errorf("session %s must survive a decline (err %v)", session, err)
	}
}

// TestGCTUIDeclineJSON pins the machine contract of a declined gc: stdout
// carries the abort report shape (empty removals, flag echoes), stderr the
// aborted notice, and the exit code stays 0.
func TestGCTUIDeclineJSON(t *testing.T) {
	repo, wt1, session := gcTUIFixture(t)

	stdout, stderr, rc := runGCTTY(t, repo, gcTTYWidth, []gcTUIStep{
		{want: "Select worktrees to remove", send: " \r"},
		{want: "Remove 1 worktree(s)?", send: "n"},
	}, "--json")
	if rc != 0 {
		t.Fatalf("rc = %d, want 0 (decline is not an error)", rc)
	}
	var res gcResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("declined --json stdout is not a gc report: %v\nstdout:\n%s", err, stdout)
	}
	if len(res.Removed) != 0 || len(res.RemoteDeleted) != 0 || res.SessionsPurged != 0 {
		t.Fatalf("report = %+v, want the empty abort shape", res)
	}
	if res.KeptSessions || res.KeptRemote {
		t.Fatalf("report = %+v, want both keeps false", res)
	}
	if !strings.Contains(stderr, "aborted") {
		t.Errorf("stderr = %q, want the aborted notice", stderr)
	}
	assertWorktreeIntact(t, wt1, session)
	assertWorktreeListed(t, repo, wt1)
}
