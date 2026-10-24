package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	bubblesprogress "charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// progress.go — one-line progress rendering for the slow discovery/removal
// paths. All output is stderr so stdout stays byte-pinned for goldens and
// --json. The line is rewritten in place: phase i+1 replaces phase i, because a
// scrolling phase history pushes the product (the table) off-screen.

// Progress modes accepted by --progress; "" behaves as auto.
const (
	progressAuto   = "auto"
	progressAlways = "always"
	progressNever  = "never"
)

const (
	// progressStartDelay keeps a fast run silent. Below it a frame is flicker,
	// not feedback; above it the user is waiting and needs liveness (clig.dev).
	progressStartDelay = 200 * time.Millisecond
	// progressMaxBar is the widest bar worth drawing; beyond it the bar stops
	// reading as a proportion and starts eating the label.
	progressMaxBar = 40
	// progressMinBar is the narrowest bar that still reads as a bar. A narrower
	// budget drops the bar and keeps the counters.
	progressMinBar = 8
)

// progressKind tags a progressEvent with the operation it carries.
type progressKind int

const (
	evPhase progressKind = iota
	evAdvance
	evDetail
	evNotice
	evDone
)

// progressEvent is the single wire format from worker to renderer. One stream
// carries every operation so the renderer replays them in the worker's order.
type progressEvent struct {
	kind  progressKind
	name  string
	total int
	n     int
	text  string
	err   error
}

// progress is the worker's nil-safe handle. Every method no-ops on a nil
// receiver, so call sites stay unconditional whether the renderer is running.
type progress struct {
	ctx    context.Context
	events chan<- progressEvent
	// queued captures a parallel worker's disclosures. The batch owner replays
	// them in input order after the join, so scheduling cannot reorder output.
	queued *[]string
}

// send is the only write path. It never waits on the renderer because
// relayProgress absorbs the backlog; ctx.Done() remains the release valve for a
// send that outlives the renderer's cancel.
func (p *progress) send(ev progressEvent) {
	// A worker from a nil parent has no renderer stream; its only write is the
	// queued notice path, so live events must no-op instead of blocking on nil
	// channels.
	if p == nil || p.events == nil {
		return
	}
	select {
	case p.events <- ev:
	case <-p.ctx.Done():
	}
}

// phase starts a new step: it replaces the label, the counter and the bar.
func (p *progress) phase(name string, total int) {
	p.send(progressEvent{kind: evPhase, name: name, total: total})
}

func (p *progress) advance(n int) { p.send(progressEvent{kind: evAdvance, n: n}) }

// detail replaces the step text beside the label (the current unit of work).
func (p *progress) detail(text string) {
	p.send(progressEvent{kind: evDetail, text: text})
}

// notice asks the renderer to print a persistent line above the progress line.
// A plain stderr write during the renderer would be overwritten by the next
// frame, which would silently drop a destructive-action disclosure.
func (p *progress) notice(text string) {
	if p == nil {
		return
	}
	if p.queued != nil {
		*p.queued = append(*p.queued, text)
		return
	}
	p.send(progressEvent{kind: evNotice, text: text})
}

// worker returns a handle for use inside a mapParallel batch. It queues
// disclosures in sink instead of the shared stream, so the batch owner can
// emit them in input order. Live progress events still reach the renderer. A
// nil parent yields a queue-only handle: live events no-op, but notices still
// queue, keeping warnings ordered when progress is disabled.
func (p *progress) worker(sink *[]string) *progress {
	w := &progress{queued: sink}
	if p != nil {
		w.ctx, w.events = p.ctx, p.events
	}
	return w
}

// emitProgress writes one persistent line: as a renderer notice while one runs
// (a raw stderr write would be erased by the next frame), else a plain stderr
// line. It is the single write path for both notices and warnings.
func emitProgress(p *progress, text string) {
	if p != nil {
		p.notice(text)
		return
	}
	fmt.Fprintln(os.Stderr, text)
}

// noticeProgress discloses a routine action (e.g. a destructive step about to
// run) that must survive the renderer.
func noticeProgress(p *progress, format string, a ...any) {
	emitProgress(p, fmt.Sprintf(format, a...))
}

// warnProgress discloses a non-fatal warning, composing the label owned by
// warnPrefix so renderer and plain-stderr warnings cannot drift.
func warnProgress(p *progress, format string, a ...any) {
	if p == nil {
		warnf(format, a...)
		return
	}
	p.notice(warnPrefix + fmt.Sprintf(format, a...))
}

