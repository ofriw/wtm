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
// when any contributor has no price. estimated is also sticky: a total is
// estimated when any contributor rests on an assumption, so the display can
// mark it and the JSON can flag it. A real zero stays a known, exact zero.
type money struct {
	amount    float64
	unknown   bool
	estimated bool
	any       bool
}

// addKnown records an exact contribution: a price the harness reported or the
// catalog listed.
func (m *money) addKnown(v float64) {
	m.any = true
	m.amount += v
}

// addEstimated records a contribution derived from a documented assumption
// rather than a reported price (see estimateCacheWriteLifetime).
func (m *money) addEstimated(v float64) {
	m.any = true
	m.amount += v
	m.estimated = true
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
	m.estimated = m.estimated || o.estimated
	if o.unknown {
		m.unknown = true
		return
	}
	m.amount += o.amount
}

// jsonValue is the machine contract: null when absent or unknown. An estimated
// total is a number; costEstimated carries the caveat.
func (m money) jsonValue() *float64 {
	if !m.any || m.unknown {
		return nil
	}
	v := roundCost(m.amount)
	return &v
}

// cell is the table contract: "-" absent, "unknown" unpriced, "~$d.dd" when an
// assumption priced part of the total, else "$d.dd".
func (m money) cell() string {
	switch {
	case !m.any:
		return "-"
	case m.unknown:
		return "unknown"
	case m.estimated:
		return "~" + formatCost(m.amount)
	default:
		return formatCost(m.amount)
	}
}

// isEstimated reports a numeric total that includes an assumed price, so the
// JSON contract can flag it beside the amount it qualifies.
func (m money) isEstimated() bool { return m.known() && m.estimated }

// knownZero is a priced $0.00: the marginal cost of a proven subscription turn,
// distinct from an absent or unknown total.
func knownZero() money { return money{any: true} }

// estimatedZero is a priced $0.00 that rests on an assumption: a proven
// subscription that may bill overage at list price once its allowance is used
// up. The marginal cost inside the allowance is $0; the estimate flag carries
// that caveat, and the list price is shown beside it.
func estimatedZero() money {
	var m money
	m.addEstimated(0)
	return m
}

// estimateSubscriptionOverage is the exact reason a subscription-billed $0
// reads estimated. status prints it verbatim beside the cache-write reason, so
// the reader can tell an allowance caveat from a token-split guess.
const estimateSubscriptionOverage = "a subscription covers the request inside its allowance, but extra usage may bill at list price, so the $0 billed cost is an estimate"

// costPair is the two prices of one accumulator. list is the pay-as-you-go
// value; billed is what the credentials actually charge. subscription marks a
// total where at least one event was proven subscription-billed, so the display
// can show both. An unproven row keeps billed == list and renders exactly as
// before, so the fallback never changes output.
// LIMIT: money's zero value is comparable (every field is a scalar, including
// the estimated flag). showsList compares only priced-ness and amount, so a
// future non-comparable field cannot break that test.
type costPair struct {
	list         money
	billed       money
	subscription bool
}

// add folds another pair in. subscription is sticky: one covered event marks
// the total, so the list price stays visible beside the discounted billed value.
func (p *costPair) add(o costPair) {
	p.list.add(o.list)
	p.billed.add(o.billed)
	p.subscription = p.subscription || o.subscription
}

// addUnknown taints both sides, so a partial total never looks complete.
func (p *costPair) addUnknown() {
	p.list.addUnknown()
	p.billed.addUnknown()
}

// known reports a priced billed total (a real zero included).
func (p costPair) known() bool { return p.billed.known() }

// showsList reports whether the pair carries a list price worth rendering: a
// proven subscription made the billed amount differ. The table and the JSON
// both consume this one rule, so they can never disagree on when to show it.
// Only priced-ness and amount compare: the estimated flag never shows a list
// by itself, so an estimated list beside an equal exact billed stays single.
func (p costPair) showsList() bool {
	return p.subscription && (p.billed.known() != p.list.known() || p.billed.amount != p.list.amount)
}

// estimateReasons names which "~" caveats a rendered total carries: the cache-write
// lifetime guess vs the subscription-overage allowance caveat. The legend prints
// only the set reasons, so a table with one kind of estimate never explains the other.
type estimateReasons struct{ cache, overage bool }

