//go:build !windows

package main

import (
	"errors"
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
// escape-level assertions must read rawErr.
func runStatusTTY(t *testing.T, repo string, cols int, args ...string) (stdout, stderr, rawErr string, rc int) {
	t.Helper()

	masterIn, slaveIn := openTTY(t, cols)
	masterErr, slaveErr := openTTY(t, cols)
	cmd := exec.Command(buildWTM(t), append([]string{"status"}, args...)...)
	cmd.Dir = repo
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slaveIn, slaveIn, slaveErr
	cmd.Env = gcTTYEnv()
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

// Status stays silent by default even on a TTY; explicit progress renders all
// phases without terminal queries or overflowing lines.
func TestStatusProgressTTY(t *testing.T) {
	repo, _, _ := gcTUIFixture(t)
	for _, mode := range []string{"default", "always"} {
		t.Run(mode, func(t *testing.T) {
			var args []string
			if mode == "always" {
				args = []string{"--progress", "always"}
			}
			stdout, stderr, rawErr, rc := runStatusTTY(t, repo, 80, args...)
			if rc != 0 {
				t.Fatalf("rc = %d, want 0\nstdout: %s\nstderr: %s", rc, stdout, stderr)
			}
			assertStatusProgress(t, stderr, mode == "always")
			assertNoTerminalQueries(t, rawErr)
			assertLinesFit(t, stderr, 80)
		})
	}
}

// assertStatusProgress pins the stderr contract for status.
func assertStatusProgress(t *testing.T, stderr string, enabled bool) {
	t.Helper()
	if !enabled {
		if stderr != "" {
			t.Errorf("default status must be silent on stderr: %q", stderr)
		}
		return
	}
	for _, want := range []string{"reading worktrees", "indexing sessions", "reading upstreams", "scanning worktrees", "✓ done"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr missing progress %q; stderr:\n%s", want, stderr)
		}
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

	errCh := make(chan error, 1)
	go func() {
		errCh <- withProgressTo(slaveErr, &g, true, func(p *progress) error {
			p.phase("demo", 1)
			p.advance(1)
			return workErr
		})
	}()
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
