package main

import (
	"fmt"
	"slices"
	"sort"
	"time"
)

// usage.go — the cost/active-time domain shared by every harness. Parsers turn
// session records into usageEvent values; addSession folds those into a
// worktreeUsage. This file owns the folding, the money/span model, the
// cross-file dedupe and the display grammar; the session loader that drives
// addSession (buildUsage) lives with the harness registry in agents.go.

// money accumulates a cost while keeping "unknown" sticky: a total is unknown
// when any contributor has no price. A real zero stays a known zero, because
// the harness (or the catalog) priced it.
type money struct {
	amount  float64
	unknown bool
	any     bool
}

func (m *money) addKnown(v float64) {
	m.any = true
	m.amount += v
}

func (m *money) addUnknown() {
	m.any = true
	m.unknown = true
}

// add folds another accumulator in; an empty source is a no-op.
func (m *money) add(o money) {
	if !o.any {
		return
	}
	m.any = true
	if o.unknown {
		m.unknown = true
		return
	}
	m.amount += o.amount
}

// jsonValue is the machine contract: null when absent or unknown.
func (m money) jsonValue() *float64 {
	if !m.any || m.unknown {
		return nil
	}
	v := roundCost(m.amount)
	return &v
}

// cell is the table contract: "-" absent, "unknown" unpriced, else "$d.dd".
func (m money) cell() string {
	switch {
	case !m.any:
		return "-"
	case m.unknown:
		return "unknown"
	default:
		return formatCost(m.amount)
	}
}

// roundCost kills float summation noise so JSON output is stable; 1e-6 is well
// below a cent and above the error of summing real token prices.
func roundCost(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }

func formatCost(v float64) string { return fmt.Sprintf("$%.2f", roundCost(v)) }

// usageKey is the cheap pre-filter shared by the parsers: a session line without
// it cannot carry cost or tokens, so content-heavy user, progress and tool-result
// lines are never unmarshalled.
var usageKey = []byte("\"usage\"")

// claudeTokens are the four priced Claude Code token classes. The harness keeps
// the 5-minute and 1-hour cache writes apart; unclassifiedCacheWrite is the
// flat cache_creation count when the record omits the lifetime breakdown.
type claudeTokens struct {
	input                  int64
	output                 int64
	cacheRead              int64
	cacheWrite5m           int64
	cacheWrite1h           int64
	unclassifiedCacheWrite int64
}

// context is the request's input size, used for the long-context rate.
func (t claudeTokens) context() int64 {
	return t.input + t.cacheRead + t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
}

// usageEvent is one parsed cost/time record before catalog pricing. A harness
// that prices its own records (Pi) sets cost; a token-only harness (Claude)
// leaves cost nil and fills tokens/tier/webSearch. end is set only for a spawn
// child, whose running time spans its tool call to its result; a zero end is a
// point event. key is the billed-turn identity (Claude: message id + request
// id); a non-empty key dedupes across a worktree's sessions, because a resumed
// or forked transcript repeats earlier messages.
type usageEvent struct {
	at        time.Time
	end       time.Time
	key       string
	model     string
	provider  string // Pi provider id; empty for Claude (never dual-mode)
	cost      *float64
	tokens    claudeTokens
	tier      string
	webSearch int64
}

// endAt is the event's trailing instant: its span end when set, else its point.
func (e usageEvent) endAt() time.Time {
	if e.end.After(e.at) {
		return e.end
	}
	return e.at
}

// span is one half-open activity interval.
type span struct{ start, end time.Time }

// unionDuration merges overlapping spans and returns the covered time. A span
// with one event has zero length (its start equals its end).
func unionDuration(spans []span) time.Duration {
	if len(spans) == 0 {
		return 0
	}
	sorted := slices.Clone(spans)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	total := time.Duration(0)
	cur := sorted[0]
	for _, s := range sorted[1:] {
		if s.start.After(cur.end) {
			total += cur.end.Sub(cur.start)
			cur = s
			continue
		}
		if s.end.After(cur.end) {
			cur.end = s.end
		}
	}
	return total + cur.end.Sub(cur.start)
}

