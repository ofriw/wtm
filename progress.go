package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	bubblesprogress "charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
)

// progress.go — inline progress rendering for the slow discovery/removal paths.
// All output is stderr so stdout stays byte-pinned for goldens and --json.

// Progress modes accepted by --progress; "" behaves as auto.
const (
	progressAuto   = "auto"
	progressAlways = "always"
	progressNever  = "never"
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

// progressEvent is the single wire format from worker to renderer. One channel
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
}

// send is the only write path; ctx.Done() is the release valve that keeps a
// send from deadlocking when Run() returns before the worker stops emitting.
func (p *progress) send(ev progressEvent) {
	if p == nil {
		return
	}
	select {
	case p.events <- ev:
	case <-p.ctx.Done():
	}
}

func (p *progress) phase(name string, total int) {
	p.send(progressEvent{kind: evPhase, name: name, total: total})
}

func (p *progress) advance(n int) { p.send(progressEvent{kind: evAdvance, n: n}) }

func (p *progress) detail(text string) {
	p.send(progressEvent{kind: evDetail, text: text})
}

// notice asks the renderer to print a persistent line above the progress block.
// A plain stderr write during the renderer would be overwritten by the next
// frame, which would silently drop a destructive-action disclosure.
func (p *progress) notice(text string) {
	p.send(progressEvent{kind: evNotice, text: text})
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

// withProgress runs work against a live renderer when gating allows, else
// against a no-op handle. enabled is the call site's request for a renderer (gc
// wants one; status only on an explicit --progress always, which overrides
// enabled); progressEnabled still governs the mode/TTY/json gating. It is the
// only place a renderer is started, so call sites stay free of terminal concerns.
func withProgress(g *globals, enabled bool, work func(*progress) error) error {
	return withProgressTo(os.Stderr, g, enabled, work)
}

func runProgressProgram(out io.Writer, work func(*progress) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan progressEvent, 64)
	prog := tea.NewProgram(newProgressModel(events, progressWidth()), progressOptions(out, ctx)...)
	workErr := feedProgress(ctx, events, work)
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

// withProgressTo is withProgress with an injectable renderer output, so tests
// can drive the renderer against a pipe instead of a terminal. The gating's
// TTY check still reads the real stderr: an explicit --progress always is what
// lets a piped output reach the renderer at all.
func withProgressTo(out io.Writer, g *globals, enabled bool, work func(*progress) error) error {
	if (!enabled && g.progress != progressAlways) || !progressEnabled(g.progress, term.IsTerminal(os.Stderr.Fd()), g.json) {
		return work(nil)
	}
	return runProgressProgram(out, work)
}

// progressOptions is the renderer's terminal contract: stderr only, no input
// (a second stdin reader would race the huh forms that run next on the tty),
// and no signal handler (the parent process owns signals).
func progressOptions(out io.Writer, ctx context.Context) []tea.ProgramOption {
	return []tea.ProgramOption{
		tea.WithOutput(out),
		tea.WithInput(nil),
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	}
}

// feedProgress runs work on its own goroutine and closes the event stream when
// it returns, so the renderer quits only after the work is done. The terminal
// done event carries the error so the summary is never a false success.
func feedProgress(ctx context.Context, events chan progressEvent, work func(*progress) error) <-chan error {
	workErr := make(chan error, 1)
	p := &progress{ctx: ctx, events: events}
	go func() {
		defer close(events)
		err := work(p)
		p.done(err)
		workErr <- err
	}()
	return workErr
}

// progressWidth is the render budget derived only from the stderr terminal.
// The last column is reserved: a line of exactly the terminal width leaves the
// cursor in the pending-wrap state, which desynchronises the renderer's cursor
// accounting and splices consecutive frames. Clamping to 120 keeps a wide
// terminal from producing an absurdly long bar.
func progressWidth() int {
	if w, _, err := term.GetSize(os.Stderr.Fd()); err == nil && w > 1 {
		return min(w-1, 120)
	}
	return 80
}

// phaseState drives the icon and bar treatment of one phase row.
type phaseState int

const (
	phaseRunning phaseState = iota
	phaseDone
)

type phaseItem struct {
	name   string
	state  phaseState
	total  int
	done   int
	detail string
}

// progressDoneMsg closes the event stream; it is the model's cue to freeze the
// terminal-state frame and quit.
type progressDoneMsg struct{ err error }

// progressEventMsg adapts a wire event into a tea message.
type progressEventMsg progressEvent

// progressModel renders the phase list inline — AltScreen is never set, so the
// final frame stays in scrollback after the program exits. bubbles' progress
// and spinner models are named fields (not embedded): both types are named
// Model, so embedding both would collide on the field name.
type progressModel struct {
	events   <-chan progressEvent
	spin     spinner.Model
	bar      bubblesprogress.Model
	phases   []*phaseItem
	active   int // running phase index; -1 when none
	width    int
	finished bool
	failed   bool
}

func newProgressModel(events <-chan progressEvent, width int) progressModel {
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
		active: -1,
		width:  max(width, 1),
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
		m.finishRunning()
		m.phases = append(m.phases, &phaseItem{name: ev.name, total: ev.total, state: phaseRunning})
		m.active = len(m.phases) - 1
	case evAdvance:
		if m.active >= 0 {
			m.phases[m.active].done += ev.n
		}
	case evDetail:
		if m.active >= 0 {
			m.phases[m.active].detail = ev.text
		}
	}
}

