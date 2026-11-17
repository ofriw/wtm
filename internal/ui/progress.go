package ui

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

// progress.go — the pure inline progress renderer for the slow discovery/
// removal paths. All output is stderr so stdout stays byte-pinned for goldens
// and --json. The gating entry points (withProgress/withProgressTo) live in
// internal/cli because they take the cli globals.

// Progress modes accepted by --progress; "" behaves as auto.
const (
	ProgressAuto   = "auto"
	ProgressAlways = "always"
	ProgressNever  = "never"
)

// ProgressKind tags a ProgressEvent with the operation it carries.
type ProgressKind int

const (
	EvPhase ProgressKind = iota
	EvAdvance
	EvDetail
	EvNotice
	EvDone
)

// ProgressEvent is the single wire format from worker to renderer. One channel
// carries every operation so the renderer replays them in the worker's order.
type ProgressEvent struct {
	Kind  ProgressKind
	Name  string
	Total int
	N     int
	Text  string
	Err   error
}

// Progress is the worker's nil-safe handle. Every method no-ops on a nil
// receiver, so call sites stay unconditional whether the renderer is running.
type Progress struct {
	ctx    context.Context
	events chan<- ProgressEvent
}

// NewProgress returns a worker handle bound to ctx and the event channel.
func NewProgress(ctx context.Context, events chan<- ProgressEvent) *Progress {
	return &Progress{ctx: ctx, events: events}
}

// send is the only write path; ctx.Done() is the release valve that keeps a
// send from deadlocking when Run() returns before the worker stops emitting.
func (p *Progress) send(ev ProgressEvent) {
	if p == nil {
		return
	}
	select {
	case p.events <- ev:
	case <-p.ctx.Done():
	}
}

// Phase announces a new phase with its total step count.
func (p *Progress) Phase(name string, total int) {
	p.send(ProgressEvent{Kind: EvPhase, Name: name, Total: total})
}

// Advance advances the current phase by n steps.
func (p *Progress) Advance(n int) { p.send(ProgressEvent{Kind: EvAdvance, N: n}) }

// Detail updates the current phase's detail line.
func (p *Progress) Detail(text string) {
	p.send(ProgressEvent{Kind: EvDetail, Text: text})
}

// Notice asks the renderer to print a persistent line above the progress block.
// A plain stderr write during the renderer would be overwritten by the next
// frame, which would silently drop a destructive-action disclosure.
func (p *Progress) Notice(text string) {
	p.send(ProgressEvent{Kind: EvNotice, Text: text})
}

// EmitProgress writes one persistent line: as a renderer notice while one runs
// (a raw stderr write would be erased by the next frame), else a plain stderr
// line. It is the single write path for both notices and warnings.
func EmitProgress(p *Progress, text string) {
	if p != nil {
		p.Notice(text)
		return
	}
	fmt.Fprintln(os.Stderr, text)
}

// NoticeProgress discloses a routine action (e.g. a destructive step about to
// run) that must survive the renderer.
func NoticeProgress(p *Progress, format string, a ...any) {
	EmitProgress(p, fmt.Sprintf(format, a...))
}

// WarnProgress discloses a non-fatal warning, composing the label owned by
// WarnPrefix so renderer and plain-stderr warnings cannot drift.
func WarnProgress(p *Progress, format string, a ...any) {
	if p == nil {
		Warnf(format, a...)
		return
	}
	p.Notice(WarnPrefix + fmt.Sprintf(format, a...))
}

// Done is the terminal event: it lets the renderer show a truthful summary even
// when work failed outside any phase (e.g. a scan error), and it is what
// releases the renderer to quit.
func (p *Progress) Done(err error) {
	p.send(ProgressEvent{Kind: EvDone, Err: err})
}

// ProgressEnabled is the single source of truth for --progress precedence: auto
// stays silent for machine output, non-TTY stderr and a dumb terminal.
func ProgressEnabled(mode string, stderrIsTTY, json bool) bool {
	switch mode {
	case ProgressAlways:
		return true
	case ProgressNever:
		return false
	}
	return !json && stderrIsTTY && os.Getenv("TERM") != "dumb"
}

// RunMaybeProgress runs work against a live renderer when the gating allows,
// else against a no-op handle. enabled is the call site's request for a
// renderer (gc wants one; status only on an explicit --progress always, which
// overrides enabled); ProgressEnabled still governs the mode/TTY/json gating.
// It is the only place a renderer is started, so call sites stay free of
// terminal concerns.
func RunMaybeProgress(out io.Writer, mode string, json, enabled bool, work func(*Progress) error) error {
	if (!enabled && mode != ProgressAlways) || !ProgressEnabled(mode, term.IsTerminal(os.Stderr.Fd()), json) {
		return work(nil)
	}
	return RunProgressProgram(out, work)
}

// RunProgressProgram runs work against a live renderer. progressOptions is the
// renderer's terminal contract: stderr only, no input (a second stdin reader
// would race the huh forms that run next on the tty), and no signal handler
// (the parent process owns signals).
func RunProgressProgram(out io.Writer, work func(*Progress) error) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan ProgressEvent, 64)
	prog := tea.NewProgram(newProgressModel(events, progressWidth()), progressOptions(out, ctx)...)
	workErr := FeedProgress(ctx, events, work)
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

func progressOptions(out io.Writer, ctx context.Context) []tea.ProgramOption {
	return []tea.ProgramOption{
		tea.WithOutput(out),
		tea.WithInput(nil),
		tea.WithContext(ctx),
		tea.WithoutSignalHandler(),
	}
}

// FeedProgress runs work on its own goroutine and closes the event stream when
// it returns, so the renderer quits only after the work is done. The terminal
// done event carries the error so the summary is never a false success.
func FeedProgress(ctx context.Context, events chan ProgressEvent, work func(*Progress) error) <-chan error {
	workErr := make(chan error, 1)
	p := &Progress{ctx: ctx, events: events}
	go func() {
		defer close(events)
		err := work(p)
		p.Done(err)
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
type progressEventMsg ProgressEvent

// progressModel renders the phase list inline — AltScreen is never set, so the
// final frame stays in scrollback after the program exits. bubbles' progress
// and spinner models are named fields (not embedded): both types are named
// Model, so embedding both would collide on the field name.
type progressModel struct {
	events   <-chan ProgressEvent
	spin     spinner.Model
	bar      bubblesprogress.Model
	phases   []*phaseItem
	active   int // running phase index; -1 when none
	width    int
	finished bool
	failed   bool
}

func newProgressModel(events <-chan ProgressEvent, width int) progressModel {
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
		return m.updateEvent(ProgressEvent(msg))
	case spinner.TickMsg:
		var cmd tea.Cmd
		m.spin, cmd = m.spin.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m progressModel) updateEvent(ev ProgressEvent) (tea.Model, tea.Cmd) {
	switch ev.Kind {
	case EvDone:
		m.finish(ev.Err)
		return m, tea.Quit
	case EvNotice:
		return m, tea.Batch(tea.Println(ev.Text), m.nextEvent)
	}
	m.apply(ev)
	return m, m.nextEvent
}

func (m *progressModel) apply(ev ProgressEvent) {
	switch ev.Kind {
	case EvPhase:
		m.finishRunning()
		m.phases = append(m.phases, &phaseItem{name: ev.Name, total: ev.Total, state: phaseRunning})
		m.active = len(m.phases) - 1
	case EvAdvance:
		if m.active >= 0 {
			m.phases[m.active].done += ev.N
		}
	case EvDetail:
		if m.active >= 0 {
			m.phases[m.active].detail = ev.Text
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
	barWidth := m.width - DispWidth(line) - 2
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
