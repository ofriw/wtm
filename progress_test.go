package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// TestProgressEnabled pins the gating truth table: auto is the only mode that
// consults json, the stderr TTY and TERM; explicit modes win unconditionally.
func TestProgressEnabled(t *testing.T) {
	cases := []struct {
		name string
		mode string
		tty  bool
		json bool
		term string
		want bool
	}{
		{"always tty", progressAlways, true, false, "xterm", true},
		{"always non-tty", progressAlways, false, false, "xterm", true},
		{"always json", progressAlways, true, true, "xterm", true},
		{"never tty", progressNever, true, false, "xterm", false},
		{"never non-tty", progressNever, false, false, "xterm", false},
		{"auto tty", progressAuto, true, false, "xterm", true},
		{"auto empty mode", "", true, false, "xterm", true},
		{"auto non-tty", progressAuto, false, false, "xterm", false},
		{"auto json", progressAuto, true, true, "xterm", false},
		{"auto dumb", progressAuto, true, false, "dumb", false},
		{"auto empty term", progressAuto, true, false, "", true},
		{"auto non-tty dumb", progressAuto, false, false, "dumb", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TERM", tc.term)
			if got := progressEnabled(tc.mode, tc.tty, tc.json); got != tc.want {
				t.Errorf("progressEnabled(%q, tty=%v, json=%v, TERM=%q) = %v, want %v",
					tc.mode, tc.tty, tc.json, tc.term, got, tc.want)
			}
		})
	}
}

func TestValidateProgress(t *testing.T) {
	for _, mode := range []string{"", progressAuto, progressAlways, progressNever} {
		if err := validateProgress(mode); err != nil {
			t.Errorf("validateProgress(%q) = %v, want nil", mode, err)
		}
	}
	if err := validateProgress("sometimes"); !isUsage(err) {
		t.Errorf("validateProgress(sometimes) = %v, want usageError", err)
	}
}

// TestProgressModelSummary pins the terminal frame: a clean run says done, and
// a work error surfaced only via the terminal event still reports failure.
func TestProgressModelSummary(t *testing.T) {
	m := newProgressModel(nil, 40, progressOptions{})
	m.apply(progressEvent{kind: evPhase, name: "scanning", total: 2})
	m.apply(progressEvent{kind: evAdvance, n: 2})
	m.finish(nil)
	if got := m.render(); !strings.Contains(got, "✓ done in") {
		t.Errorf("render = %q, want the done summary", got)
	}

	m = newProgressModel(nil, 40, progressOptions{})
	m.finish(errors.New("boom"))
	if got := m.render(); !strings.Contains(got, "✗ failed") {
		t.Errorf("render = %q, want the failed summary", got)
	}
	m2 := newProgressModel(nil, 40, progressOptions{})
	updated, _ := m2.Update(progressDoneMsg{err: errors.New("panic")})
	if got := updated.(progressModel).render(); !strings.Contains(got, "✗ failed") {
		t.Errorf("render after failed done = %q, want the failed summary", got)
	}

}

// TestProgressLineFitsWidth pins the truncation contract: a long detail cannot
// push any rendered line past the budget, even with a bar and a total.
func TestProgressLineFitsWidth(t *testing.T) {
	m := newProgressModel(nil, 20, progressOptions{})
	m.apply(progressEvent{kind: evPhase, name: "scanning worktrees", total: 100})
	m.apply(progressEvent{kind: evAdvance, n: 50})
	m.apply(progressEvent{kind: evDetail, text: "~/a/very/long/path/that/exceeds/the/budget"})
	for _, line := range strings.Split(m.render(), "\n") {
		if w := dispWidth(line); w > 20 {
			t.Errorf("line is %d cells, exceeds 20: %q", w, line)
		}
	}
}

// TestProgressBarOnlyWhileRunning pins the bar contract: only an in-flight phase
// carries one, its cells are full blocks over light shades, and a finished phase
// carries none — a 100% bar frozen into the scrollback frame is noise, and
// bubbles' default half-block fill (▌) reads as a fence rather than a bar.
func TestProgressBarOnlyWhileRunning(t *testing.T) {
	m := newProgressModel(nil, 40, progressOptions{})
	m.apply(progressEvent{kind: evPhase, name: "scanning", total: 4})
	m.apply(progressEvent{kind: evAdvance, n: 2})
	running := m.render()
	for _, want := range []string{"█", "░"} {
		if !strings.Contains(running, want) {
			t.Errorf("running bar is missing %q: %q", want, running)
		}
	}
	if strings.Contains(running, "▌") {
		t.Errorf("running bar uses the half-block default: %q", running)
	}

	m.finish(nil)
	if finished := m.render(); strings.ContainsAny(finished, "█░▌") {
		t.Errorf("finished frame still carries a bar: %q", finished)
	}
}

