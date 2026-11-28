package ui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

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
		{"always tty", ProgressAlways, true, false, "xterm", true},
		{"always non-tty", ProgressAlways, false, false, "xterm", true},
		{"always json", ProgressAlways, true, true, "xterm", true},
		{"never tty", ProgressNever, true, false, "xterm", false},
		{"never non-tty", ProgressNever, false, false, "xterm", false},
		{"auto tty", ProgressAuto, true, false, "xterm", true},
		{"auto empty mode", "", true, false, "xterm", true},
		{"auto non-tty", ProgressAuto, false, false, "xterm", false},
		{"auto json", ProgressAuto, true, true, "xterm", false},
		{"auto dumb", ProgressAuto, true, false, "dumb", false},
		{"auto empty term", ProgressAuto, true, false, "", true},
		{"auto non-tty dumb", ProgressAuto, false, false, "dumb", false},
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

// TestProgressModelSummary pins the terminal frame: a clean run says done, and
// a work error surfaced only via the terminal event still reports failure.
func TestProgressModelSummary(t *testing.T) {
	m := newProgressModel(nil, 40)
	m.apply(ProgressEvent{Kind: EvPhase, Name: "scanning", Total: 2})
	m.apply(ProgressEvent{Kind: EvAdvance, N: 2})
	m.finish(nil)
	if got := m.render(); !strings.Contains(got, "✓ done") {
		t.Errorf("render = %q, want the done summary", got)
	}

	m = newProgressModel(nil, 40)
	m.finish(errors.New("boom"))
	if got := m.render(); !strings.Contains(got, "✗ failed") {
		t.Errorf("render = %q, want the failed summary", got)
	}
	m2 := newProgressModel(nil, 40)
	updated, _ := m2.Update(progressDoneMsg{err: errors.New("panic")})
	if got := updated.(progressModel).render(); !strings.Contains(got, "✗ failed") {
		t.Errorf("render after failed done = %q, want the failed summary", got)
	}

}

// TestProgressLineFitsWidth pins the truncation contract: a long detail cannot
// push any rendered line past the budget, even with a bar and a total.
func TestProgressLineFitsWidth(t *testing.T) {
	m := newProgressModel(nil, 20)
	m.apply(ProgressEvent{Kind: EvPhase, Name: "scanning worktrees", Total: 100})
	m.apply(ProgressEvent{Kind: EvAdvance, N: 50})
	m.apply(ProgressEvent{Kind: EvDetail, Text: "~/a/very/long/path/that/exceeds/the/budget"})
	for _, line := range strings.Split(m.render(), "\n") {
		if w := DispWidth(line); w > 20 {
			t.Errorf("line is %d cells, exceeds 20: %q", w, line)
		}
	}
}

// TestProgressBarOnlyWhileRunning pins the bar contract: only an in-flight phase
// carries one, its cells are full blocks over light shades, and a finished phase
// carries none — a 100% bar frozen into the scrollback frame is noise, and
// bubbles' default half-block fill (▌) reads as a fence rather than a bar.
func TestProgressBarOnlyWhileRunning(t *testing.T) {
	m := newProgressModel(nil, 40)
	m.apply(ProgressEvent{Kind: EvPhase, Name: "scanning", Total: 4})
	m.apply(ProgressEvent{Kind: EvAdvance, N: 2})
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
	m := newProgressModel(nil, 40)
	m.apply(ProgressEvent{Kind: EvPhase, Name: "removing", Total: 2})
	m.apply(ProgressEvent{Kind: EvAdvance, N: 1})
	m.finish(errors.New("worktree is locked"))
	frame := m.render()
	for _, want := range []string{"removing", "✗ failed"} {
		if !strings.Contains(frame, want) {
			t.Errorf("failed frame is missing %q: %q", want, frame)
		}
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

// TestProgressNoticePrintsPinnedLine pins the notice contract: EvNotice must
// become a persistent print (tea.Println) — a frame write would be overwritten
// by the next frame and silently drop a destructive-action disclosure. It also
// re-arms the event loop, and the frame itself stays untouched.
func TestProgressNoticePrintsPinnedLine(t *testing.T) {
	events := make(chan ProgressEvent, 1)
	// nextEvent blocks on this channel; feeding one event lets the drained
	// command return instead of deadlocking the test.
	events <- ProgressEvent{Kind: EvAdvance, N: 1}
	m := newProgressModel(events, 40)
	const notice = "will delete remote origin/feat"
	_, cmd := m.Update(progressEventMsg{Kind: EvNotice, Text: notice})
	if cmd == nil {
		t.Fatal("EvNotice produced no command; the disclosure would be dropped")
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

// TestProgressDoneFinishes pins the done wire-format kind: an EvDone event
// ends the renderer, so the exported kind set is behaviorally part of the
// ProgressEvent wire format.
func TestProgressDoneFinishes(t *testing.T) {
	m := newProgressModel(make(chan ProgressEvent, 1), 40)
	updated, cmd := m.Update(progressEventMsg{Kind: EvDone})
	if cmd == nil {
		t.Fatal("EvDone produced no command")
	}
	if !updated.(progressModel).finished {
		t.Error("EvDone did not finish the progress renderer")
	}
}