// needsOverageLegend reports the allowance caveat: a proven subscription bills $0
// on an estimate because extra usage may still bill at list price.
func (p costPair) needsOverageLegend() bool { return p.subscription && p.billed.isEstimated() }

// needsCacheLegend reports the token-split guess: the list price (or an
// unproven billed total) rests on the 5-minute cache-write assumption. A list
// the pair hides (showsList false) carries no "~" cell, so it asks for no line.
func (p costPair) needsCacheLegend() bool {
	return (p.showsList() && p.list.isEstimated()) || (!p.subscription && p.billed.isEstimated())
}

// reasons resolves the pair to the legend lines its rendered cells need.
func (p costPair) reasons() estimateReasons {
	return estimateReasons{cache: p.needsCacheLegend(), overage: p.needsOverageLegend()}
}

// merge unions two reason sets, so table, summary and model rows accumulate.
func (r estimateReasons) merge(o estimateReasons) estimateReasons {
	return estimateReasons{cache: r.cache || o.cache, overage: r.overage || o.overage}
}

// any reports whether any legend line is needed.
func (r estimateReasons) any() bool { return r.cache || r.overage }

// bothUnpriced reports a pair whose two sides are both unpriced, so a partial
// taint collapses to one "unknown" instead of "unknown/unknown". One rule
// serves both the table cell and the JSON listCost decision.
func (p costPair) bothUnpriced() bool { return !p.list.known() && !p.billed.known() }

// cell is the table contract: the billed value, plus "/list" when a proven
// subscription makes the two differ, so a reader sees the actual charge and the
// price the same tokens would cost metered.
func (p costPair) cell() string {
	if p.showsList() {
		// After markPartial taints both sides, collapse to single "unknown"
		if p.bothUnpriced() {
			return "unknown"
		}
		return p.billed.cell() + "/" + p.list.cell()
	}
	return p.billed.cell()
}

// roundCost kills float summation noise so JSON output is stable; 1e-6 is well
// below a cent and above the error of summing real token prices.
func roundCost(v float64) float64 { return float64(int64(v*1e6+0.5)) / 1e6 }

func formatCost(v float64) string { return fmt.Sprintf("$%.2f", roundCost(v)) }

// usageKey is the cheap pre-filter shared by the parsers: a session line without
// it cannot carry cost or tokens, so content-heavy user, progress and tool-result
// lines are never unmarshalled.
var usageKey = []byte("\"usage\"")

// promptTokens are the four priced Claude Code token classes. The harness keeps
// the 5-minute and 1-hour cache writes apart; unclassifiedCacheWrite is the
// flat cache_creation count when the record omits the lifetime breakdown.
type promptTokens struct {
	input                  int64
	output                 int64
	cacheRead              int64
	cacheWrite5m           int64
	cacheWrite1h           int64
	unclassifiedCacheWrite int64
}

// context is the request's input size, used for the long-context rate.
func (t promptTokens) context() int64 {
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
	tokens    promptTokens
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
	cost  costPair
	spans []span
}

func (m *modelUsage) active() time.Duration { return unionDuration(m.spans) }

