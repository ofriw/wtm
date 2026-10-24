//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"
)

// progress_tui_test.go — Tier-2 e2e of inline progress on a real stderr PTY.
// stdout/stdin share one pty and stderr gets a second, mirroring gc_tui_test.go:
// explicitly requested progress stays isolated from stdout, so the frames are
// pollable and every line can be width-checked.

// runStatusTTY runs `wtm status` behind ptys and returns its streams, the raw
// (unstripped) stderr, and its exit code. text() erases escape sequences, so
// escape-level assertions must read rawErr. env passes the timing seams that
// keep the renderer deterministic on a loaded runner.
func runStatusTTY(t *testing.T, repo string, cols int, env, args []string) (stdout, stderr, rawErr string, rc int) {
	t.Helper()

	masterIn, slaveIn := openTTY(t, cols)
	masterErr, slaveErr := openTTY(t, cols)
	cmd := exec.Command(buildWTM(t), append([]string{"status"}, args...)...)
	cmd.Dir = repo
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slaveIn, slaveIn, slaveErr
	cmd.Env = childTerminalEnv(append(gcTTYEnv(), env...))
	if err := cmd.Start(); err != nil {
		t.Fatalf("start wtm status: %v", err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	_ = slaveIn.Close()
	_ = slaveErr.Close()

	outStream, errStream := &ttyPoll{}, &ttyPoll{}
	var wg sync.WaitGroup
	wg.Add(2)
	go drainPTY(masterIn, outStream.append, &wg)
	go drainPTY(masterErr, errStream.append, &wg)

	waitCh, _ := waitGCTTY(cmd)
	waitErr := awaitStatus(t, cmd, waitCh, outStream, errStream)
	wg.Wait()
	return crToLF(outStream.text()), crToLF(errStream.text()), errStream.raw(), exitCode(t, "wtm status", waitErr)
}

// awaitStatus bounds a non-interactive child with a watchdog so a hang is a
// diagnosable failure instead of stuck CI.
func awaitStatus(t *testing.T, cmd *exec.Cmd, waitCh <-chan error, out, errS *ttyPoll) error {
	t.Helper()
	select {
	case err := <-waitCh:
		return err
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatalf("wtm status did not finish in 30s\nstdout:\n%s\nstderr:\n%s", out.text(), errS.text())
		return nil // unreachable
	}
}

// Status stays silent when it is fast, and on an explicit --progress always it
// keeps a truthful summary on stderr without terminal queries. Animated frames
// coalesce at 60 FPS, so a short phase may never be painted; phase coverage
// belongs to TestStatusPlainProgressPhases, and frame width to
// TestProgressLineFitsWidth. Asserting either on the diff-rendered stream would
// splice fragments of successive frames. The default mode pins the silence
// window far past the fixture's work, so silence is a contract instead of a
// race against 200 ms.
func TestStatusProgressTTY(t *testing.T) {
	repo, _, _ := gcTUIFixture(t)
	for _, mode := range []string{"default", "always"} {
		t.Run(mode, func(t *testing.T) {
			var args, env []string
			if mode == "always" {
				args = []string{"--progress", "always"}
			} else {
				env = []string{progressDelayEnv + "=1h"}
			}
			stdout, stderr, rawErr, rc := runStatusTTY(t, repo, 80, env, args)
			if rc != 0 {
				t.Fatalf("rc = %d, want 0\nstdout: %s\nstderr: %s", rc, stdout, stderr)
			}
			if mode == "default" {
				if stderr != "" {
					t.Errorf("default status must be silent on stderr: %q", stderr)
				}
				assertNoTerminalQueries(t, rawErr)
				return
			}
			if !strings.Contains(stderr, "✓ done") {
				t.Errorf("stderr missing the done summary: %q", stderr)
			}
			assertNoTerminalQueries(t, rawErr)
		})
	}
}

// TestStatusPlainProgressPhases pins phase coverage on the renderer's non-TTY
// sibling: a redirected --progress always writes one plain line per transition,
// so a fast phase cannot be hidden by frame cadence. This is the phase-content
// contract; TestStatusProgressTTY owns the animated wiring and summary.
func TestStatusPlainProgressPhases(t *testing.T) {
	repo, _, _ := gcTUIFixture(t)
	_, stderr, rc := runWTM(t, repo, nil, "status", "--progress", "always")
	if rc != 0 {
		t.Fatalf("rc = %d, want 0\nstderr: %s", rc, stderr)
	}
	if strings.Contains(stderr, "\x1b") {
		t.Errorf("plain progress carries escape bytes: %q", stderr)
	}
	assertOrdered(t, stderr, "start indexing sessions", "start scanning worktrees", "✓ done in")
}

// assertOrdered pins that every want appears in s, in the given order.
func assertOrdered(t *testing.T, s string, want ...string) {
	t.Helper()
	rest := s
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Fatalf("%q missing or out of order; want %q in:\n%s", w, strings.Join(want, " -> "), s)
		}
		rest = rest[i+len(w):]
	}
}

