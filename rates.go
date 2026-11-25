package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// rates.go — the public rate catalog. wtm holds no rates: it reads one public
// catalog, caches it, and prices the tokens a harness does not price itself
// (Claude Code). A missing catalog never fails status; it makes the derived
// cost unknown, which is the honest answer.

const (
	// ratesURL is the LiteLLM catalog: the de-facto public price list, with the
	// four cache classes, service-tier and long-context rates Claude Code needs.
	ratesURL = "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"
	// ratesEnv overrides the catalog path. Tests and offline users pin it.
	ratesEnv = "WTM_RATES"
	// ratesCacheTTL bounds staleness; a stale cache is still used when a
	// refresh fails, so a price change degrades to old numbers, never to none.
	ratesCacheTTL     = 24 * time.Hour
	ratesFetchTimeout = 15 * time.Second
	// ratesRetryCooldown bounds retries after a failed fetch. Without it, a
	// broken network would delay every status run by ratesFetchTimeout. A cache
	// write failure does not start a cooldown: the next run can still fetch and
	// price, so a broken cache never hides a working network.
	ratesRetryCooldown = 15 * time.Minute
	// ratesMaxBytes bounds every catalog body read, network and local. The real
	// catalog is a few MB; a larger body is a wrong endpoint or a hostile file,
	// and parsing a truncated catalog would silently drop models. The cap+1 read
	// lets the loader detect the overflow without buffering an unbounded body.
	ratesMaxBytes = 16 << 20
	// maxAttemptMarkerBytes bounds a cooldown marker read. A marker is a tiny
	// JSON object; a larger file is corruption, not a cause worth reading.
	maxAttemptMarkerBytes = 4 << 10
	// maxAttemptMessageBytes caps the recorded failure text printed to stderr,
	// so a corrupted marker cannot flood the terminal.
	maxAttemptMessageBytes = 512
	// longContextThreshold matches the catalog's *_above_200k_tokens fields: a
	// request whose input context exceeds the model's standard window is priced
	// at the above-200k rate when the catalog lists one.
	longContextThreshold = 200_000
	// maxCatalogRate bounds any catalog figure, in USD per token or per query.
	// The priciest public model is far below it, so a larger value is corrupt
	// data, not a price: it would turn a typo into a billion-dollar total.
	maxCatalogRate = 1.0
	// maxTokenCount bounds a session's recorded counts. A negative or absurd
	// count is corrupt data; arithmetic on it would yield a negative or an
	// overflowing price that reads as real money.
	maxTokenCount = 1e12
	// maxSessionCost bounds one harness-reported total or tier cost. The priciest
	// real session is far below it; a larger figure would read as a real bill.
	maxSessionCost = 1e6
	// unreadableCount is the transcript decoder's mark for an integer figure it
	// cannot read (an int64 overflow, a fraction in a count field). The decoder
	// keeps the record and names the field through invalidCount, so the class
	// reads unknown while its readable siblings survive. It is a sentinel, never
	// a count: sums of a few of them stay far inside int64.
	unreadableCount int64 = -1 << 60
	// unreadableText is the same mark for a string figure (a service tier, a
	// child model). It matches no catalog tier or model, so the affected class
	// prices unknown instead of defaulting.
	unreadableText = "\ufffdunreadable"
)

// warnSink reports one rejected catalog or session figure. Callers must route
// it through progressWarn so a nil renderer still reaches stderr.
type warnSink func(error)

func doWarnSink(warn warnSink, err error) {
	if warn != nil {
		warn(err)
	}
}

// progressWarn adapts a progress handle to the catalog warning seam. Warnings
// ride the renderer when one runs and fall back to plain stderr otherwise:
// money must not vanish silently in machine-output modes.
func progressWarn(p *progress) warnSink {
	return func(err error) { warnProgress(p, "%v", err) }
}

// dedupeWarn drops identical warning texts after the first, so one cause is
// disclosed once per run. The catalog retry path re-parses the same cache file
// after a failed refresh, so a per-run dedupe keeps the warning count honest.
func dedupeWarn(warn warnSink) warnSink {
	seen := map[string]bool{}
	return func(err error) {
		if err == nil || seen[err.Error()] {
			return
		}
		seen[err.Error()] = true
		doWarnSink(warn, err)
	}
}

// invalidAmount names why a figure cannot be money, in the unit the caller
// names. An empty result means valid; a listed zero is a real free class.
func invalidAmount(v float64, max float64, unit string) string {
	switch {
	case math.IsNaN(v):
		return "not a number"
	case math.IsInf(v, 0):
		return "number out of range"
	case v < 0:
		return "negative"
	case v > max:
		return fmt.Sprintf("above %g %s", max, unit)
	}
	return ""
}

func invalidCost(v float64) string { return invalidAmount(v, maxSessionCost, "USD") }

// addReportedCost trusts a harness-reported figure only inside the cost bound.
// A rejected figure taints the value as unknown and warns once, so corrupt
// money never renders as exact dollars.
func addReportedCost(m *money, v float64, field string, warn warnSink) {
	if cause := invalidCost(v); cause != "" {
		m.addUnknown()
		doWarnSink(warn, fmt.Errorf("%s: %s", field, cause))
		return
	}
	m.addKnown(v)
}

// invalidCount names why a count cannot be used; zero is a real "none".
func invalidCount(n int64) string {
	switch {
	case n == unreadableCount:
		return "unreadable"
	case n < 0:
		return "negative"
	case n > maxTokenCount:
		return fmt.Sprintf("above %g", float64(maxTokenCount))
	}
	return ""
}

// sanitizeTokens zeroes every rejected class and marks its tier, so neither the
// price nor the cell shows a corrupt count. The mark is sticky across folding.
func sanitizeTokens(t promptTokens, model string, warn warnSink) promptTokens {
	t.sanitize(&t.input, tierInput, model, "input", warn)
	t.sanitize(&t.output, tierOutput, model, "output", warn)
	t.sanitize(&t.cacheRead, tierCacheRead, model, "cacheRead", warn)
	t.sanitize(&t.cacheWrite5m, tierCacheWrite, model, "cacheWrite5m", warn)
	t.sanitize(&t.cacheWrite1h, tierCacheWrite, model, "cacheWrite1h", warn)
	t.sanitize(&t.unclassifiedCacheWrite, tierCacheWrite, model, "unclassifiedCacheWrite", warn)
	sanitizeWriteConflict(&t, model, warn)
	return t
}