// modelUsage is one model's totals inside a harness; spans are per-session
// model spans, merged for the active time.
type modelUsage struct {
	cost  money
	spans []span
}

func (m *modelUsage) active() time.Duration { return unionDuration(m.spans) }

// harnessUsage is one harness's totals for a worktree.
type harnessUsage struct {
	cost   money
	spans  []span // session spans (all events)
	models map[string]*modelUsage
}

func (h *harnessUsage) active() time.Duration { return unionDuration(h.spans) }

// worktreeUsage is every harness's totals for one worktree. A worktree with no
// events has no harnesses, which is the "no events" signal everywhere. partial
// marks a total that lost an unreadable session, so it never reads as complete.
type worktreeUsage struct {
	harnesses map[string]*harnessUsage
	partial   bool
}

func (u *worktreeUsage) hasEvents() bool { return len(u.harnesses) > 0 }

func (u *worktreeUsage) cost() money {
	var m money
	for _, h := range u.harnesses {
		m.add(h.cost)
	}
	// A harness whose every session was unreadable keeps no entry (see
	// markPartial), so the worktree-level flag must taint the total itself.
	if u.partial {
		m.addUnknown()
	}
	return m
}

func (u *worktreeUsage) active() time.Duration {
	var spans []span
	for _, h := range u.harnesses {
		spans = append(spans, h.spans...)
	}
	return unionDuration(spans)
}

// harness returns (creating if needed) the accumulator for one harness.
func (u *worktreeUsage) harness(name string) *harnessUsage {
	if u.harnesses == nil {
		u.harnesses = map[string]*harnessUsage{}
	}
	h := u.harnesses[name]
	if h == nil {
		h = &harnessUsage{models: map[string]*modelUsage{}}
		u.harnesses[name] = h
	}
	return h
}

// addSession folds one session's events into the worktree: the session span for
// the harness, each model's span inside the session, and every cost.
func (u *worktreeUsage) addSession(harness string, events []usageEvent, price func(usageEvent) money) {
	if len(events) == 0 {
		return
	}
	h := u.harness(harness)
	session, perModel := foldSession(events, h, price)
	h.spans = append(h.spans, session)
	for name, m := range perModel {
		h.model(name).absorb(m)
	}
}

// foldSession accumulates one session's harness cost and returns the session
// span plus each model's per-session spans and cost.
func foldSession(events []usageEvent, h *harnessUsage, price func(usageEvent) money) (span, map[string]*modelUsage) {
	session := span{start: events[0].at, end: events[0].at}
	perModel := map[string]*modelUsage{}
	prev := ""
	for _, e := range events {
		session.start = minTime(session.start, e.at)
		session.end = maxTime(session.end, e.endAt())
		prev = foldEvent(perModel, h, e, prev, price)
	}
	return session, perModel
}

// foldEvent extends one model's open run and both cost accumulators. prev is
// the previous event's model: consecutive same-model events chain into one run
// (first→last event), a different model closes the run, so an interleaved
// model never inherits another model's idle time. An isolated event is a point.
func foldEvent(perModel map[string]*modelUsage, h *harnessUsage, e usageEvent, prev string, price func(usageEvent) money) string {
	m := perModel[e.model]
	if m == nil {
		m = &modelUsage{}
		perModel[e.model] = m
	}
	if prev == e.model && len(m.spans) > 0 {
		last := &m.spans[len(m.spans)-1]
		last.start = minTime(last.start, e.at)
		last.end = maxTime(last.end, e.endAt())
	} else {
		m.spans = append(m.spans, span{start: e.at, end: e.endAt()})
	}
	c := price(e)
	m.cost.add(c)
	h.cost.add(c)
	return e.model
}