// TestProgressFailedRun pins the failure contract: a run error keeps the
// phase rows and flips the summary to ✗ failed.
func TestProgressFailedRun(t *testing.T) {
	m := newProgressModel(nil, 40, progressOptions{})
	m.apply(progressEvent{kind: evPhase, name: "removing", total: 2})
	m.apply(progressEvent{kind: evAdvance, n: 1})
	m.finish(errors.New("worktree is locked"))
	if frame := m.render(); !strings.Contains(frame, "✗ failed") {
		t.Errorf("failed frame = %q, want the failure marker", frame)
	}
}

// drainCmd executes a cmd to its leaf messages, recursing through batches, so
// a command's effect is asserted without running a program on a pty.
func drainCmd(cmd tea.Cmd) []tea.Msg {
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var msgs []tea.Msg
		for _, c := range batch {
			msgs = append(msgs, drainCmd(c)...)
		}
		return msgs
	}
	return []tea.Msg{msg}
}

// TestProgressNoticePrintsPinnedLine pins the notice contract: evNotice must
// become a persistent print (tea.Println) — a frame write would be overwritten
// by the next frame and silently drop a destructive-action disclosure. It also
// re-arms the event loop, and the frame itself stays untouched.
func TestProgressNoticePrintsPinnedLine(t *testing.T) {
	events := make(chan progressEvent, 1)
	// nextEvent blocks on this channel; feeding one event lets the drained
	// command return instead of deadlocking the test.
	events <- progressEvent{kind: evAdvance, n: 1}
	m := newProgressModel(events, 40, progressOptions{})
	const notice = "will delete remote origin/feat"
	_, cmd := m.Update(progressEventMsg{kind: evNotice, text: notice})
	if cmd == nil {
		t.Fatal("evNotice produced no command; the disclosure would be dropped")
	}
	printed, rearmed := false, false
	for _, msg := range drainCmd(cmd) {
		if strings.Contains(fmt.Sprintf("%v", msg), notice) {
			printed = true
		}
		if _, ok := msg.(progressEventMsg); ok {
			rearmed = true
		}
	}
	if !printed {
		t.Error("notice text never reached a print command")
	}
	if !rearmed {
		t.Error("notice handling did not re-arm the event loop")
	}
	if frame := m.render(); strings.Contains(frame, notice) {
		t.Errorf("notice leaked into the frame: %q", frame)
	}
}

// terminalQueries are the reply-expecting sequences that must never reach the
// progress stream: input is disabled (tea.WithInput(nil)), so nothing can read
// the terminal's answer and it would sit in the tty until the shell echoed it
// (charmbracelet/bubbletea#1590).
var terminalQueries = []string{"\x1b[?2026$p", "\x1b[?2027$p", "\x1b[?u"}

// TestWithProgressGatedOff pins the gating half of the orchestration: with the
// renderer gated off, work gets the nil-safe handle and no renderer runs.
func TestWithProgressGatedOff(t *testing.T) {
	var g globals
	g.progress = progressNever
	ran := false
	err := withProgressTo(io.Discard, &g, progressOptions{}, func(p *progress) error {
		ran = p == nil
		return nil
	})
	if err != nil || !ran {
		t.Fatalf("withProgressTo = (%v, nil-handle=%v), want (nil, true)", err, ran)
	}
}

// assertNoTerminalQueries pins the input-disabled contract on a captured
// progress stream.
func assertNoTerminalQueries(t *testing.T, stream string) {
	t.Helper()
	for _, q := range terminalQueries {
		if strings.Contains(stream, q) {
			t.Errorf("progress stream carries terminal query %q: its reply would leak to the shell prompt\nstream:\n%s", q, stream)
		}
	}
}

// TestProgressTransientClears pins the status contract: the line disappears on
// exit so the command's own output owns the terminal.
func TestProgressTransientClears(t *testing.T) {
	m := newProgressModel(nil, 40, progressOptions{transient: true})
	m.apply(progressEvent{kind: evPhase, name: "scanning worktrees", total: 2})
	if got := m.render(); !strings.Contains(got, "scanning worktrees") {
		t.Fatalf("live frame = %q, want the phase label", got)
	}
	m.finish(nil)
	if got := m.render(); got != "" {
		t.Errorf("transient frame after exit = %q, want empty", got)
	}
}