// sanitizeWriteConflict rejects a cache-write split the parser found contradicted
// by its own stated total. The parser has no warning sink, so it flags the tier
// and this single sanitizing site names it and zeroes every write class.
func sanitizeWriteConflict(t *promptTokens, model string, warn warnSink) {
	if !t.conflicts.has(tierCacheWrite) {
		return
	}
	doWarnSink(warn, fmt.Errorf("model %s cache write split: contradicts the stated cache_creation total; count stays unknown", model))
	t.corrupt = t.corrupt.with(tierCacheWrite)
	t.cacheWrite5m, t.cacheWrite1h, t.unclassifiedCacheWrite = 0, 0, 0
	// Clear the flag so a second sanitize of the same events cannot re-emit the
	// warning: this site is the single-warning owner (see promptTokens.conflicts).
	t.conflicts = 0
}

func (t *promptTokens) sanitize(n *int64, id tier, model, class string, warn warnSink) {
	if cause := invalidCount(*n); cause != "" {
		doWarnSink(warn, fmt.Errorf("model %s token %s: %s; count stays unknown", model, class, cause))
		t.corrupt = t.corrupt.with(id)
		*n = 0
	}
}

// validateRate rejects one figure and keeps a nil field nil, so the affected
// token class reads unknown instead of negative or astronomical. unit names the
// price basis, so a per-query figure never warns in per-token terms.
func validateRate(model, field, unit string, v *float64, warn warnSink) *float64 {
	if v == nil {
		return nil
	}
	if cause := invalidAmount(*v, maxCatalogRate, unit); cause != "" {
		doWarnSink(warn, rateCatalogError(model, field, cause))
		return nil
	}
	return v
}

// rateCatalogError is the single phrasing for one rejected catalog figure:
// field locates it, cause says why, and the consequence names what turns
// unknown — a token class for a per-token rate, the search price for a
// per-query fee.
func rateCatalogError(model, field, cause string) error {
	consequence := "token class stays unknown"
	if strings.Contains(field, "webSearch") || strings.Contains(field, "search_context_cost_per_query") {
		consequence = "search price stays unknown"
	}
	return fmt.Errorf("rate catalog %s.%s: %s; %s", model, field, cause, consequence)
}

// validateRates applies validateRate to every field of one price set. prefix
// names the set (standard, batch, above_200k) so the warning locates the field.
func validateRates(model, prefix string, r tokenRates, warn warnSink) tokenRates {
	return tokenRates{
		input:        validateRate(model, prefix+"input", "USD/token", r.input, warn),
		output:       validateRate(model, prefix+"output", "USD/token", r.output, warn),
		cacheRead:    validateRate(model, prefix+"cacheRead", "USD/token", r.cacheRead, warn),
		cacheWrite5m: validateRate(model, prefix+"cacheWrite5m", "USD/token", r.cacheWrite5m, warn),
		cacheWrite1h: validateRate(model, prefix+"cacheWrite1h", "USD/token", r.cacheWrite1h, warn),
	}
}

// validateEntry validates one catalog entry. hasAbove stays as decoded: a
// rejected above-200k figure leaves its class unknown, and dropping the whole
// set would silently misprice a valid sibling figure. webSearch is a
// per-query price, so it shares the money ceiling.
func validateEntry(name string, r modelRate, warn warnSink) modelRate {
	standard := validateRates(name, "standard.", r.standard, warn)
	return modelRate{
		standard:    standard,
		batch:       validateRates(name, "batch.", r.batch, warn),
		above:       validateRates(name, "above_200k.", fillAboveFromStandard(r.above, standard, r.brokenAbove), warn),
		hasAbove:    r.hasAbove,
		webSearch:   validateRate(name, "webSearch", "USD/query", r.webSearch, warn),
		brokenAbove: r.brokenAbove,
	}
}

// fillAboveFromStandard fills an unlisted above-200k class from the validated
// standard rate. A stated figure that failed to decode (broken) passes through
// as nil, so that class reads unknown instead of silently borrowing the cheaper
// standard rate; valid listed figures pass through to validation untouched.
func fillAboveFromStandard(above, standard, broken tokenRates) tokenRates {
	if broken.input == nil {
		mergeRate(&above.input, standard.input)
	}
	if broken.output == nil {
		mergeRate(&above.output, standard.output)
	}
	if broken.cacheRead == nil {
		mergeRate(&above.cacheRead, standard.cacheRead)
	}
	if broken.cacheWrite5m == nil {
		mergeRate(&above.cacheWrite5m, standard.cacheWrite5m)
	}
	if broken.cacheWrite1h == nil {
		mergeRate(&above.cacheWrite1h, standard.cacheWrite1h)
	}
	return above
}

// errRateCatalogTooLarge marks a catalog body over ratesMaxBytes, so callers and
// tests can recognise the refusal instead of treating it as a parse failure.
var errRateCatalogTooLarge = errors.New("rate catalog exceeds size limit")

// errNoPricedModels marks a catalog that decoded but holds no priced model, so
// a caller can distinguish an empty catalog from a malformed one.
var errNoPricedModels = errors.New("rate catalog has no priced models")

// errRateCatalogCoolingDown marks a skipped refresh whose last failure is too
// recent. The marker carries the cause, so a cooled-down run never mistakes the
// cooldown for a missing cache.
var errRateCatalogCoolingDown = errors.New("rate catalog refresh is cooling down after a recent failure")

// rateAttemptRecord is the cooldown marker's body. It carries the failure cause
// across process boundaries, so the next run can report why the refresh stopped.
type rateAttemptRecord struct {
	Message  string `json:"message"`
	TooLarge bool   `json:"tooLarge,omitempty"`
}

// tokenRates is one price set. Missing fields are nil, not zero: a catalog that
// lists no rate must read as unknown, while a listed 0 is a real free class.
type tokenRates struct {
	input        *float64
	output       *float64
	cacheRead    *float64
	cacheWrite5m *float64
	cacheWrite1h *float64
}

// any reports whether any price in the set is present.
func (r tokenRates) any() bool {
	return r.input != nil || r.output != nil || r.cacheRead != nil || r.cacheWrite5m != nil || r.cacheWrite1h != nil
}