// harnessUsage is one harness's totals for a worktree.
type harnessUsage struct {
	cost   costPair
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

func (u *worktreeUsage) cost() costPair {
	var p costPair
	for _, h := range u.harnesses {
		p.add(h.cost)
	}
	// A harness whose every session was unreadable keeps no entry (see
	// markPartial), so the worktree-level flag must taint the total itself.
	if u.partial {
		p.addUnknown()
	}
	return p
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
func (u *worktreeUsage) addSession(harness string, events []usageEvent, price func(usageEvent) costPair) {
	u.addBilledSession(harness, events, nil, price)
}

// addBilledSession folds one session where billed[i] marks the events that
// contribute cost. Every event still folds into the spans, so dedupe never
// changes activity time; only the billed subset contributes money. A nil mask
// bills every event, which is the no-dedupe case.
func (u *worktreeUsage) addBilledSession(harness string, events []usageEvent, billed []bool, price func(usageEvent) costPair) {
	if len(events) == 0 {
		return
	}
	h := u.harness(harness)
	session, perModel := foldSession(events, h, billed, price)
	h.spans = append(h.spans, session)
	for name, m := range perModel {
		h.model(name).absorb(m)
	}
}

// foldSession accumulates one session's harness cost and returns the session
// span plus each model's per-session spans and cost.
func foldSession(events []usageEvent, h *harnessUsage, billed []bool, price func(usageEvent) costPair) (span, map[string]*modelUsage) {
	session := span{start: events[0].at, end: events[0].at}
	perModel := map[string]*modelUsage{}
	prev := ""
	for i, e := range events {
		session.start = minTime(session.start, e.at)
		session.end = maxTime(session.end, e.endAt())
		prev = foldEvent(perModel, h, e, billed == nil || i >= len(billed) || billed[i], prev, price)
	}
	return session, perModel
}

// foldEvent extends one model's open run and both cost accumulators. prev is
// the previous event's model: consecutive same-model events chain into one run
// (first→last event), a different model closes the run, so an interleaved
// model never inherits another model's idle time. An isolated event is a point.
// A non-billed event still chains spans; only its money is skipped.
func foldEvent(perModel map[string]*modelUsage, h *harnessUsage, e usageEvent, bill bool, prev string, price func(usageEvent) costPair) string {
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
	if bill {
		c := price(e)
		m.cost.add(c)
		h.cost.add(c)
	}
	return e.model
}

// dedupeScore orders two copies of one billed turn. A copy that carries its own
// price first beats one that lost it, so a self-priced Pi copy is never dropped
// for a larger token-only copy (see piEventKeyN). Then the larger token total
// wins, because a partial stream undercounts; a token tie breaks on the
// web-search count, which the catalog prices as real money, then on the larger
// self-priced cost. Either field alone can carry cost the other copy lacks.
type dedupeScore struct {
	tokens  int64
	web     int64
	cost    float64
	hasCost bool
}

func (s dedupeScore) beats(o dedupeScore) bool {
	if s.hasCost != o.hasCost {
		return s.hasCost
	}
	if s.tokens != o.tokens {
		return s.tokens > o.tokens
	}
	if s.web != o.web {
		return s.web > o.web
	}
	return s.cost > o.cost
}

// scoreOf reads the comparable size of one event for dedupe: token and
// web-search counts for catalog-priced turns, self-priced cost for Pi turns.
func scoreOf(e usageEvent) dedupeScore {
	s := dedupeScore{tokens: e.tokens.total(), web: e.webSearch}
	if e.cost != nil {
		s.cost, s.hasCost = *e.cost, true
	}
	return s
}

// dedupeKeyedEvents decides which occurrence of each billed turn prices. It
// returns a per-session billed mask: true marks the occurrence that contributes
// cost. Spans ignore the mask and fold every event, so dedupe never changes
// activity time. Keyless events always bill. The winner is the first occurrence
// of a key with the best score, so the billed money total is stable across
// session order; which tied copy wins can vary, and it carries the provider,
// model and timestamp the pricer reads. The caller keeps its event slices;
// only the returned masks describe the change.
func dedupeKeyedEvents(sessions [][]usageEvent) [][]bool {
	best := map[string]dedupeScore{}
	for _, events := range sessions {
		for _, e := range events {
			if e.key == "" {
				continue
			}
			s := scoreOf(e)
			if w, ok := best[e.key]; !ok || s.beats(w) {
				best[e.key] = s
			}
		}
	}
	masks := make([][]bool, len(sessions))
	for si, events := range sessions {
		mask := make([]bool, len(events))
		for ei, e := range events {
			if e.key == "" {
				mask[ei] = true
				continue
			}
			// Keep only the first record matching the best score; delete its
			// key so equal duplicates later in the walk drop too. Equality is
			// the beats tie (neither side wins), so equal-cost copies elect the first.
			w, ok := best[e.key]
			s := scoreOf(e)
			if ok && !s.beats(w) && !w.beats(s) {
				mask[ei] = true
				delete(best, e.key)
			}
		}
		masks[si] = mask
	}
	return masks
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
		// Billed is the reader-facing amount, so its order decides the rows.
		a, b := h.models[names[i]].cost.billed, h.models[names[j]].cost.billed
		if ak, bk := a.known(), b.known(); ak != bk {
			return ak
		}
		if !a.unknown && !b.unknown && a.amount != b.amount {
			return a.amount > b.amount
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