// TestProgressLabelReplaces pins the single-line contract: a new phase or step
// replaces the previous text, so the block never accumulates history.
func TestProgressLabelReplaces(t *testing.T) {
	m := newProgressModel(nil, 80, progressOptions{})
	m.apply(progressEvent{kind: evPhase, name: "indexing sessions"})
	m.apply(progressEvent{kind: evDetail, text: "pi · 128 files"})
	got := m.render()
	for _, want := range []string{"indexing sessions", "pi · 128 files"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame = %q, want %q", got, want)
		}
	}
	m.apply(progressEvent{kind: evPhase, name: "parsing sessions", total: 4})
	m.apply(progressEvent{kind: evAdvance, n: 2})
	got = m.render()
	if strings.Contains(got, "indexing sessions") {
		t.Errorf("frame kept the previous phase: %q", got)
	}
	for _, want := range []string{"parsing sessions", "2/4"} {
		if !strings.Contains(got, want) {
			t.Errorf("frame = %q, want %q", got, want)
		}
	}
}

// TestPlainProgressOnPipe pins the non-TTY contract: an explicit --progress
// always on a pipe writes plain lines, never escape bytes, so a redirected run
// stays readable in a log.
func TestPlainProgressOnPipe(t *testing.T) {
	var g globals
	g.progress = progressAlways
	var out strings.Builder
	err := withProgressTo(&out, &g, progressOptions{}, func(p *progress) error {
		p.phase("indexing sessions", 0)
		p.detail("pi · 128 files")
		p.notice("warning: broken session")
		return nil
	})
	if err != nil {
		t.Fatalf("withProgressTo: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "\x1b") {
		t.Errorf("plain progress carries escape bytes: %q", got)
	}
	for _, want := range []string{"start indexing sessions", "warning: broken session", "✓ done in"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain progress = %q, want %q", got, want)
		}
	}
	if strings.Contains(got, "128 files") {
		t.Errorf("plain progress echoed an animated step: %q", got)
	}
}

// TestFastTransientFlushesNotices pins the fast-path disclosure contract: a run
// that beats the silence window drops its frames, but every notice still
// prints. Otherwise a warning would appear or vanish depending on machine speed.
func TestFastTransientFlushesNotices(t *testing.T) {
	const warning = "warning: claude cost unavailable: catalog offline"
	events := make(chan progressEvent, 4)
	events <- progressEvent{kind: evPhase, name: "loading rate catalog"}
	events <- progressEvent{kind: evNotice, text: warning}
	events <- progressEvent{kind: evDone}
	close(events)

	var out strings.Builder
	drainSilentRun(&out, events)
	if got, want := out.String(), warning+"\n"; got != want {
		t.Errorf("silent drain wrote %q, want %q", got, want)
	}
}