// modelRate is one catalog entry: the two service tiers, the long-context set
// and the per-request web search price.
type modelRate struct {
	standard  tokenRates
	batch     tokenRates
	above     tokenRates
	hasAbove  bool
	webSearch *float64
	// brokenAbove marks the above-200k fields the row stated but could not
	// decode. They read unknown instead of borrowing the standard rate, so a
	// malformed long-context figure never prices a long prompt at the cheaper
	// standard rate while the warning claims the class stays unknown.
	brokenAbove tokenRates
}

// ratesFor picks the tier and context set. nil means the pair is unpriced, so
// the record reads unknown rather than silently mispriced.
func (r modelRate) ratesFor(tier string, t promptTokens) *tokenRates {
	above, ok := r.aboveContext(t)
	if !ok {
		return nil
	}
	switch tier {
	case "", "standard", "default":
		return r.standardRates(above)
	case "batch":
		// The catalog has no batch+long-context rate. Combining two rate
		// adjustments would invent a price, so report unknown.
		if above {
			return nil
		}
		return &r.batch
	default:
		return nil
	}
}

// aboveContext selects the long-context rates. A rejected prompt count leaves
// the context unknown, so choosing a set would invent a price; ok=false refuses
// the event. Without long rates the standard set is context-free, so a model
// with only standard rates still prices valid sibling tokens.
func (r modelRate) aboveContext(t promptTokens) (above bool, ok bool) {
	if !r.hasAbove {
		return false, true
	}
	if t.corruptPrompt() {
		return false, false
	}
	return t.context() > longContextThreshold, true
}

// standardRates selects the standard set, or the long-context set when the
// prompt exceeds the threshold. validateEntry already filled the absent
// above-200k classes, so pricing needs no merge.
func (r modelRate) standardRates(above bool) *tokenRates {
	if !above {
		return &r.standard
	}
	return &r.above
}

// mergeRate keeps a listed rate and fills an unlisted one from base.
func mergeRate(dst **float64, base *float64) {
	if *dst == nil {
		*dst = base
	}
}

// catalog maps a model name to its rate.
type catalog map[string]modelRate

// estimateCacheWriteLifetime is the exact reason a cost reads estimated. wtm
// prints it verbatim in the status legend, and a test asserts it, so the stated
// reason can never drift from the estimate it explains.
const estimateCacheWriteLifetime = "a usage record omitted the cache-write lifetime (5m vs 1h), so wtm priced those tokens at the 5-minute rate; a 1-hour write costs more, so the value is a lower bound, not exact data"

// priceWith turns one event into its price pair and per-tier breakdown and
// reports rejected derived amounts through warn (nil keeps them silent). A
// resolved model with zero tokens is a known $0.00; a missing model, tier, or
// needed class is unknown. A record that omits the cache-write lifetime is
// priced at the 5-minute rate and marked estimated, because the true 5m/1h
// split is unknowable. The billed side mirrors the list here; the subscription
// substitution (usagePricer) replaces billed per tier. A corrupt web-search
// count is named before the rate lookup, so an event no rate set can price
// still discloses it.
func (c catalog) priceWith(e usageEvent, warn warnSink) pricedEvent {
	searchCorrupt := rejectSearchCount(&e, warn)
	r, ok := resolveModel(c, e.model)
	if !ok {
		return unpriced()
	}
	rates := r.ratesFor(e.tier, e.tokens)
	if rates == nil {
		return unpriced()
	}
	return priceRates(r, *rates, e, searchCorrupt, warn)
}

// priceRates prices one event against its resolved rate set: a zero-token event
// is a known $0.00, every rejected tier taints its class, and the web-search
// count joins the total only (it is priced per query, not per token). A rejected
// search count leaves the total unknown even when the tokens priced fine. The
// billed side mirrors the list here; usagePricer substitutes coverage per tier.
func priceRates(r modelRate, rates tokenRates, e usageEvent, searchCorrupt bool, warn warnSink) pricedEvent {
	p := pricedEvent{tiers: priceTiers(rates, e.tokens, e.model, warn)}
	p.tiers.taintCorrupt(e.tokens.corrupt)
	p.total.list.addKnown(0)           // resolved: a zero is a real zero
	p.total.list.add(p.tiers.totals()) // a tainted tier taints the total
	priceWebSearch(&p.total.list, r, e, warn)
	if searchCorrupt {
		p.total.list.addUnknown()
	}
	if cause := invalidCost(p.total.list.amount); cause != "" && p.total.list.known() {
		doWarnSink(warn, fmt.Errorf("model %s event price: %s; cost stays unknown", e.model, cause))
		p.total.list.addUnknown()
	}
	p.total.billed = p.total.list
	return p
}

// unpriced is an event with no resolvable rate: both sides unknown and no tiers.
func unpriced() pricedEvent {
	var p pricedEvent
	p.total.list.addUnknown()
	p.total.billed.addUnknown()
	return p
}

// priceTiers prices each token class into its displayed tier. The cache-write
// classes (5m, 1h) collapse into the single write tier, while an unclassified
// write is priced at the 5-minute rate and marked estimated. The billed side
// starts equal to the list; the subscription substitution replaces it later.
func priceTiers(r tokenRates, t promptTokens, model string, warn warnSink) tokenTiers {
	var tiers tokenTiers
	addClass(&tiers[tierInput].list, r.input, t.input, model, "input", warn)
	addClass(&tiers[tierOutput].list, r.output, t.output, model, "output", warn)
	addClass(&tiers[tierCacheRead].list, r.cacheRead, t.cacheRead, model, "cacheRead", warn)
	addClass(&tiers[tierCacheWrite].list, r.cacheWrite5m, t.cacheWrite5m, model, "cacheWrite5m", warn)
	addClass(&tiers[tierCacheWrite].list, r.cacheWrite1h, t.cacheWrite1h, model, "cacheWrite1h", warn)
	addEstimatedClass(&tiers[tierCacheWrite].list, r.cacheWrite5m, t.unclassifiedCacheWrite, model, "unclassifiedCacheWrite", warn)
	for i := range tiers {
		tiers[i].billed = tiers[i].list
	}
	return tiers
}