// done is the terminal event: it lets the renderer show a truthful summary even
// when work failed outside any phase (e.g. a scan error), and it is what
// releases the renderer to quit.
func (p *progress) done(err error) {
	p.send(progressEvent{kind: evDone, err: err})
}

// validateProgress rejects an unknown --progress value so it can never silently
// fall back to auto.
func validateProgress(mode string) error {
	switch mode {
	case "", progressAuto, progressAlways, progressNever:
		return nil
	}
	return usagef("--progress: want auto, always or never, got %q", mode)
}

// progressEnabled is the single source of truth for --progress precedence: auto
// stays silent for machine output, non-TTY stderr and a dumb terminal.
func progressEnabled(mode string, stderrIsTTY, json bool) bool {
	switch mode {
	case progressAlways:
		return true
	case progressNever:
		return false
	}
	return !json && stderrIsTTY && os.Getenv("TERM") != "dumb"
}

// progressDelayEnv pins the transient silence window for end-to-end tests: the
// product default is wall-clock bound, so a subprocess test cannot otherwise
// assert silence deterministically. WTM_NOW is the precedent for such a seam.
const progressDelayEnv = "WTM_PROGRESS_DELAY"

// transientDelay resolves the silence window. An explicit delay wins, so unit
// tests never depend on the host environment; otherwise the env seam replaces
// the product default. An unparsable value falls back and discloses, like
// WTM_NOW.
func transientDelay(explicit time.Duration) time.Duration {
	if explicit > 0 {
		return explicit
	}
	raw := os.Getenv(progressDelayEnv)
	if raw == "" {
		return progressStartDelay
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		warnf("%s %q is not a duration: using %s", progressDelayEnv, raw, progressStartDelay)
		return progressStartDelay
	}
	return d
}

// progressOptions shapes one renderer run.
type progressOptions struct {
	// transient erases the line on exit so the command's own output follows a
	// clean terminal, and lets a run that beats delay stay silent. gc/delete
	// keep the line: it is their only summary.
	transient bool
	// delay is the orchestration-side silence window resolved by transientDelay;
	// the model never sees it.
	delay time.Duration
}

// withProgress runs work against a renderer on stderr when gating allows, else
// against a no-op handle. It is the only place a renderer is started, so call
// sites stay free of terminal concerns.
func withProgress(g *globals, opts progressOptions, work func(*progress) error) error {
	return withProgressTo(os.Stderr, g, opts, work)
}

// withProgressTo is withProgress with an injectable output, so tests drive the
// renderer against a pipe or a pty. The gating's TTY check still reads the real
// stderr: an explicit --progress always is what lets a piped output render.
func withProgressTo(out io.Writer, g *globals, opts progressOptions, work func(*progress) error) error {
	if !progressEnabled(g.progress, term.IsTerminal(os.Stderr.Fd()), g.json) {
		return work(nil)
	}
	if !writerIsTerminal(out) {
		return runPlainProgress(out, work)
	}
	// Only a transient run suppresses its first frames: status must stay silent
	// when it is fast, while gc/delete always owe the user their summary. An
	// explicit --progress always is a request to see every phase at once, so it
	// keeps its zero delay.
	if opts.transient {
		opts.delay = transientDelay(opts.delay)
	}
	return runProgressProgram(out, opts, work)
}

// runPlainProgress is the non-TTY renderer: one plain line per transition, no
// escape bytes. A redirected `--progress always` must stay readable in a log,
// and the animated renderer writes cursor codes into the file.
func runPlainProgress(out io.Writer, work func(*progress) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workErr, events := feedProgress(ctx, work)
	started := time.Now()
	for ev := range events {
		switch ev.kind {
		case evPhase:
			_, _ = fmt.Fprintf(out, "start %s\n", ev.name)
		case evNotice:
			_, _ = fmt.Fprintln(out, ev.text)
		case evDone:
			if ev.err != nil {
				_, _ = fmt.Fprintln(out, "✗ failed")
			} else {
				_, _ = fmt.Fprintf(out, "✓ done in %s\n", elapsed(started))
			}
		}
	}
	return <-workErr
}

