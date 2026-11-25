package main

import (
	"bytes"
	"encoding/json"
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
// addBilledSession (the report path) lives with the harness registry in agents.go.

// estimateCause names the documented assumption behind an estimated amount.
// It is a bit set, so a fold that mixes both assumptions keeps both reasons and
// the legend can print exactly the ones the rendered cells carry.
type estimateCause uint8

const (
	// causeCacheWrite: a record omitted the 5m/1h cache-write split.
	causeCacheWrite estimateCause = 1 << iota
	// causeSubscriptionOverage: a covered $0 may still bill overage. Only
	// estimatedZero may set it; `make guard` fails the build on any other writer.
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
// below a cent and above the error of summing real token prices. Above the int64
// fixed-point range (|v| > MaxInt64/1e6) the int64 conversion would wrap to a
// wrong exact figure, so the exact float is kept instead.
func roundCost(v float64) float64 {
	const scale = 1e6
	if v >= math.MaxInt64/scale || v <= -math.MaxInt64/scale {
		return v
	}
	return float64(int64(v*scale+0.5)) / scale
}

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

// usageKey is the cheap byte pre-filter: a session line without it cannot bill,
// so content-heavy lines are never unmarshalled.
var usageKey = []byte("\"usage\"")

// scanRecord reads a JSON record's top-level type and usage presence in one
// token walk. typ/typOK are the top-level "type" string; hasUsage reports a
// usage key at a billing location: the top level or one nested inside a message
// object. malformed marks a body that did not read as a well-formed object, so
// callers fail closed instead of trusting a partial record.
func scanRecord(b []byte) (typ string, typOK, hasUsage, malformed bool) {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return "", false, false, true
	}
	msg := 0 // open message objects whose members are being scanned
	for {
		if !dec.More() {
			if msg == 0 {
				return typ, typOK, hasUsage, false
			}
			if _, err := dec.Token(); err != nil { // close this message object
				return typ, typOK, hasUsage, true
			}
			msg--
			continue
		}
		key, err := dec.Token()
		if err != nil {
			return typ, typOK, hasUsage, true
		}
		name, ok := key.(string)
		if !ok {
			return typ, typOK, hasUsage, true
		}
		switch name {
		case "usage":
			hasUsage = true
			if !skipJSONValue(dec, false) {
				return typ, typOK, hasUsage, true
			}
		case "type":
			if msg == 0 && !typOK {
				v, err := dec.Token()
				if err != nil {
					return typ, typOK, hasUsage, true
				}
				if s, ok := v.(string); ok {
					typ, typOK = s, true
				}
			} else if !skipJSONValue(dec, false) {
				return typ, typOK, hasUsage, true
			}
		case "message":
			tok, err := dec.Token()
			if err != nil {
				return typ, typOK, hasUsage, true
			}
			if d, ok := tok.(json.Delim); ok {
				switch d {
				case '{':
					msg++
				case '[':
					if !skipJSONValue(dec, true) {
						return typ, typOK, hasUsage, true
					}
				}
			}
		default:
			if !skipJSONValue(dec, false) {
				return typ, typOK, hasUsage, true
			}
		}
	}
}

// skipJSONValue consumes one JSON value from dec, reporting false when the input
// ends or corrupts mid-value; opened drains the rest of an already-open value.
func skipJSONValue(dec *json.Decoder, opened bool) bool {
	depth := 0
	if opened {
		depth = 1
	}
	for {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		d, ok := tok.(json.Delim)
		if !ok {
			if depth == 0 {
				return true
			}
			continue
		}
		switch d {
		case '{', '[':
			depth++
		case '}', ']':
			if depth == 1 {
				return true
			}
			depth--
		}
	}
}

// promptTokens are the priced prompt classes shared by every harness. Pi keeps
// one cache-write class (filled into cacheWrite5m); Claude keeps the 5-minute
// and 1-hour writes apart. unclassifiedCacheWrite is Claude's flat
// cache_creation count when the record omits the lifetime breakdown. corrupt
// marks rejected counts: zeroed, but the tier still reads unknown.
type promptTokens struct {
	input                  int64
	output                 int64
	cacheRead              int64
	cacheWrite5m           int64
	cacheWrite1h           int64
	unclassifiedCacheWrite int64
	corrupt                tierMask
	// conflicts marks a class whose figures contradict each other inside one
	// record. The parser proves it but has no warning sink, so it flags the tier
	// and sanitizeTokens (the single sink site) rejects and names it. Cleared
	// there, so it never reaches a fold.
	conflicts tierMask
}

// tierMask marks corrupt classes; the three write classes share tierCacheWrite,
// because the display collapses them into one cell.
type tierMask uint8

func (m tierMask) has(id tier) bool { return m&(1<<id) != 0 }

func (m tierMask) with(id tier) tierMask { return m | 1<<id }

// corruptPrompt reports a rejected input, cache-read or cache-write count, which
// invalidates every derived miss and hit-rate value.
func (t promptTokens) corruptPrompt() bool {
	return t.corrupt.has(tierInput) || t.corrupt.has(tierCacheRead) || t.corrupt.has(tierCacheWrite)
}

// total is the sum of every token class, for dedupe scoring and the empty-tier
// test. It is not the prompt size; context() is.
func (t promptTokens) total() int64 {
	return t.input + t.output + t.cacheRead + t.cacheWrite5m + t.cacheWrite1h + t.unclassifiedCacheWrite
}

// hasUsageEvidence reports any recorded token use, including a rejected count:
// sanitization zeroes the figure, but zero tokens is not the same as no record.
func (t promptTokens) hasUsageEvidence() bool { return t.total() > 0 || t.corrupt != 0 }

// derefInt reads an optional JSON count; absent means zero, never a negative
// sentinel. Parsers use it so one missing field cannot fail a record.
func derefInt(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// rawFields is the per-figure decode seam shared by every transcript parser. A
// typed decode of the whole struct fails on the first number this build cannot
// hold (an int64 overflow, a fraction, 1e999), and with it loses every readable
// charge, count, and timestamp in the same record. Reading the object as raw
// members lets each parser reject one figure by name and keep its siblings.
type rawFields map[string]json.RawMessage

func rawObject(b []byte) (rawFields, error) {
	var f rawFields
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, err
	}
	return f, nil
}

func (f rawFields) value(key string) (json.RawMessage, bool) {
	raw, ok := f[key]
	return raw, ok && statesValue(raw)
}

// count reads one optional integer figure. A value that is not an integer this
// build can hold keeps the unreadable sentinel, so its class reads unknown.
func (f rawFields) count(key string) *int64 {
	raw, ok := f.value(key)
	if !ok {
		return nil
	}
	v := new(int64)
	if json.Unmarshal(raw, v) != nil {
		*v = unreadableCount
	}
	return v
}

// amount reads one optional price figure; an unreadable one keeps NaN, which
// invalidAmount rejects by field name. A negative-zero parse (a negative
// underflow like -1e-999 that rounds to -0) is unreadable too: it is not an
// exact figure, and passing it would price a real charge at a silent zero.
// Explicit numeric zero stays a valid, exact free class.
// amount reads one optional price figure; an unreadable one keeps NaN, which
// invalidAmount rejects by field name. A numeric overflow (a value this build
// cannot hold) keeps +Inf so it reports as out of range, not as a non-number.
func (f rawFields) amount(key string) *float64 {
	raw, ok := f.value(key)
	if !ok {
		return nil
	}
	v := new(float64)
	if json.Unmarshal(raw, v) != nil {
		if isJSONNumber(raw) {
			*v = math.Inf(1)
		} else {
			*v = math.NaN()
		}
		return v
	}
	if *v == 0 && math.Signbit(*v) {
		*v = math.NaN() // negative underflow (-1e-999) rounds to -0; not exact
	}
	return v
}

// text reads one optional string figure; an unreadable one keeps the unreadable
// marker, which names no tier or model and so prices unknown.
func (f rawFields) text(key string) *string {
	raw, ok := f.value(key)
	if !ok {
		return nil
	}
	v := new(string)
	if json.Unmarshal(raw, v) != nil {
		*v = unreadableText
	}
	return v
}

// decodeObject reads a nested figure, keeping absence distinct from a zero value.
// Only numeric and text members get a per-field pardon: a member that is not the
// object it claims to be fails the record.
func decodeObject[T any](f rawFields, key string) (*T, error) {
	raw, ok := f.value(key)
	if !ok {
		return nil, nil
	}
	v := new(T)
	if err := json.Unmarshal(raw, v); err != nil {
		return nil, err
	}
	return v, nil
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
	if t.corruptPrompt() || t.miss() <= 0 {
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
// output) or a numerator (no prompt tokens) is undefined, as is a corrupt class.
func (t promptTokens) ratiosDefined() bool {
	return t.corrupt == 0 && t.context() > 0 && t.output > 0
}

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

// plus folds another count field by field; corruption is sticky across folds.
func (t promptTokens) plus(o promptTokens) promptTokens {
	return promptTokens{
		input: t.input + o.input, output: t.output + o.output,
		cacheRead:              t.cacheRead + o.cacheRead,
		cacheWrite5m:           t.cacheWrite5m + o.cacheWrite5m,
		cacheWrite1h:           t.cacheWrite1h + o.cacheWrite1h,
		unclassifiedCacheWrite: t.unclassifiedCacheWrite + o.unclassifiedCacheWrite,
		corrupt:                t.corrupt | o.corrupt,
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

// mergeSpans sorts and unions overlapping or touching spans. It clones its
// input, so callers may pass a slice they still own.
func mergeSpans(spans []span) []span {
	if len(spans) == 0 {
		return nil
	}
	sorted := slices.Clone(spans)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].start.Before(sorted[j].start) })
	out := []span{sorted[0]}
	for _, s := range sorted[1:] {
		last := &out[len(out)-1]
		if s.start.After(last.end) {
			out = append(out, s)
			continue
		}
		last.end = maxTime(last.end, s.end)
	}
	return out
}

// unionDuration merges overlapping spans and returns the covered time. A span
// with one event has zero length (its start equals its end). The sum is
// saturated at the maximum representable duration: a corrupt-but-parseable
// timestamp far outside the supported range makes Time.Sub saturate, and adding
// two such spans would wrap negative (and project to an exact zero). Saturating
// keeps the total a qualified over-approximation instead of a false figure.
func unionDuration(spans []span) time.Duration {
	// mergeSpans yields sorted, non-overlapping spans, so each end >= start.
	var total uint64
	for _, s := range mergeSpans(spans) {
		total += uint64(s.end.Sub(s.start))
		if total > math.MaxInt64 {
			total = math.MaxInt64
		}
	}
	return time.Duration(total)
}

// modelUsage is one model's totals inside a harness; spans are its usage
// spans clipped to the session's causal turns, merged for the active time.
type modelUsage struct {
	cost    costPair
	tiers   tokenTiers
	tokens  promptTokens
	spans   []span
	partial bool // lost records may have carried this model's tokens or time
}

func (m *modelUsage) active() time.Duration { return unionDuration(m.spans) }

// tokensKnown reports whether the model has token evidence, including a corrupt
// class that renders unknown instead of absent.
func (m *modelUsage) tokensKnown() bool { return m.tokens.hasUsageEvidence() }

// harnessUsage is one harness's totals for a worktree.
type harnessUsage struct {
	cost    costPair
	spans   []span // causal turn spans
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
// contribute cost. Activity folds every event's causal turns before the cost
// gate, so dedupe never changes active time and a session that recorded turns
// but no usage still proves TIME (cost stays absent, never a free zero). Only
// the billed subset contributes money. A nil mask bills every event, which is
// the no-dedupe case. A nil activity slice is the test/older-parse fallback: it
// treats the usage event range as one turn.
func (u *worktreeUsage) addBilledSession(harness string, events []usageEvent, activity []activityEvent, billed []bool, price func(usageEvent) pricedEvent, now time.Time) {
	turns := sessionTurns(activity, events)
	if len(events) == 0 && len(turns) == 0 {
		return
	}
	h := u.harness(harness)
	// An empty event set is a usage-free transcript: its turns prove TIME (and
	// the answering model's share) but no money or loss.
	if len(events) > 0 {
		attributeTurns(h, foldSession(events, h, billed, price, answerBarriers(turns, events)), turns, events, now)
	} else {
		creditAnswers(h, turns, now)
	}
	h.spans = append(h.spans, clampSpans(turnSpans(turns), now)...)
}

// attributeTurns clips each model's usage runs to the turns, then credits the
// turns' uncovered openings to the answering models. Attribution uses the
// complete turns; only the resulting spans are clamped to the clock, so a turn
// crossing the clock still credits its answering model.
func attributeTurns(h *harnessUsage, perModel map[string]*modelUsage, turns []turn, events []usageEvent, now time.Time) {
	clipped, covered := clipModels(perModel, turns)
	prefixes := turnOpenings(turns, events, mergeSpans(covered))
	for name, m := range perModel {
		m.spans = append(clampSpans(clipped[name], now), clampSpans(prefixes[name], now)...)
		h.model(name).absorb(m)
	}
	absorbAnswerOnly(h, perModel, prefixes, now)
}

// creditAnswers proves a usage-free transcript's model time: every turn with an
// explicit answer credits that model its opening. No usage means no partial
// flag, because no billed record was lost.
func creditAnswers(h *harnessUsage, turns []turn, now time.Time) {
	absorbAnswerOnly(h, nil, turnOpenings(turns, nil, nil), now)
}

// absorbAnswerOnly adds the prefixes of models that no usage event names, so a
// named assistant reply with no usage still appears with its time. perModel may
// be nil, which is the usage-free transcript case.
func absorbAnswerOnly(h *harnessUsage, perModel map[string]*modelUsage, prefixes map[string][]span, now time.Time) {
	for name, spans := range prefixes {
		if perModel[name] != nil {
			continue
		}
		h.model(name).absorb(&modelUsage{spans: clampSpans(spans, now)})
	}
}

// clipModels clips each model's raw runs to the session turns and returns the
// per-model result plus one flat slice of that time for coverage checks.
func clipModels(perModel map[string]*modelUsage, turns []turn) (map[string][]span, []span) {
	blocks := turnSpans(turns)
	clipped := make(map[string][]span, len(perModel))
	var covered []span
	for name, m := range perModel {
		spans := clipSpans(m.spans, blocks)
		clipped[name] = spans
		covered = append(covered, spans...)
	}
	return clipped, covered
}

// foldSession accumulates one session's harness cost and returns each model's
// raw usage spans and cost. The caller clips those spans to the session's
// causal turns, so model TIME excludes idle between turns without losing a
// spawn child's call-to-result attribution.
func foldSession(events []usageEvent, h *harnessUsage, billed []bool, price func(usageEvent) pricedEvent, barriers map[string][]span) map[string]*modelUsage {
	perModel := map[string]*modelUsage{}
	prev := ""
	for i, e := range events {
		prev = foldEvent(perModel, h, e, billed == nil || i >= len(billed) || billed[i], prev, price, barriers[e.model])
	}
	return perModel
}

// foldEvent extends one model's open run and both cost accumulators. prev is
// the previous event's model: consecutive same-model events chain into one raw
// run (first→last event), a different model closes the run. The caller later
// clips the raw runs to the session's causal turns, so idle never reaches a
// model's TIME. An isolated event is a point. A non-billed event still chains
// spans; only its money is skipped.
func foldEvent(perModel map[string]*modelUsage, h *harnessUsage, e usageEvent, bill bool, prev string, price func(usageEvent) pricedEvent, barriers []span) string {
	m := modelFor(perModel, e.model)
	extendRun(m, e, prev, barriers)
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

// extendRun chains consecutive same-model events into one raw run; a different
// model or an empty run starts a new span. Raw spans may include idle between
// turns; addBilledSession clips them to the causal turns.
func extendRun(m *modelUsage, e usageEvent, prev string, barriers []span) {
	if prev == e.model && len(m.spans) > 0 && !bridgesForeignTurn(&m.spans[len(m.spans)-1], e, barriers) {
		last := &m.spans[len(m.spans)-1]
		last.start = minTime(last.start, e.at)
		last.end = maxTime(last.end, e.endAt())
		return
	}
	m.spans = append(m.spans, span{start: e.at, end: e.endAt()})
}

// bridgesForeignTurn reports whether chaining the previous span onto e would
// bridge a turn answered by another model. Such a barrier lies wholly between
// the previous span's end and e's start, so the chain would cover the foreign
// answer's turn.
func bridgesForeignTurn(last *span, e usageEvent, barriers []span) bool {
	for _, b := range barriers {
		if !b.start.Before(last.end) && !b.end.After(e.at) {
			return true
		}
	}
	return false
}

// answerBarriers maps each model named by the transcript to the merged spans of
// turns answered by a different model. A chain of same-model point events must
// never bridge one of these turns: bridging would attribute the foreign
// answer's turn to the wrong model. Turns with no named answer are not
// barriers, so the fallback opening attribution keeps ownership of them. A
// session where every answered turn belongs to one model has no barriers, so
// same-model chains across identical-answer turns stay intact.
func answerBarriers(turns []turn, events []usageEvent) map[string][]span {
	names := make(map[string]bool)
	for _, e := range events {
		names[e.model] = true
	}
	byAnswer := map[string][]span{}
	for _, t := range turns {
		if t.answer == "" {
			continue
		}
		byAnswer[t.answer] = append(byAnswer[t.answer], t.span)
	}
	if len(byAnswer) <= 1 {
		return nil
	}
	out := make(map[string][]span, len(names))
	for name := range names {
		var foreign []span
		for ans, spans := range byAnswer {
			if ans != name {
				foreign = append(foreign, spans...)
			}
		}
		if len(foreign) == 0 {
			continue
		}
		out[name] = mergeSpans(foreign)
	}
	return out
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
// for a larger token-only copy (see piEventKeyN). A valid copy then beats one
// with a rejected figure, so a corrupt remainder never outranks the valid copy
// of the same turn. Then the larger token total wins, because a partial stream
// undercounts; a token tie breaks on the web-search count, which the catalog
// prices as real money, then on the larger self-priced cost. Either field alone
// can carry cost the other copy lacks.
type dedupeScore struct {
	tokens  int64
	web     int64
	cost    float64
	hasCost bool
	valid   bool
}

func (s dedupeScore) beats(o dedupeScore) bool {
	if s.hasCost != o.hasCost {
		return s.hasCost
	}
	if s.valid != o.valid {
		return s.valid
	}
	if s.tokens != o.tokens {
		return s.tokens > o.tokens
	}
	if s.web != o.web {
		return s.web > o.web
	}
	return s.cost > o.cost
}

// invalidTierCosts reports whether any harness-reported tier price is rejected.
func invalidTierCosts(c *tokenCosts) bool {
	return invalidCostPtr(c.input) || invalidCostPtr(c.output) || invalidCostPtr(c.cacheRead) || invalidCostPtr(c.cacheWrite)
}

func invalidCostPtr(v *float64) bool { return v != nil && invalidCost(*v) != "" }

// scoreOf reads the comparable size of one event for dedupe: token and
// web-search counts for catalog-priced turns, self-priced cost for Pi turns.
// A corrupt figure ranks as absent and marks the copy invalid, using the same
// validators as the fold, so it cannot outscore its valid sibling and bury real
// money. No warning fires here: folding stays the single warning and
// unknown-taint site.
func scoreOf(e usageEvent) dedupeScore {
	s := dedupeScore{tokens: e.tokens.total(), valid: e.tokens.corrupt == 0}
	if invalidCount(e.webSearch) == "" {
		s.web = e.webSearch
	} else {
		s.valid = false
	}
	if e.cost != nil {
		if invalidCost(*e.cost) == "" {
			s.cost, s.hasCost = *e.cost, true
		} else {
			s.valid = false
		}
	}
	if e.tierCost != nil && invalidTierCosts(e.tierCost) {
		s.valid = false
	}
	return s
}

// dedupeBest is a key's winning score across every copy, plus the winning score
// across the metered (uncovered) copies alone. hasUncovered is tracked
// independently of which copy supplies the overall winning score: a larger
// covered copy must not erase the fact that a metered twin proves the charge
// . Election prefers the best-scoring metered copy whenever one exists, so
// transcript placement never decides whether a subscription proves the charge.
type dedupeBest struct {
	score        dedupeScore
	uncovered    dedupeScore
	hasUncovered bool
}

// coveredProof reports whether one event's own provider and timestamp fall under
// its session's subscription proof. It is per-event, so a metered twin in one
// session is never hidden by a covered copy of the same turn in another.
type coveredProof func(usageEvent) bool

// dedupeKeyedEvents decides which occurrence of each billed turn prices. It
// returns a per-session billed mask: true marks the occurrence that contributes
// cost. Spans ignore the mask and fold every event, so dedupe never changes
// activity time. Keyless events always bill. The winner is the first occurrence
// of a key with the best score, except that a covered copy never beats a metered
// twin: when any uncovered copy of a key exists, only an uncovered copy can be
// elected, so billing stays at list price instead of substituting coverage for a
// duplicate whose provenance is unestablished. The caller keeps its event
// slices; only the returned masks describe the change.
func dedupeKeyedEvents(sessions [][]usageEvent, covered []coveredProof) [][]bool {
	best := bestScores(sessions, covered)
	masks := make([][]bool, len(sessions))
	for si, events := range sessions {
		masks[si] = electBilledMask(events, best, coveredOf(covered, si))
	}
	return masks
}

// coveredOf returns the session's coverage proof, defaulting to never-covered so
// a caller that passes no proofs treats every copy as metered.
func coveredOf(covered []coveredProof, si int) coveredProof {
	if si < len(covered) && covered[si] != nil {
		return covered[si]
	}
	return func(usageEvent) bool { return false }
}

// bestScores records the overall winning score per billed-turn key and the
// winning score among the metered (uncovered) copies alone. Coverage is read per
// event, not per session. The uncovered score is tracked on every uncovered
// copy, even one that loses to a covered twin: the existence of a metered copy
// is evidence that must survive the election regardless of which copy supplies
// the winning figures.
func bestScores(sessions [][]usageEvent, covered []coveredProof) map[string]dedupeBest {
	best := map[string]dedupeBest{}
	for si, events := range sessions {
		cov := coveredOf(covered, si)
		for _, e := range events {
			if e.key == "" {
				continue
			}
			s := scoreOf(e)
			w, ok := best[e.key]
			// A tie keeps the first occurrence as the winner.
			if !ok || s.beats(w.score) {
				w.score = s
			}
			if !cov(e) {
				if !w.hasUncovered || s.beats(w.uncovered) {
					w.uncovered = s
				}
				w.hasUncovered = true
			}
			best[e.key] = w
		}
	}
	return best
}

// electBilledMask marks the billed occurrence of each key. When a metered
// (uncovered) copy exists, the first copy at the best uncovered score wins the
// slot and covered copies are skipped: only a metered copy can prove the
// charge, regardless of which copy supplied the overall best score . When
// every copy is covered, the first copy at the best score wins, so equal
// duplicates later in the walk drop.
func electBilledMask(events []usageEvent, best map[string]dedupeBest, covered coveredProof) []bool {
	mask := make([]bool, len(events))
	for ei, e := range events {
		if e.key == "" {
			mask[ei] = true
			continue
		}
		w, ok := best[e.key]
		if !ok {
			continue
		}
		if w.hasUncovered {
			if covered(e) {
				continue // a covered copy never substitutes for a metered twin
			}
			target := w.uncovered
			if !scoreOf(e).beats(target) && !target.beats(scoreOf(e)) {
				mask[ei] = true
				delete(best, e.key)
			}
			continue
		}
		s := scoreOf(e)
		if !s.beats(w.score) && !w.score.beats(s) {
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