// taintCorrupt marks each rejected tier unknown on both sides, so a zeroed count
// never renders as a partial exact price while good sibling classes keep theirs.
func (t *tokenTiers) taintCorrupt(mask tierMask) {
	for id := tier(0); id < numTiers; id++ {
		if mask.has(id) {
			t[id].addUnknown()
		}
	}
}

// rejectSearchCount names a corrupt web-search count before any rate lookup, so
// a model or context that cannot resolve cannot skip the disclosure. The count
// is zeroed for the price path, which would warn again on the same figure; the
// result reports whether the total must read unknown.
func rejectSearchCount(e *usageEvent, warn warnSink) bool {
	cause := invalidCount(e.webSearch)
	if cause == "" {
		return false
	}
	doWarnSink(warn, countCauseError(e.model, "webSearch", cause))
	e.webSearch = 0
	return true
}

// countCauseError is the single phrasing for a rejected count, so the early
// search disclosure and the pricing path never warn twice for one figure.
func countCauseError(model, class, cause string) error {
	return fmt.Errorf("model %s %s: %s; count stays unknown", model, class, cause)
}

func priceWebSearch(m *money, r modelRate, e usageEvent, warn warnSink) {
	priceClass(m, r.webSearch, e.webSearch, e.model, "webSearch", warn, 0)
}

func addClass(m *money, rate *float64, n int64, model, class string, warn warnSink) {
	priceClass(m, rate, n, model, class, warn, 0)
}

// addEstimatedClass prices a class whose split is unknown, at an assumed rate.
// A nil rate leaves the part unknown: without any basis, an estimate would
// invent a number, which is worse than an honest gap.
func addEstimatedClass(m *money, rate *float64, n int64, model, class string, warn warnSink) {
	priceClass(m, rate, n, model, class, warn, causeCacheWrite)
}

// priceClass prices one catalog class under the maxSessionCost ceiling: an
// accepted count times an accepted rate can still exceed it. cause marks an assumed price.
func priceClass(m *money, rate *float64, n int64, model, class string, warn warnSink, cause estimateCause) {
	if n == 0 {
		return
	}
	v, ok := classProduct(m, rate, n, model, class, warn)
	switch {
	case !ok:
		return
	case cause != 0:
		m.addEstimated(v, cause)
	default:
		m.addKnown(v)
	}
}

// classProduct validates one class's count, rate, and amount; ok reports whether a price exists.
// The rate is revalidated at this pricing boundary , so a rate that reached
// multiplication without catalog validation can never produce a wrong exact price.
func classProduct(m *money, rate *float64, n int64, model, class string, warn warnSink) (float64, bool) {
	cause := invalidCount(n)
	if cause != "" || rate == nil {
		if cause != "" {
			doWarnSink(warn, countCauseError(model, class, cause))
		}
		m.addUnknown()
		return 0, false
	}
	if c := invalidAmount(*rate, maxCatalogRate, "USD/token"); c != "" {
		doWarnSink(warn, fmt.Errorf("model %s %s price: %s; cost stays unknown", model, class, c))
		m.addUnknown()
		return 0, false
	}
	v := float64(n) * *rate
	if c := invalidCost(v); c != "" {
		doWarnSink(warn, fmt.Errorf("model %s %s price: %s; cost stays unknown", model, class, c))
		m.addUnknown()
		return 0, false
	}
	return v, true
}

// resolveModel finds a rate for a recorded model id. The catalog uses many
// aliases (provider prefixes, version/date suffixes), so it tries the exact
// name first, then normalized candidates. The reserved unknown label is not a
// model id: an unnamed session must never borrow a pinned "unknown" catalog entry.
func resolveModel(c catalog, name string) (modelRate, bool) {
	if name == unknownValue {
		return modelRate{}, false
	}
	for _, candidate := range modelCandidates(name) {
		if r, ok := c[candidate]; ok {
			return r, true
		}
	}
	return modelRate{}, false
}

// modelCandidates lists lookup keys in priority order: the exact name, then the
// base after a provider path (slash) or provider dot ("anthropic.claude-…"),
// then each with its version/date suffix stripped. The dot split is guarded to
// an alphabetic prefix: a version dot ("gpt-4.1", "gemini-2.5-flash") must not
// emit a fragment that could match a different catalog key.
func modelCandidates(name string) []string {
	seen := map[string]bool{}
	var out []string
	bases := []string{name}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		bases = append(bases, name[i+1:])
	}
	if i := strings.LastIndex(name, "."); i >= 0 && allLetters(name[:i]) {
		bases = append(bases, name[i+1:])
	}
	for _, b := range bases {
		out = appendCandidate(out, seen, b)
		out = appendCandidate(out, seen, stripModelSuffix(b))
	}
	return out
}

// appendCandidate adds a non-empty, unseen candidate in place.
func appendCandidate(out []string, seen map[string]bool, s string) []string {
	s = strings.TrimSpace(s)
	if s == "" || seen[s] {
		return out
	}
	seen[s] = true
	return append(out, s)
}

// stripModelSuffix removes trailing qualifiers (":qualifier", "-vN",
// "-YYYYMMDD"), looping until stable so chained qualifiers
// ("model-v2-20240101") strip fully.
func stripModelSuffix(s string) string {
	for {
		next := stripModelSuffixOnce(s)
		if next == s {
			return s
		}
		s = next
	}
}