// dedupeKeyedEvents collapses repeated billed turns across a worktree's
// sessions. The same billed record (same key) can appear in two
// session files, so it must price once; the largest token total wins, because a
// partial stream undercounts. Ties keep the first occurrence. Keyless events
// pass through untouched. Winners compare by billed size, never by struct
// equality: cost pointers compare by identity and times by location, so an
// equal-looking repeat would otherwise drop every copy. Each kept session stays
// a subsequence of its time-sorted input, so folding order is preserved.
// Results are read from the passed slice: dedupeKeyedEvents replaces its inner
// slices, so a caller-held event slice cannot observe the change.
func dedupeKeyedEvents(sessions [][]usageEvent) {
	best := map[string]int64{}
	for _, events := range sessions {
		for _, e := range events {
			if e.key == "" {
				continue
			}
			if w, ok := best[e.key]; !ok || e.tokens.total() > w {
				best[e.key] = e.tokens.total()
			}
		}
	}
	for si, events := range sessions {
		kept := make([]usageEvent, 0, len(events))
		for _, e := range events {
			if e.key == "" {
				kept = append(kept, e)
				continue
			}
			// Keep only the largest record; delete its key so equal
			// duplicates later in the walk drop too.
			if w, ok := best[e.key]; ok && e.tokens.total() == w {
				kept = append(kept, e)
				delete(best, e.key)
			}
		}
		sessions[si] = kept
	}
}

// model returns the harness's accumulator for one model, creating it on demand.
func (h *harnessUsage) model(name string) *modelUsage {
	m := h.models[name]
	if m == nil {
		m = &modelUsage{}
		h.models[name] = m
	}
	return m
}

// absorb folds a per-session model accumulator into the worktree-wide one.
func (m *modelUsage) absorb(o *modelUsage) {
	m.spans = append(m.spans, o.spans...)
	m.cost.add(o.cost)
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// harnessNames returns the worktree's harnesses in registry order (pi, claude),
// then any unknown harness sorted by name, so table rows are deterministic.
func (u *worktreeUsage) harnessNames() []string {
	var names []string
	for _, a := range agents() {
		if _, ok := u.harnesses[a.name]; ok {
			names = append(names, a.name)
		}
	}
	known := len(names)
	for name := range u.harnesses {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names[known:])
	return names
}

// modelNames returns a harness's models, most expensive first, then unknown
// costs, then by name, so the detail table is deterministic.
func (h *harnessUsage) modelNames() []string {
	names := make([]string, 0, len(h.models))
	for name := range h.models {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		a, b := h.models[names[i]], h.models[names[j]]
		if ak, bk := a.cost.known(), b.cost.known(); ak != bk {
			return ak
		}
		if !a.cost.unknown && !b.cost.unknown && a.cost.amount != b.cost.amount {
			return a.cost.amount > b.cost.amount
		}
		return names[i] < names[j]
	})
	return names
}

// known reports a priced total (a real zero included).
func (m money) known() bool { return m.any && !m.unknown }

// humanAge is the AGE grammar: one unit, largest first. A future creation mark
// (clock skew, a future .git mtime) reads as zero, never as a negative age.
func humanAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int64(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int64(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd", int64(d.Hours()/24))
	case d < 365*24*time.Hour:
		return fmt.Sprintf("%dM", int64(d.Hours()/(24*30)))
	default:
		return fmt.Sprintf("%dy", int64(d.Hours()/(24*365)))
	}
}

// humanActive is the compact ACTIVE grammar: "Yh ZZ m" at or above an hour,
// minutes below it.
func humanActive(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int64(d.Minutes()))
	}
	return humanClock(d)
}

// humanClock is the detail TIME grammar; it always names the hour so a column
// of rows reads as one unit ("0h 45m"), matching the design examples.
func humanClock(d time.Duration) string {
	return fmt.Sprintf("%dh %02dm", int64(d.Hours()), int64(d.Minutes())%60)
}

// seconds is the JSON unit for every active-time value.
func seconds(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}