// runProgressProgram starts the renderer only after the delay window closes: a
// run that finishes inside it writes no frame, so a fast status leaves stderr
// clean instead of flashing a frame and a stray newline. Events emitted before
// the window closes queue in the relay, so the delay costs memory, never work
// time; notices still print because a disclosure must not depend on speed.
func runProgressProgram(out io.Writer, opts progressOptions, work func(*progress) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	workErr, events := feedProgress(ctx, work)
	if opts.delay > 0 {
		select {
		case err := <-workErr:
			drainSilentRun(out, events)
			return err
		case <-time.After(opts.delay):
		}
	}
	prog := tea.NewProgram(newProgressModel(events, progressWidth(out), opts), teaOptions(out, ctx)...)
	_, runErr := prog.Run()
	cancel()
	if err := <-workErr; err != nil {
		if runErr != nil {
			return fmt.Errorf("%w; renderer: %w", err, runErr)
		}
		return err
	}
	return runErr
}

// drainSilentRun ends a run that finished inside the silence window. Frames are
// dropped, but a notice is a disclosure: dropping it would make warnings depend
// on machine speed.
func drainSilentRun(out io.Writer, events <-chan progressEvent) {
	for ev := range events {
		if ev.kind == evNotice {
			_, _ = fmt.Fprintln(out, ev.text)
		}
	}
}

// teaOptions is the renderer's terminal contract: the given output only, no
// input (a second stdin reader would race the huh forms that run next on the
// tty), and no signal handler (the parent process owns signals).
func teaOptions(out io.Writer, ctx context.Context) []tea.ProgramOption {
	return []tea.ProgramOption{
		tea.WithOutput(out),
		tea.WithInput(nil),
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	}
}

// feedProgress runs work on its own goroutine and closes the event stream when
// it returns, so the renderer quits only after the work is done. The terminal
// done event carries the error so the summary is never a false success. The
// returned stream is the relay's, so a renderer that has not started yet can
// never block the work.
func feedProgress(ctx context.Context, work func(*progress) error) (<-chan error, <-chan progressEvent) {
	workErr := make(chan error, 1)
	inbox := make(chan progressEvent, 64)
	p := &progress{ctx: ctx, events: inbox}
	go func() {
		defer close(inbox)
		err := work(p)
		p.done(err)
		workErr <- err
	}()
	return workErr, relayProgress(ctx, inbox)
}

// relayProgress decouples worker sends from the renderer. The renderer starts
// only after the silence window closes and can run slower than the workers, so
// a bounded hand-off would turn render latency into work latency: once the
// buffer filled, a worker would block, the run would stop being fast, and the
// product would paint the frame it promised to suppress. The backlog is bounded
// by the work itself (a few events per worktree), so it needs no cap. The
// context cancels an abandoned relay once its renderer exits, so a queued
// backlog cannot outlive the run.
func relayProgress(ctx context.Context, in <-chan progressEvent) <-chan progressEvent {
	out := make(chan progressEvent)
	go func() {
		defer close(out)
		var backlog []progressEvent
		open := true
		for open || len(backlog) > 0 {
			var recv <-chan progressEvent
			if open {
				recv = in
			}
			var send chan<- progressEvent
			var head progressEvent
			if len(backlog) > 0 {
				send, head = out, backlog[0]
			}
			select {
			case <-ctx.Done():
				return
			case ev, ok := <-recv:
				if !ok {
					open = false
					continue
				}
				backlog = append(backlog, ev)
			case send <- head:
				backlog = backlog[1:]
			}
		}
	}()
	return out
}

// progressWidth is the render budget derived only from the renderer's output.
// The last column is reserved: a line of exactly the terminal width leaves the
// cursor in the pending-wrap state, which desynchronises the renderer's cursor
// accounting and splices consecutive frames. Clamping to 120 keeps a wide
// terminal from producing an absurdly long bar.
func progressWidth(out io.Writer) int {
	if f, ok := out.(interface{ Fd() uintptr }); ok {
		if w, _, err := term.GetSize(f.Fd()); err == nil && w > 1 {
			return min(w-1, 120)
		}
	}
	return 80
}

// writerIsTerminal decides animation vs plain lines. A pipe cannot be redrawn.
func writerIsTerminal(out io.Writer) bool {
	f, ok := out.(interface{ Fd() uintptr })
	return ok && term.IsTerminal(f.Fd())
}

// elapsed renders a run duration at a resolution a human can read.
func elapsed(start time.Time) string {
	return time.Since(start).Round(100 * time.Millisecond).String()
}

// progressDoneMsg closes the event stream; it is the model's cue to freeze the
// terminal-state line and quit.
type progressDoneMsg struct{ err error }

// progressEventMsg adapts a wire event into a tea message.
type progressEventMsg progressEvent