// stripModelSuffixOnce removes one trailing qualifier.
func stripModelSuffixOnce(s string) string {
	if i := strings.IndexAny(s, "@:"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "-v"); i >= 0 && allDigits(s[i+2:]) {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "-"); i >= 0 && len(s[i+1:]) == 8 && allDigits(s[i+1:]) {
		s = s[:i]
	}
	return s
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// allLetters reports a purely alphabetic string, so a dot split only fires on
// a provider prefix, never on a version fragment.
func allLetters(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

// litellmEntry is the catalog subset wtm reads. Every price is a pointer so an
// absent field stays distinguishable from a listed zero. The long-context block
// keeps its broken siblings separately: a stated-but-undecodable figure marks
// its sibling slot, so that class reads unknown instead of borrowing the
// standard rate.
type litellmEntry struct {
	standard    tokenRates
	batch       tokenRates
	above       tokenRates
	aboveBroken tokenRates
	Search      map[string]json.RawMessage
}

func (e litellmEntry) toRate(name string, warn warnSink) modelRate {
	return modelRate{
		standard:    e.standard,
		batch:       e.batch,
		above:       e.above,
		hasAbove:    e.above.any(),
		brokenAbove: e.aboveBroken,
		webSearch:   searchRate(e.Search, name, warn),
	}
}

// searchRate picks a per-query price. Anthropic lists one price for every
// context size, so a split catalog resolves in a fixed order (medium, low,
// high). The first stated size is authoritative: a stated but malformed figure
// is rejected with one warning and never borrows a lower-priority price, so the
// class reads unknown. Every other stated size stays subject to the same
// validation, so a malformed non-authoritative figure still warns even when the
// authoritative size is broken.
func searchRate(m map[string]json.RawMessage, model string, warn warnSink) *float64 {
	var first *float64
	authoritativeBroken := false
	for _, key := range []string{"search_context_size_medium", "search_context_size_low", "search_context_size_high"} {
		raw, ok := m[key]
		if !ok || !statesValue(raw) {
			continue // absent and null both mean no price
		}
		v, valid := decodeSearchRate(raw, key, model, warn)
		if first == nil {
			if valid {
				first = v
			} else {
				authoritativeBroken = true
			}
		}
	}
	if authoritativeBroken {
		return nil
	}
	return first
}

// decodeSearchRate reads one stated search size. A value that is not a JSON
// number, or that falls outside the money bound, warns once and is rejected; a
// valid one returns its value. The raw value is unmarshaled exactly once.
func decodeSearchRate(raw json.RawMessage, key, model string, warn warnSink) (*float64, bool) {
	var v float64
	if json.Unmarshal(raw, &v) != nil {
		doWarnSink(warn, rateCatalogError(model, "webSearch."+key, malformedCause(raw, true)))
		return nil, false
	}
	if cause := invalidAmount(v, maxCatalogRate, "USD/query"); cause != "" {
		doWarnSink(warn, rateCatalogError(model, "webSearch."+key, cause))
		return nil, false
	}
	return &v, true
}

// catalogFigures is the per-figure decode seam for one catalog row. It mirrors
// rawFields' individual-field pardon: a malformed figure stays unknown (nil)
// and is reported, never voiding its siblings. Only a JSON number claims a
// price, so the catalog's prose sample_spec row stays a non-model.
type catalogFigures struct {
	f      rawFields
	priced bool
	bad    []badFigure
}

// amount decodes one optional price figure. A JSON number claims a price even
// when this build cannot read it; prose claims nothing. A malformed figure
// stays nil so its class reads unknown.
func (c *catalogFigures) amount(key string) *float64 {
	raw, ok := c.f.value(key)
	if !ok {
		return nil
	}
	if isJSONNumber(raw) {
		c.priced = true
	}
	if v, ok := c.decodeNumber(raw, key); ok {
		return v
	}
	return nil
}

// decodeNumber decodes one stated number figure. A figure this build cannot
// hold — an overflow, or a negative-zero underflow like -1e-999 that rounds to
// -0 — records a bad figure and reports false, so the class reads unknown. An
// explicit numeric zero is a valid, exact free class.
func (c *catalogFigures) decodeNumber(raw json.RawMessage, key string) (*float64, bool) {
	v := new(float64)
	if json.Unmarshal(raw, v) != nil || (*v == 0 && math.Signbit(*v)) {
		c.bad = append(c.bad, badFigure{key: key, cause: malformedCause(raw, true)})
		return nil, false
	}
	return v, true
}

// search decodes the optional search-price object, keeping its members raw so
// searchRate can validate each stated size. A non-object is a malformed figure.
func (c *catalogFigures) search(key string) map[string]json.RawMessage {
	raw, ok := c.f.value(key)
	if !ok {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		c.bad = append(c.bad, badFigure{key: key, cause: malformedCause(raw, false)})
		return nil
	}
	return m
}

// statesValue reports whether an object member states a figure at all: an absent
// key and a JSON null both mean no value. It backs the catalog's per-figure
// decode and the transcript decoders that share rawFields.
func statesValue(v json.RawMessage) bool { return len(v) > 0 && string(v) != "null" }

// isJSONNumber reports whether a figure is written as a JSON number. A number
// claims a price even when this build cannot use it; prose ("float", in the
// catalog's sample_spec row) claims nothing.
func isJSONNumber(v json.RawMessage) bool {
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case ' ', '\t', '\n', '\r':
		default:
			return c == '-' || ('0' <= c && c <= '9')
		}
	}
	return false
}

// malformedCause names why one figure cannot be read, in the unit the field
// expects, so stderr never carries a Go type name.
func malformedCause(v json.RawMessage, price bool) string {
	switch {
	case !price:
		return "not a price table" // search_context_cost_per_query holds an object
	case isJSONNumber(v):
		return "number out of range"
	default:
		return "not a number"
	}
}

// badFigure names one catalog key whose figure could not be read, and why.
type badFigure struct{ key, cause string }

// parseCatalog reads the catalog object. Entries that are not priced models
// (the sample spec, comment rows) are skipped instead of failing the parse.
func parseCatalog(r io.Reader, warn warnSink) (catalog, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}
	c := catalog{}
	for name, v := range raw {
		if rate, ok := pricedEntry(name, v, warn); ok {
			c[name] = validateEntry(name, rate, warn)
		}
	}
	if len(c) == 0 {
		return nil, errNoPricedModels
	}
	return c, nil
}

// pricedEntry decodes one catalog row figure by figure, reporting false for rows
// that claim no price. A malformed figure keeps its siblings and leaves its
// class unknown, so the exact row stays the match and no alias row supplies its
// price.
func pricedEntry(name string, raw json.RawMessage, warn warnSink) (modelRate, bool) {
	f, err := rawObject(raw)
	if err != nil {
		return modelRate{}, false // not an object: a spec or comment row
	}
	if isDocName(name) {
		return modelRate{}, false // documentation row: prose claims no price
	}
	e, c := decodeLitellm(f)
	return finishEntry(name, e, c, warn)
}

