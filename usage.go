package main

import (
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
)

// usage.go — the cost/active-time domain shared by every harness. Parsers turn
// session records into usageEvent values; addBilledSession folds those into a
// worktreeUsage. This file owns the folding, the money/span model, the
// cross-file dedupe and the display grammar; the session loader that drives
// addBilledSession (buildUsage) lives with the harness registry in agents.go.

// estimateCause names the documented assumption behind an estimated amount.
// It is a bit set, so a fold that mixes both assumptions keeps both reasons and
// the legend can print exactly the ones the rendered cells carry.
type estimateCause uint8

const (
	// causeCacheWrite: a record omitted the 5m/1h cache-write split.
	causeCacheWrite estimateCause = 1 << iota
	// causeSubscriptionOverage: a covered $0 may still bill overage.
	causeSubscriptionOverage
)

// money accumulates a cost while keeping "unknown" sticky: a total is unknown
// when any contributor has no price. A cause is also sticky: a total is
// estimated when any contributor rests on an assumption, so the display can
// mark it and the JSON can flag it. A real zero stays a known, exact zero.
type money struct {
	amount  float64
	unknown bool
	causes  estimateCause
	any     bool
}

// addKnown records an exact contribution: a price the harness reported or the
// catalog listed.
func (m *money) addKnown(v float64) {
	m.any = true
	m.amount += v
}

// addEstimated records a contribution derived from a documented assumption
// rather than a reported price (see estimateCacheWriteLifetime and
// estimateSubscriptionOverage).
func (m *money) addEstimated(v float64, cause estimateCause) {
	m.any = true
	m.amount += v
	m.causes |= cause
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
	m.causes |= o.causes
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
	case m.causes != 0:
		return "~" + formatCost(m.amount)
	default:
		return formatCost(m.amount)
	}
}

// isEstimated reports a numeric total that includes an assumed price, so the
// JSON contract can flag it beside the amount it qualifies.
func (m money) isEstimated() bool { return m.known() && m.causes != 0 }

// hasCause reports one documented assumption among the total's causes.
func (m money) hasCause(c estimateCause) bool { return m.causes&c != 0 }

// knownZero is a priced $0.00: the marginal cost of a proven subscription turn,
// distinct from an absent or unknown total.
func knownZero() money { return money{any: true} }