// progressModel renders exactly one line: icon, optional bar, counter, label and
// step. AltScreen is never set, so a non-transient final line stays in
// scrollback after the program exits. bubbles' progress and spinner models are
// named fields (not embedded): both types are named Model, so embedding both
// would collide on the field name.
type progressModel struct {
	events   <-chan progressEvent
	spin     spinner.Model
	bar      bubblesprogress.Model
	opts     progressOptions
	label    string
	step     string
	n        int
	total    int
	started  time.Time
	finished bool
	failed   bool
	width    int
}

func newProgressModel(events <-chan progressEvent, width int, opts progressOptions) progressModel {
	return progressModel{
		events: events,
		spin:   spinner.New(spinner.WithSpinner(spinner.MiniDot)),
		bar: bubblesprogress.New(
			bubblesprogress.WithoutPercentage(),
			// bubbles' default fill is a half block (▌), which stops reading as a bar
			// once it spans the whole budget; full blocks stay contiguous.
			bubblesprogress.WithFillCharacters(
				bubblesprogress.DefaultFullCharFullBlock,
				bubblesprogress.DefaultEmptyCharBlock,
			),
		),
		opts:    opts,
		started: time.Now(),
		width:   max(width, 1),
	}
}

func (m progressModel) Init() tea.Cmd {
	return tea.Batch(m.spin.Tick, m.nextEvent)
}

// nextEvent is a command, so the blocking channel read never stalls the UI
// loop; each processed event arms the next read.
func (m progressModel) nextEvent() tea.Msg {
	ev, ok := <-m.events
	if !ok {
		return progressDoneMsg{err: errors.New("progress stream ended unexpectedly")}
	}
	return progressEventMsg(ev)
}

func (m progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case progressDoneMsg:
		m.finish(msg.err)
		return m, tea.Quit
	case progressEventMsg:
		return m.updateEvent(progressEvent(msg))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m progressModel) updateEvent(ev progressEvent) (tea.Model, tea.Cmd) {
	switch ev.kind {
	case evDone:
		m.finish(ev.err)
		return m, tea.Quit
	case evNotice:
		return m, tea.Batch(tea.Println(ev.text), m.nextEvent)
	}
	m.apply(ev)
	return m, m.nextEvent
}

func (m *progressModel) apply(ev progressEvent) {
	switch ev.kind {
	case evPhase:
		m.label, m.step, m.n, m.total = ev.name, "", 0, ev.total
	case evAdvance:
		m.n += ev.n
	case evDetail:
		m.step = ev.text
	}
}

func (m *progressModel) finish(err error) {
	m.finished = true
	m.failed = err != nil
}

func (m progressModel) View() tea.View {
	return tea.NewView(m.render())
}

// render is empty in transient mode once the run ends: an empty frame is how
// the renderer clears the line before the command's own output.
func (m progressModel) render() string {
	if m.finished && m.opts.transient {
		return ""
	}
	return ansi.Truncate(m.frame(), m.width, "")
}

// frame lays out the single line. The bar only exists for a known total while
// the run is live: after the run it would freeze into scrollback as noise.
func (m progressModel) frame() string {
	icon := m.icon()
	text := m.labelText()
	if m.total <= 0 || m.finished {
		return strings.TrimSpace(icon + " " + text)
	}
	bar := m.barView(icon, text)
	return strings.TrimSpace(icon + " " + bar + " " + text)
}

func (m progressModel) icon() string {
	switch {
	case m.failed:
		return "✗"
	case m.finished:
		return "✓"
	}
	return m.spin.View()
}

// labelText composes the words: label · step · counter, dropping the empty
// parts so short phases do not render dangling separators.
func (m progressModel) labelText() string {
	if m.finished {
		if m.failed {
			return "failed"
		}
		return "done in " + elapsed(m.started)
	}
	parts := make([]string, 0, 3)
	if m.label != "" {
		parts = append(parts, m.label)
	}
	if m.step != "" {
		parts = append(parts, m.step)
	}
	if m.total > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d", min(m.n, m.total), m.total))
	}
	return strings.Join(parts, " · ")
}

// barView renders the bar and its surrounding spaces, or nothing when the
// remaining budget is too small to read. SetWidth only reaches this call's copy
// of the bar, so ViewAs must stay the next use of m.bar.
func (m progressModel) barView(icon, text string) string {
	budget := m.width - dispWidth(icon) - dispWidth(text) - 3
	if budget < progressMinBar {
		return ""
	}
	m.bar.SetWidth(min(budget, progressMaxBar))
	return m.bar.ViewAs(min(1, float64(m.n)/float64(m.total)))
}