// decodeLitellm reads one catalog row figure by figure (see catalogFigures).
func decodeLitellm(f rawFields) (litellmEntry, catalogFigures) {
	c := catalogFigures{f: f}
	var above, aboveBroken tokenRates
	for i := range aboveFieldKeys {
		c.above(i, &above, &aboveBroken)
	}
	return litellmEntry{
		standard: tokenRates{
			input: c.amount("input_cost_per_token"), output: c.amount("output_cost_per_token"),
			cacheRead:    c.amount("cache_read_input_token_cost"),
			cacheWrite5m: c.amount("cache_creation_input_token_cost"),
			cacheWrite1h: c.amount("cache_creation_input_token_cost_above_1hr"),
		},
		batch: tokenRates{
			input: c.amount("input_cost_per_token_batches"), output: c.amount("output_cost_per_token_batches"),
			cacheRead:    c.amount("cache_read_input_token_cost_batches"),
			cacheWrite5m: c.amount("cache_creation_input_token_cost_batches"),
		},
		above:       above,
		aboveBroken: aboveBroken,
		Search:      c.search("search_context_cost_per_query"),
	}, c
}

// aboveFieldKeys names the long-context catalog keys in slot order. It is the
// single place the key-to-set mapping lives: the decode drives both the value
// slot and its broken sibling off the same index.
var aboveFieldKeys = [...]string{
	"input_cost_per_token_above_200k_tokens",
	"output_cost_per_token_above_200k_tokens",
	"cache_read_input_token_cost_above_200k_tokens",
	"cache_creation_input_token_cost_above_200k_tokens",
	"cache_creation_input_token_cost_above_1hr_above_200k_tokens",
}

// slot returns a pointer to the tokenRates slot at aboveFieldKeys index i.
func (r *tokenRates) slot(i int) **float64 {
	switch i {
	case 0:
		return &r.input
	case 1:
		return &r.output
	case 2:
		return &r.cacheRead
	case 3:
		return &r.cacheWrite5m
	case 4:
		return &r.cacheWrite1h
	}
	panic("aboveFieldKeys index out of range")
}

// above decodes one stated long-context figure into its slot and marks the
// sibling broken slot when the figure cannot be decoded: both destinations are
// set from one decode of the same raw value. An absent or null figure is
// unstated, so nothing is set.
func (c *catalogFigures) above(i int, value, broken *tokenRates) {
	key := aboveFieldKeys[i]
	raw, ok := c.f.value(key)
	if !ok {
		return
	}
	if isJSONNumber(raw) {
		c.priced = true
	}
	if v, ok := c.decodeNumber(raw, key); ok {
		*value.slot(i) = v
		return
	}
	*broken.slot(i) = &brokenAboveMarker
}

// finishEntry classifies a decoded row: prose stays a silent non-model, a
// corrupt exact entry stays a model with unknown classes, and a priced row
// warns per malformed figure.
func finishEntry(name string, e litellmEntry, c catalogFigures, warn warnSink) (modelRate, bool) {
	rate := e.toRate(name, warn)
	rate.brokenAbove = e.aboveBroken
	rate.hasAbove = rate.hasAbove || rate.brokenAbove.any()
	if !c.priced {
		if len(c.bad) > 0 && !isDocName(name) {
			warnBad(name, c.bad, warn)
			return rate, true
		}
		return modelRate{}, false
	}
	warnBad(name, c.bad, warn)
	return rate, true
}

// warnBad reports each undecodable figure of one catalog row.
func warnBad(name string, bad []badFigure, warn warnSink) {
	for _, b := range bad {
		doWarnSink(warn, rateCatalogError(name, b.key, b.cause))
	}
}

// isDocName reports whether a catalog key is documentation rather than a model:
// the sample spec row and comment rows. Their prose values claim no price, so
// they stay silent non-models even when their figures cannot be decoded.
func isDocName(name string) bool {
	return name == "sample_spec" || strings.HasPrefix(name, "#")
}

// brokenAboveMarker is the shared non-nil token stored in brokenAbove slots to
// mark a stated-but-undecodable above-200k field; only nil-ness is ever used.
var brokenAboveMarker float64

// catalogSource names where a resolved catalog came from, so progress can say
// whether the run waited on the network. It is a label, not a decision input.
type catalogSource string

const (
	catalogEnv      catalogSource = "file"
	catalogCache    catalogSource = "cache"
	catalogFetching catalogSource = "fetching over network"
	catalogFetched  catalogSource = "fetched"
	catalogStale    catalogSource = "stale cache"
)

// loadCatalog resolves the catalog: env override, fresh cache, network fetch,
// then a stale cache as the last resort. The returned error means no catalog is
// available at all, so callers report unknown costs.
func loadCatalog() (catalog, error) { return loadCatalogFrom(ratesURL) }

// loadCatalogReporting is loadCatalog that announces each resolution point, so
// a long network fetch is visible instead of looking like a hang. The handle is
// also the warning sink: fetch failures inside the renderer must ride it.
func loadCatalogReporting(p *progress, now time.Time) (catalog, error) {
	return loadCatalogFromNotify(ratesURL, p, now)
}

// loadCatalogFrom is loadCatalog with an injectable source URL, so tests drive
// the real fetch chain against a local server instead of the network.
func loadCatalogFrom(url string) (catalog, error) { return loadCatalogFromNotify(url, nil, nowFunc()) }

func loadCatalogFromNotify(url string, p *progress, now time.Time) (catalog, error) {
	if path := os.Getenv(ratesEnv); path != "" {
		p.detail(string(catalogEnv))
		return parseCatalogFile(path, progressWarn(p))
	}
	path, err := ratesCachePath()
	if err != nil {
		return nil, err
	}
	return loadFromCachePath(path, url, p, now)
}

// loadFromCachePath resolves a fresh cache, then a fetch, then the stale cache
// as the last resort. One deduplicated sink is shared by every parse of the
// same cache file, so a retry cannot repeat a warning.
func loadFromCachePath(path, url string, p *progress, now time.Time) (catalog, error) {
	warn := dedupeWarn(progressWarn(p))
	c, fresh, cacheUnusable := readFreshCache(path, warn, now)
	if fresh {
		p.detail(string(catalogCache))
		return c, nil
	}
	if err := p.abort(); err != nil {
		return nil, err
	}
	c, fetchErr := tryFetch(path, url, p, cacheUnusable, warn, now)
	if fetchErr == nil {
		return c, nil
	}
	return staleCatalog(path, warn, p, fetchErr, now)
}