// TestWithProgressRendererPTY pins the in-process orchestration on a real
// terminal: under an explicit --progress always, work runs against a live
// renderer on the injected writer, the work error surfaces as withProgressTo's
// error, and the terminal frame reports the failure.
func TestWithProgressRendererPTY(t *testing.T) {
	var g globals
	g.progress = progressAlways
	masterErr, slaveErr := openTTY(t, 80)
	workErr := errors.New("boom")

	outStream := &ttyPoll{}
	var wg sync.WaitGroup
	wg.Add(1)
	go drainPTY(masterErr, outStream.append, &wg)

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	// A failed observation must not leave the work goroutine blocked.
	t.Cleanup(releaseOnce)

	errCh := make(chan error, 1)
	go func() {
		errCh <- withProgressTo(slaveErr, &g, progressOptions{}, func(p *progress) error {
			p.phase("demo", 1)
			p.advance(1)
			// Hold the phase until a frame carries it: a phase that ends between
			// frames is never visible, and a sleep would only guess the cadence.
			<-release
			return workErr
		})
	}()
	if err := outStream.waitContains("demo", 15*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	releaseOnce()
	err := <-errCh
	// Closing the slave ends the drain: the renderer has exited by the time
	// withProgressTo returns.
	_ = slaveErr.Close()
	wg.Wait()

	if !errors.Is(err, workErr) {
		t.Fatalf("withProgressTo error = %v, want the work error", err)
	}
	stderr := crToLF(outStream.text())
	for _, want := range []string{"demo", "✗ failed"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("renderer output missing %q; stderr:\n%s", want, stderr)
		}
	}
}

// TestProgressFastTransientSilentPTY pins the delay contract on a real
// terminal: a transient run that finishes inside the silence window writes no
// frame at all, so a fast status leaves stderr clean. The window is explicit so
// the assertion cannot race the product default.
func TestProgressFastTransientSilentPTY(t *testing.T) {
	var g globals
	g.progress = progressAlways
	masterErr, slaveErr := openTTY(t, 80)
	outStream := &ttyPoll{}
	var wg sync.WaitGroup
	wg.Add(1)
	go drainPTY(masterErr, outStream.append, &wg)

	err := withProgressTo(slaveErr, &g, progressOptions{transient: true, delay: time.Hour}, func(p *progress) error {
		// A backlog past any fixed hand-off buffer: status emits ~4 events per
		// worktree, so this is ~100 worktrees' worth. Buffering used to strand
		// the worker here until the window expired.
		p.phase("indexing sessions", 200)
		for i := range 200 {
			p.detail(fmt.Sprintf("worktree-%d", i))
			p.advance(1)
		}
		p.notice("warning: broken session")
		return nil
	})
	_ = slaveErr.Close()
	wg.Wait()
	if err != nil {
		t.Fatalf("withProgressTo: %v", err)
	}
	// Frames stay silent, but a disclosure must still print.
	text := outStream.text()
	if !strings.Contains(text, "warning: broken session") {
		t.Errorf("fast transient run dropped its notice; stderr text %q", text)
	}
	if strings.Contains(text, "indexing sessions") || strings.Contains(outStream.raw(), "\x1b") {
		t.Errorf("fast transient run painted a frame: %q", outStream.raw())
	}
}

// TestProgressFastTransientVisiblePTY pins the other side of the same window:
// with an explicit tiny delay the renderer takes over and the phase reaches the
// terminal, so the silent test above cannot pass by never rendering at all.
func TestProgressFastTransientVisiblePTY(t *testing.T) {
	var g globals
	g.progress = progressAlways
	masterErr, slaveErr := openTTY(t, 80)
	outStream := &ttyPoll{}
	var wg sync.WaitGroup
	wg.Add(1)
	go drainPTY(masterErr, outStream.append, &wg)

	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	errCh := make(chan error, 1)
	go func() {
		errCh <- withProgressTo(slaveErr, &g, progressOptions{transient: true, delay: time.Nanosecond}, func(p *progress) error {
			p.phase("indexing sessions", 0)
			// Outlast the renderer's first frame by holding the phase until the
			// test observes it: no sleep, no frame-cadence guess.
			<-release
			return nil
		})
	}()
	if err := outStream.waitContains("indexing sessions", 15*time.Second, nil); err != nil {
		t.Fatal(err)
	}
	releaseOnce()
	if err := <-errCh; err != nil {
		t.Fatalf("withProgressTo: %v", err)
	}
	_ = slaveErr.Close()
	wg.Wait()
	rawErr := outStream.raw()
	if !strings.Contains(crToLF(outStream.text()), "indexing sessions") {
		t.Errorf("visible transient run never rendered its phase; raw stderr:\n%s", rawErr)
	}
	assertNoTerminalQueries(t, rawErr)
}