// estimatedZero is a priced $0.00 that rests on an assumption: a proven
// subscription that may bill overage at list price once its allowance is used
// up. The marginal cost inside the allowance is $0; the estimate flag carries
// that caveat, and the list price is shown beside it.
func estimatedZero() money {
	var m money
	m.addEstimated(0, causeSubscriptionOverage)
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
// the causes bit set). showsList compares only priced-ness and amount, so a
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
// on an estimate because extra usage may still bill at list price. Only the
// overage cause qualifies; a cache-write guess on a covered total must not ask
// for the allowance line.
func (p costPair) needsOverageLegend() bool {
	return p.billed.known() && p.billed.hasCause(causeSubscriptionOverage)
}

// needsCacheLegend reports the token-split guess: the list price (or a billed
// total) rests on the 5-minute cache-write assumption. A list the pair hides
// (showsList false) carries no "~" cell, so it asks for no line.
func (p costPair) needsCacheLegend() bool {
	if p.showsList() && p.list.known() && p.list.hasCause(causeCacheWrite) {
		return true
	}
	return p.billed.known() && p.billed.hasCause(causeCacheWrite)
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

// roundRate gives a 0..1 share the same 1e-6 trim as a price, so a ratio in the
// machine contract is as byte-stable as a cost.
func roundRate(v float64) float64 { return roundCost(v) }

func formatCost(v float64) string { return fmt.Sprintf("$%.2f", roundCost(v)) }

// tier names one accumulator and JSON token class. The order is the index into
// tokenTiers and the JSON key order; the table's display order is separate (see
// status_detail.go detailTokenMeta).
type tier int

const (
	tierInput tier = iota
	tierCacheRead
	tierCacheWrite
	tierOutput
	numTiers
)

// tierMeta is each tier's JSON key, indexed by tier and kept in tier order, so
// the machine contract and the accumulator can never disagree on which tier is
// which.
var tierMeta = [numTiers]string{
	"input",
	"cacheRead",
	"cacheWrite",
	"output",
}

// tokenTiers is a model's per-tier price breakdown, indexed by tier. Each tier
// keeps the billed/list pair, so the subscription invariant holds per tier
// exactly as it does for the total.
type tokenTiers [numTiers]costPair

func (t *tokenTiers) add(o tokenTiers) {
	for i := range t {
		t[i].add(o[i])
	}
}

// totals sums the tier list prices; the caller adds the web search price and
// the resolved baseline to reach the event total.
func (t tokenTiers) totals() money {
	var m money
	for i := range t {
		m.add(t[i].list)
	}
	return m
}

// tokenCosts is a harness-reported per-class price (Pi). A nil field means the
// harness did not price that class, so its tier stays absent, never zero.
type tokenCosts struct {
	input      *float64
	output     *float64
	cacheRead  *float64
	cacheWrite *float64
}

// pricedEvent is one event's price: the total (token classes plus web search)
// and the per-tier breakdown. Both sides are filled together, so the billed/list
// rule reaches every figure the display draws from one event.
type pricedEvent struct {
	total costPair
	tiers tokenTiers
}

// usageKey is the cheap pre-filter shared by the parsers: a session line without
// it cannot carry cost or tokens, so content-heavy user, progress and tool-result
// lines are never unmarshalled.
var usageKey = []byte("\"usage\"")

// promptTokens are the priced prompt classes shared by every harness. Pi keeps
// one cache-write class (filled into cacheWrite5m); Claude keeps the 5-minute
// and 1-hour writes apart. unclassifiedCacheWrite is Claude's flat
// cache_creation count when the record omits the lifetime breakdown.
type promptTokens struct {
	input                  int64
	output                 int64
	cacheRead              int64
	cacheWrite5m           int64
	cacheWrite1h           int64
	unclassifiedCacheWrite int64
}

// total is the sum of every token class, for dedupe scoring and the empty-tier
// test. It is not the prompt size; context() is.
func (t promptTokens) total() int64 {
	return t.input + t.output + t.cacheRead + t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
}

// derefInt reads an optional JSON count; absent means zero, never a negative
// sentinel. Parsers use it so one missing field cannot fail a record.
func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// context is the request's input size (every prompt token: fresh, written and
// cached), used for the long-context rate and as the hit-rate denominator.
func (t promptTokens) context() int64 {
	return t.input + t.cacheRead + t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
}

// miss is the prompt tokens not served from cache: fresh input plus every cache
// write (the tokens a build spends). It is context() minus the cache reads, so
// the miss subtotal and the hit-rate denominator share one value.
func (t promptTokens) miss() int64 { return t.context() - t.cacheRead }

// hitRate is the cache-read share of the prompt tokens, and ok=false when the
// ratio is undefined. A cache read always follows a build (uncached input or a
// cache write), so reads with no recorded miss mean the build predates the
// recorded window: the share would read a false 100% and is refused here, as is
// a model that recorded no prompt tokens at all. The share is also capped at
// maxHitRate, so the 1e-6 machine rounding cannot lift a near-1 value to 1.0.
func (t promptTokens) hitRate() (float64, bool) {
	if t.miss() <= 0 {
		return 0, false
	}
	return min(float64(t.cacheRead)/float64(t.context()), maxHitRate), true
}

// maxHitRate is the highest share any model may report: no window is ever fully
// cache-served (a read always follows a build), and the cap stops the machine
// rounding in cacheJSON from emitting a false 1.0.
const maxHitRate = 0.999

// ratiosDefined reports whether the summary's input:output ratios exist: both
// sides must have recorded tokens, since a ratio without a denominator (no
// output) or a numerator (no prompt tokens) is undefined.
func (t promptTokens) ratiosDefined() bool { return t.context() > 0 && t.output > 0 }

// outputRatio is one input:output share for the worktree summary: n divided by
// the recorded output, ok=false when no output was recorded. Every summary
// ratio divides by the same output, so the three read as comparable addends
// (total = cached + uncached); a zero denominator is refused, never an infinity.
func (t promptTokens) outputRatio(n int64) (float64, bool) {
	if t.output <= 0 {
		return 0, false
	}
	return float64(n) / float64(t.output), true
}

// tier returns the token count in one displayed tier. The cache-write classes
// (5m, 1h, unclassified) collapse into the single displayed write tier; the
// split stays internal so the rate catalog can price it exactly.
func (t promptTokens) tier(id tier) int64 {
	switch id {
	case tierInput:
		return t.input
	case tierCacheRead:
		return t.cacheRead
	case tierCacheWrite:
		return t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
	case tierOutput:
		return t.output
	}
	return 0
}

// plus folds another token count in field by field.
func (t promptTokens) plus(o promptTokens) promptTokens {
	return promptTokens{
		input: t.input + o.input, output: t.output + o.output,
		cacheRead:              t.cacheRead + o.cacheRead,
		cacheWrite5m:           t.cacheWrite5m + o.cacheWrite5m,
		cacheWrite1h:           t.cacheWrite1h + o.cacheWrite1h,
		unclassifiedCacheWrite: t.unclassifiedCacheWrite + o.unclassifiedCacheWrite,
	}
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
	tierCost  *tokenCosts // harness-reported per-class price; nil for Claude
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
	cost    costPair
	tiers   tokenTiers
	tokens  promptTokens
	spans   []span
	partial bool // lost records may have carried this model's tokens or time
}

func (m *modelUsage) active() time.Duration { return unionDuration(m.spans) }

// harnessUsage is one harness's totals for a worktree.
type harnessUsage struct {
	cost    costPair
	spans   []span // session spans (all events)
	models  map[string]*modelUsage
	partial bool // an unreadable session or dropped record may belong here
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

// addBilledSession folds one session where billed[i] marks the events that
// contribute cost. Every event still folds into the spans, so dedupe never
// changes activity time; only the billed subset contributes money. A nil mask
// bills every event, which is the no-dedupe case.
func (u *worktreeUsage) addBilledSession(harness string, events []usageEvent, billed []bool, price func(usageEvent) pricedEvent) {
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
func foldSession(events []usageEvent, h *harnessUsage, billed []bool, price func(usageEvent) pricedEvent) (span, map[string]*modelUsage) {
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
func foldEvent(perModel map[string]*modelUsage, h *harnessUsage, e usageEvent, bill bool, prev string, price func(usageEvent) pricedEvent) string {
	m := modelFor(perModel, e.model)
	extendRun(m, e, prev)
	if bill {
		foldBilled(m, h, e, price)
	}
	return e.model
}

// modelFor returns (creating if needed) the session accumulator for one model.
func modelFor(perModel map[string]*modelUsage, name string) *modelUsage {
	m := perModel[name]
	if m == nil {
		m = &modelUsage{}
		perModel[name] = m
	}
	return m
}

// extendRun chains consecutive same-model events into one run; a different
// model or an empty run starts a new span.
func extendRun(m *modelUsage, e usageEvent, prev string) {
	if prev == e.model && len(m.spans) > 0 {
		last := &m.spans[len(m.spans)-1]
		last.start = minTime(last.start, e.at)
		last.end = maxTime(last.end, e.endAt())
		return
	}
	m.spans = append(m.spans, span{start: e.at, end: e.endAt()})
}

// foldBilled adds one event's priced pair to the model and harness totals.
func foldBilled(m *modelUsage, h *harnessUsage, e usageEvent, price func(usageEvent) pricedEvent) {
	p := price(e)
	m.cost.add(p.total)
	m.tiers.add(p.tiers)
	m.tokens = m.tokens.plus(e.tokens)
	h.cost.add(p.total)
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
	best := bestScores(sessions)
	masks := make([][]bool, len(sessions))
	for si, events := range sessions {
		masks[si] = electBilledMask(events, best)
	}
	return masks
}

// bestScores records the winning score per billed-turn key.
func bestScores(sessions [][]usageEvent) map[string]dedupeScore {
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
	return best
}

// electBilledMask marks the first occurrence matching each key's best score.
func electBilledMask(events []usageEvent, best map[string]dedupeScore) []bool {
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
	return mask
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
	m.tiers.add(o.tiers)
	m.tokens = m.tokens.plus(o.tokens)
	m.partial = m.partial || o.partial
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

// mergedModels folds every harness's models into one accumulator per name,
// summing cost, tokens, and spans. The detail block lists models, not harnesses, so a name
// two harnesses share reads as a single row.
func (u *worktreeUsage) mergedModels() map[string]*modelUsage {
	out := make(map[string]*modelUsage, len(u.harnesses))
	for _, h := range u.harnesses {
		for name, m := range h.models {
			dst := out[name]
			if dst == nil {
				dst = &modelUsage{}
				out[name] = dst
			}
			dst.absorb(m)
		}
	}
	return out
}

// tokens sums every model's folded token counts across harnesses: the
// worktree-level total the summary ratios divide. Dedupe already decided which
// events fold (see foldEvent), so this only adds what those folds recorded.
func (u *worktreeUsage) tokens() promptTokens {
	var t promptTokens
	for _, h := range u.harnesses {
		for _, m := range h.models {
			t = t.plus(m.tokens)
		}
	}
	return t
}

// sortedModelNames returns the merged models ordered most expensive first,
// then unknown costs, then by name.
func (u *worktreeUsage) sortedModelNames(models map[string]*modelUsage) []string {
	names := make([]string, 0, len(models))
	for name := range models {
		names = append(names, name)
	}
	sortModels(names, func(n string) costPair { return models[n].cost })
	return names
}

// sortModels orders model names by billed cost: known before unknown, then
// descending amount, then name. Billed is the reader-facing amount, so its
// order decides the rows; one comparator serves every model listing.
func sortModels(names []string, cost func(string) costPair) {
	sort.Slice(names, func(i, j int) bool {
		a, b := cost(names[i]).billed, cost(names[j]).billed
		if ak, bk := a.known(), b.known(); ak != bk {
			return ak
		}
		if !a.unknown && !b.unknown && a.amount != b.amount {
			return a.amount > b.amount
		}
		return names[i] < names[j]
	})
}

// known reports a priced total (a real zero included).
func (m money) known() bool { return m.any && !m.unknown }

// durationUnits is the mixed-radix basis for humanDuration, largest first.
// Months use 30 days and years 365, matching the previous single-unit grammar.
var durationUnits = []struct {
	size time.Duration
	name string
}{
	{365 * 24 * time.Hour, "y"},
	{30 * 24 * time.Hour, "M"},
	{24 * time.Hour, "d"},
	{time.Hour, "h"},
	{time.Minute, "m"},
}

// humanDuration is the single grammar for every time figure (age, active time,
// per-model time): the non-zero units from years down to minutes, with seconds
// only below a minute, so a span reads as one breakdown ("2d 5h 11m") and a
// future mark never reads negative.
func humanDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int64(d.Seconds()))
	}
	var parts []string
	for _, u := range durationUnits {
		if n := int64(d / u.size); n > 0 {
			parts = append(parts, fmt.Sprintf("%d%s", n, u.name))
			d -= time.Duration(n) * u.size
		}
	}
	if len(parts) == 0 {
		return "0s"
	}
	return strings.Join(parts, " ")
}

// formatPercent renders a 0..1 share as a one-decimal percent. The ceiling
// lives in hitRate (maxHitRate), so the caller never passes a value that could
// round to a false 100%.
func formatPercent(v float64) string {
	return fmt.Sprintf("%.1f%%", v*100)
}

// formatRatio renders an input:output share with the output normalised to 1.
// The shared denominator is what makes the summary's three ratios comparable
// and additive.
func formatRatio(v float64) string {
	return fmt.Sprintf("%.1f:1", v)
}

// formatTokens renders a token count in a compact unit: exact below 1000, then
// k/M/B with one decimal, rounded down so a value never crosses into the next
// unit (999999 reads 999.9k, not 1000.0k). The exact count stays in JSON.
func formatTokens(n int64) string {
	switch {
	case n < 1_000:
		return fmt.Sprintf("%d", n)
	case n < 1_000_000:
		return fmt.Sprintf("%.1fk", math.Floor(float64(n)/100)/10)
	case n < 1_000_000_000:
		return fmt.Sprintf("%.1fM", math.Floor(float64(n)/100_000)/10)
	default:
		return fmt.Sprintf("%.1fB", math.Floor(float64(n)/100_000_000)/10)
	}
}

// seconds is the JSON unit for every active-time value.
func seconds(d time.Duration) int64 {
	if d < 0 {
		return 0
	}
	return int64(d / time.Second)
}