// tryFetch refreshes the cache or reports why it could not. A failed fetch
// starts a cooldown to avoid repeated stalls. cacheUnusable is a cache path
// whose stat already failed, so the write step stays silent: readFreshCache
// disclosed that cause once, with its net effect.
func tryFetch(path, url string, p *progress, cacheUnusable bool, warn warnSink, now time.Time) (catalog, error) {
	if cooling, cause := fetchCoolingDownAt(path, now); cooling {
		if errors.Is(cause, errRateCatalogCoolingDown) {
			return nil, cause
		}
		return nil, fmt.Errorf("%w: %w", errRateCatalogCoolingDown, cause)
	}
	p.detail(string(catalogFetching))
	c, err := fetchAndCache(path, url, p, cacheUnusable, warn)
	if err == nil {
		p.detail(string(catalogFetched))
		return c, nil
	}
	if markErr := markFetchAttempt(path, err); markErr != nil {
		warnProgress(p, "rate cooldown marker write failed: %s; the next run may retry the fetch", markErr)
	}
	return nil, err
}

// staleCatalog serves the last good catalog when the refresh failed. Serving old
// prices is a disclosure, not a silent degradation: one persistent warning names
// the fetch cause and the cache age so the user can judge the numbers. When the
// cached copy itself fails to parse, both distinct causes are reported once (f6,
// f15). warn is the run's deduplicated sink, so re-parsing the same cache file
// cannot repeat corrupt-figure warnings.
func staleCatalog(path string, warn warnSink, p *progress, fetchErr error, now time.Time) (catalog, error) {
	c, err := parseCatalogFile(path, warn)
	if err == nil {
		p.detail(string(catalogStale))
		warnProgress(p, "rate catalog refresh failed: %s; using the cached copy (%s)", fetchErr, cacheAgeLabel(path, now))
		return c, nil
	}
	// A missing cache is the normal cold start: stay silent, since the fetch
	// cause is the real failure. Any other parse failure names the cache once —
	// the same deduplicated text readFreshCache uses, so a fresh-corrupt cache
	// that was already disclosed stays a single report.
	if !errors.Is(err, os.ErrNotExist) {
		doWarnSink(warn, fmt.Errorf("%s: %w; refetching", path, err))
	}
	if fetchErr != nil {
		return nil, fetchErr
	}
	return nil, err
}

func fetchCoolingDownAt(path string, now time.Time) (bool, error) {
	if cooling, cause := recentAttemptCause(attemptPath(path), now); cooling {
		return true, cause
	}
	fallback := fallbackAttemptPath(path)
	// Avoid creating the private directory on the common path where no fallback
	// marker exists.
	if _, err := os.Lstat(fallback); err != nil {
		return false, nil
	}
	dir, err := ensureFallbackDir()
	if err != nil {
		return false, nil
	}
	return recentAttemptCause(filepath.Join(dir, fallbackAttemptName(path)), now)
}

// recentAttemptCause reports a fresh marker and its cause. A future mtime is
// invalid, not a long cooldown; a non-regular file is never opened. Expired or
// invalid markers are removed so they cannot leak forever.
func recentAttemptCause(path string, now time.Time) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, nil
	}
	if !info.Mode().IsRegular() {
		_ = os.Remove(path)
		return false, nil
	}
	age := now.Sub(info.ModTime())
	if age < 0 || age >= ratesRetryCooldown {
		_ = os.Remove(path)
		return false, nil
	}
	return true, readAttemptCause(path)
}

// readAttemptCause reconstructs the failure a marker recorded. A missing,
// unreadable, or oversized body is still a cooldown, so callers never fall
// through to ENOENT.
func readAttemptCause(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return errRateCatalogCoolingDown
	}
	data, err := readFileLimited(path, maxAttemptMarkerBytes)
	if err != nil {
		return errRateCatalogCoolingDown
	}
	var rec rateAttemptRecord
	if json.Unmarshal(data, &rec) != nil || rec.Message == "" {
		return errRateCatalogCoolingDown
	}
	if rec.TooLarge {
		return errRateCatalogTooLarge
	}
	return errors.New(sanitizeAttemptMessage(rec.Message))
}

// sanitizeAttemptMessage strips terminal control bytes and caps the length, so
// a corrupted marker cannot inject escapes into stderr.
func sanitizeAttemptMessage(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f) {
			continue
		}
		// Skip runes that do not fit whole, so the cap is exact and no rune splits.
		if n := utf8.RuneLen(r); n < 0 || b.Len()+n > maxAttemptMessageBytes {
			break
		}
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		return errRateCatalogCoolingDown.Error()
	}
	return b.String()
}

// markFetchAttempt records a fetch failure and its cause. A marker-write
// failure must not prevent pricing, so the caller continues; it must disclose
// the error, because without a marker every later run retries the fetch and
// waits out the timeout with no named cause. When the cache directory is
// unwritable (a file named .wtm, a read-only home), a marker in the fallback
// directory still arms the cooldown after ensureFallbackDir validates the
// directory.
func markFetchAttempt(path string, cause error) error {
	rec := rateAttemptRecord{Message: cause.Error(), TooLarge: errors.Is(cause, errRateCatalogTooLarge)}
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	err = writeFileAtomic(attemptPath(path), data, 0o600)
	if err == nil {
		return nil
	}
	dir, dirErr := ensureFallbackDir()
	if dirErr != nil {
		return fmt.Errorf("attempt marker: %w (fallback: %v)", err, dirErr)
	}
	if fallbackErr := writeFileAtomic(filepath.Join(dir, fallbackAttemptName(path)), data, 0o600); fallbackErr != nil {
		return fmt.Errorf("attempt marker: %w (fallback: %v)", err, fallbackErr)
	}
	return nil
}

func attemptPath(path string) string { return path + ".attempt" }

// fallbackAttemptDir locates cooldown markers when the cache directory is
// unwritable. userTempDirName provides the platform-specific path component;
// callers use ensureFallbackDir to validate the directory before use.
func fallbackAttemptDir() string {
	return filepath.Join(os.TempDir(), userTempDirName())
}