// TestProgressSilentWindowNeverStallsWork pins the contract behind the silence
// window: the window costs the renderer's latency, never the worker's. A backlog
// larger than any fixed hand-off buffer must still let a fast run finish and
// stay silent — when the buffer filled, the worker blocked until the window
// expired and status painted the frame it promised to suppress.
func TestProgressSilentWindowNeverStallsWork(t *testing.T) {
	const warning = "warning: usage for /repo/wt: unreadable session"
	var out strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- runProgressProgram(&out, progressOptions{transient: true, delay: 30 * time.Second}, func(p *progress) error {
			p.phase("parsing sessions", 200)
			for i := range 200 {
				p.detail(fmt.Sprintf("worktree-%d", i))
				p.advance(1)
				if i == 100 {
					p.notice(warning)
				}
			}
			return nil
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("fast run = %v, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("fast run stalled in its silence window: worker latency depends on the renderer")
	}
	if got, want := out.String(), warning+"\n"; got != want {
		t.Errorf("silent run wrote %q, want only the notice %q", got, want)
	}
}

// TestRelayProgressStopsWhenCanceled pins the abandoned-renderer release path:
// a renderer that exits early must not leave the relay blocked on a queued
// backlog. The input stays open, so only cancellation can end the relay.
func TestRelayProgressStopsWhenCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	in := make(chan progressEvent, 1)
	in <- progressEvent{kind: evNotice, text: "queued"}
	out := relayProgress(ctx, in)
	cancel()
	deadline := time.After(time.Second)
	for {
		select {
		case _, ok := <-out:
			if !ok {
				return
			}
		case <-deadline:
			t.Fatal("relay stayed blocked after cancel with an unread backlog")
		}
	}
}

// TestProgressStreamKeepsEveryEventInOrder pins what the renderer replays: the
// hand-off removes backpressure only, it never drops or reorders, so counters
// and labels still describe the work.
func TestProgressStreamKeepsEveryEventInOrder(t *testing.T) {
	const count = 500
	workErr, stream := feedProgress(context.Background(), func(p *progress) error {
		p.phase("parsing sessions", count)
		for i := range count {
			p.detail(fmt.Sprintf("worktree-%d", i))
			p.advance(1)
		}
		p.notice("warning: tail")
		return nil
	})
	var got []progressEvent
	for ev := range stream {
		got = append(got, ev)
	}
	if err := <-workErr; err != nil {
		t.Fatalf("work = %v, want nil", err)
	}
	if want := count*2 + 3; len(got) != want { // phase + pairs + notice + done
		t.Fatalf("stream carried %d events, want %d", len(got), want)
	}
	if got[0].kind != evPhase || got[0].total != count {
		t.Errorf("first event = %+v, want the phase", got[0])
	}
	for i := range count {
		detail, advance := got[1+2*i], got[2+2*i]
		if detail.kind != evDetail || detail.text != fmt.Sprintf("worktree-%d", i) {
			t.Fatalf("event %d = %+v, want detail worktree-%d", 1+2*i, detail, i)
		}
		if advance.kind != evAdvance || advance.n != 1 {
			t.Fatalf("event %d = %+v, want one advance", 2+2*i, advance)
		}
	}
	if got[len(got)-2].kind != evNotice || got[len(got)-1].kind != evDone {
		t.Errorf("stream tail = %+v, %+v, want the notice then done", got[len(got)-2], got[len(got)-1])
	}
}

// TestProgressStreamLosesNothingUnderConcurrentWorkers mirrors the production
// fan-out: every worker's advance must reach the renderer, or the counter would
// under-report the work.
func TestProgressStreamLosesNothingUnderConcurrentWorkers(t *testing.T) {
	const workers, perWorker = 8, 50
	workErr, stream := feedProgress(context.Background(), func(p *progress) error {
		mapParallel(workers*perWorker, workers, func(int) struct{} {
			p.advance(1)
			return struct{}{}
		})
		return nil
	})
	advances := 0
	for ev := range stream {
		if ev.kind == evAdvance {
			advances += ev.n
		}
	}
	if err := <-workErr; err != nil {
		t.Fatalf("work = %v, want nil", err)
	}
	if want := workers * perWorker; advances != want {
		t.Errorf("stream carried %d advances, want %d", advances, want)
	}
}

// TestTransientDelay pins the silence-window resolution: an explicit duration
// wins (tests never depend on the host), else the env seam overrides the
// product default, and a malformed value falls back with a disclosure.
func TestTransientDelay(t *testing.T) {
	t.Setenv(progressDelayEnv, "")
	if got := transientDelay(0); got != progressStartDelay {
		t.Errorf("default delay = %s, want %s", got, progressStartDelay)
	}
	t.Setenv(progressDelayEnv, "3m")
	if got := transientDelay(0); got != 3*time.Minute {
		t.Errorf("env delay = %s, want 3m", got)
	}
	if got := transientDelay(time.Second); got != time.Second {
		t.Errorf("explicit delay = %s, want 1s", got)
	}
	t.Setenv(progressDelayEnv, "soon")
	stderr := captureStderr(t, func() {
		if got := transientDelay(0); got != progressStartDelay {
			t.Errorf("malformed env fell back to %s, want %s", got, progressStartDelay)
		}
	})
	if !strings.Contains(stderr, progressDelayEnv) {
		t.Errorf("malformed env was silent: %q", stderr)
	}
}

// TestNilParentWorkerOnlyQueuesNotices pins the disabled-progress worker
// contract: build workers always receive a handle, so live calls must no-op
// instead of blocking on a nil event stream, while notices still queue for the
// batch owner.
func TestNilParentWorkerOnlyQueuesNotices(t *testing.T) {
	var parent *progress
	var queued []string
	w := parent.worker(&queued)

	done := make(chan struct{})
	go func() {
		defer close(done)
		w.phase("removing worktrees", 1)
		w.advance(1)
		w.detail("wt1")
		w.notice("warning: kept local branch")
		w.done(nil)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("nil-parent worker blocked on a live progress call")
	}
	if len(queued) != 1 || queued[0] != "warning: kept local branch" {
		t.Errorf("queued = %q, want the single notice", queued)
	}
}