// finishRunning closes the active phase as done.
func (m *progressModel) finishRunning() {
	if m.active >= 0 && m.phases[m.active].state == phaseRunning {
		m.phases[m.active].state = phaseDone
	}
	m.active = -1
}

func (m *progressModel) finish(err error) {
	m.finishRunning()
	m.finished = true
	m.failed = err != nil
}

func (m progressModel) View() tea.View {
	return tea.NewView(m.render())
}

func (m progressModel) render() string {
	lines := make([]string, 0, len(m.phases)+1)
	for _, ph := range m.phases {
		lines = append(lines, m.phaseLine(ph))
	}
	if summary := m.summaryLine(); summary != "" {
		lines = append(lines, summary)
	}
	return strings.Join(lines, "\n")
}

func (m progressModel) phaseLine(ph *phaseItem) string {
	line := phaseIcon(ph.state, m.spin) + " " + ph.name
	if ph.total > 0 {
		line += fmt.Sprintf("  %d/%d", min(ph.done, ph.total), ph.total)
	}
	if ph.detail != "" {
		line += "  " + ph.detail
	}
	line = m.withBar(line, ph)
	return ansi.Truncate(line, m.width, "")
}

func phaseIcon(state phaseState, spin spinner.Model) string {
	switch state {
	case phaseRunning:
		return spin.View()
	case phaseDone:
		return "✓"
	}
	return "•"
}

// withBar appends a determinate bar to the in-flight phase only: a finished
// phase already reports n/n and ✓, so a 100% bar would freeze into scrollback as
// noise rather than information. A too-narrow line simply omits the bar. ViewAs
// renders the fraction it is handed and the spring is deliberately left unarmed
// (no SetPercent), so Update needs no progress FrameMsg and the bar can never lag
// the counters it reports.
func (m progressModel) withBar(line string, ph *phaseItem) string {
	if ph.total <= 0 || ph.state != phaseRunning {
		return line
	}
	barWidth := m.width - dispWidth(line) - 2
	if barWidth < 4 {
		return line
	}
	// SetWidth only reaches this call's copy of the bar, so ViewAs must stay the
	// next use of m.bar.
	m.bar.SetWidth(barWidth)
	return line + "  " + m.bar.ViewAs(min(1, float64(ph.done)/float64(ph.total)))
}

func (m progressModel) summaryLine() string {
	if !m.finished {
		return ""
	}
	if m.failed {
		return "✗ failed"
	}
	return "✓ done"
}