// ensureFallbackDir creates or validates the fallback directory. It rejects
// non-directories and symlinks without chmodding existing paths. Unix requires
// mode 0700 owned by the current user (privateFallbackDir); Windows relies on
// the temp directory ACL.
func ensureFallbackDir() (string, error) {
	dir := fallbackAttemptDir()
	info, err := os.Lstat(dir)
	if err == nil {
		return validateFallbackDir(dir, info)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if err := os.Mkdir(dir, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err = os.Lstat(dir)
	if err != nil {
		return "", err
	}
	return validateFallbackDir(dir, info)
}

func validateFallbackDir(dir string, info os.FileInfo) (string, error) {
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !privateFallbackDir(info) {
		return "", fmt.Errorf("rate marker dir is not a private directory")
	}
	return dir, nil
}

// fallbackAttemptPath returns the fallback marker path for a cache path. The
// hash keeps the name short and unique per cache path.
func fallbackAttemptPath(path string) string {
	return filepath.Join(fallbackAttemptDir(), fallbackAttemptName(path))
}

func fallbackAttemptName(path string) string {
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("wtm-rates-attempt-%x", sum[:8])
}

// clearAttemptMarkers removes both cooldown markers after a successful fetch,
// so a marker file cannot outlive the failure it records. The fallback marker
// is removed only after ensureFallbackDir validates its directory.
func clearAttemptMarkers(path string) {
	_ = os.Remove(attemptPath(path))
	fallback := fallbackAttemptPath(path)
	if _, err := os.Lstat(fallback); err != nil {
		return
	}
	dir, err := ensureFallbackDir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, fallbackAttemptName(path)))
}

// readFreshCache serves the cache when it is inside its TTL and parses cleanly.
// A missing cache is silent: the first run refetches. Any other stat or parse
// failure names the path and cause and returns false, so the caller refetches
// instead of serving a corrupt copy in silence. Both failures stay visible
// beside the fetch cause, which staleCatalog reports later. A stat failure also
// blocks the cache write, so it is disclosed once, with both effects, and the
// path is reported unusable to keep fetchAndCache silent.
func readFreshCache(path string, warn warnSink, now time.Time) (catalog, bool, bool) {
	fresh, err := cacheFresh(path, now)
	if err != nil {
		// A missing cache is the normal cold start, not corruption. Every other
		// stat failure (permissions, I/O, a link loop) means the path cannot be
		// read or written. Windows reports a file where a directory is needed as
		// ERROR_PATH_NOT_FOUND (ENOENT), so a missing path is a cold start only
		// when its parent really is a directory; otherwise the path is unusable.
		if errors.Is(err, os.ErrNotExist) {
			// A parent that exists but is not a directory blocks both read and
			// write, so the path is unusable. A missing parent is a normal cold
			// start: the cache dir is created on write.
			if d, derr := os.Stat(filepath.Dir(path)); derr == nil && !d.IsDir() {
				doWarnSink(warn, fmt.Errorf("%s: %w; refetching and skipping the rate cache write", path, err))
				return nil, false, true
			}
			return nil, false, false
		}
		doWarnSink(warn, fmt.Errorf("%s: %w; refetching and skipping the rate cache write", path, err))
		return nil, false, true
	}
	if !fresh {
		return nil, false, false
	}
	c, err := parseCatalogFile(path, warn)
	if err != nil {
		doWarnSink(warn, fmt.Errorf("%s: %w; refetching", path, err))
		return nil, false, false
	}
	return c, true, false
}

func fetchAndCache(path, url string, p *progress, cacheUnusable bool, warn warnSink) (catalog, error) {
	raw, err := fetchRates(p.context(), url)
	if err != nil {
		return nil, err
	}
	c, err := parseCatalog(bytes.NewReader(raw), warn)
	if err != nil {
		return nil, err
	}
	// A failed cache write must not hide prices: keep the fetched catalog for
	// this run and retry the fetch next run instead of serving unknown costs.
	// An unreadable mode is not a licence to widen it: skip the write. An
	// already-disclosed unusable path skips it without a second warning.
	if !cacheUnusable {
		mode, err := fileModeOrDefault(path)
		if err != nil {
			warnProgress(p, "rate cache mode %s: %s; skipping the cache write", path, err)
		} else if err := writeFileAtomic(path, raw, mode); err != nil {
			warnProgress(p, "rate cache write failed: %s; prices will be fetched again next run", err)
		}
	}
	// The fetch succeeded, so any prior failure marker is obsolete even when the
	// cache write failed.
	clearAttemptMarkers(path)
	return c, nil
}

func ratesCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wtm", "rates.json"), nil
}

// parseCatalogFile reads through readFileLimited: the cap stops a corrupt or
// hostile file from exhausting memory, and the guarded open refuses a FIFO,
// device or directory before reading, so a hostile cache path cannot hang
// status. openNoBlock still shares delete, so the Windows atomic replace
// (open_windows.go, rename_windows.go) keeps working.
func parseCatalogFile(path string, warn warnSink) (catalog, error) {
	data, err := readFileLimited(path, ratesMaxBytes)
	if errors.Is(err, errFileTooLarge) {
		// Keep a size failure distinct from a parse failure, so the caller and
		// the cooldown marker can name the real cause.
		return nil, fmt.Errorf("%w: %w", errRateCatalogTooLarge, err)
	}
	if err != nil {
		return nil, err
	}
	return parseCatalog(bytes.NewReader(data), warn)
}

// cacheFresh reports whether the cache is inside its TTL. A future mtime is
// invalid, not an exceptionally long-lived cache: report stale so clock skew or
// a bad copy cannot pin old prices forever. The file stays for the stale
// fallback.
func cacheFresh(path string, now time.Time) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	age := now.Sub(info.ModTime())
	if age < 0 {
		return false, nil
	}
	return age < ratesCacheTTL, nil
}

// cacheAgeLabel names how old the cached copy is. A failed stat or a future
// mtime yields a reason instead of a plausible duration, so the warning never
// states a false age.
func cacheAgeLabel(path string, now time.Time) string {
	info, err := os.Stat(path)
	if err != nil {
		return "age unknown"
	}
	age := now.Sub(info.ModTime())
	if age < 0 {
		return "age unknown (mtime in the future)"
	}
	return humanDuration(age)
}

// fetchRates retrieves the catalog under the run context, so an interrupted
// status does not wait out the HTTP timeout.
func fetchRates(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: ratesFetchTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rate catalog: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, ratesMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > ratesMaxBytes {
		return nil, fmt.Errorf("rate catalog: response exceeds %d bytes: %w", ratesMaxBytes, errRateCatalogTooLarge)
	}
	return data, nil
}
