package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
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
	// ratesRetryCooldown bounds how often a failed fetch is retried. Without
	// it, a broken network would delay every status run by ratesFetchTimeout.
	ratesRetryCooldown = 15 * time.Minute
	// longContextThreshold matches the catalog's *_above_200k_tokens fields: a
	// request whose input context exceeds the model's standard window is priced
	// at the above-200k rate when the catalog lists one.
	longContextThreshold = 200_000
)

// tokenRates is one price set. Missing fields are nil, not zero: a catalog that
// lists no rate must read as unknown, while a listed 0 is a real free class.
type tokenRates struct {
	input        *float64
	output       *float64
	cacheRead    *float64
	cacheWrite5m *float64
	cacheWrite1h *float64
}

// modelRate is one catalog entry: the two service tiers, the long-context set
// and the per-request web search price.
type modelRate struct {
	standard  tokenRates
	batch     tokenRates
	above     tokenRates
	hasAbove  bool
	webSearch *float64
}

// ratesFor picks the tier and context set. nil means the pair is unpriced, so
// the record reads unknown rather than silently mispriced.
func (r modelRate) ratesFor(tier string, context int64) *tokenRates {
	above := r.hasAbove && context > longContextThreshold
	switch tier {
	case "", "standard", "default":
		if above {
			return &r.above
		}
		return &r.standard
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

// catalog maps a model name to its rate.
type catalog map[string]modelRate

// estimateCacheWriteLifetime is the exact reason a cost reads estimated. wtm
// prints it verbatim in the status legend, and a test asserts it, so the stated
// reason can never drift from the estimate it explains.
const estimateCacheWriteLifetime = "a usage record omitted the cache-write lifetime (5m vs 1h), so wtm priced those tokens at the 5-minute rate; a 1-hour write costs more, so the value is a lower bound, not exact data"

// price turns one Claude event into money. A resolved model with zero tokens is
// a known $0.00; a missing model, tier, or needed class is unknown. A record
// that omits the cache-write lifetime is priced at the 5-minute rate and marked
// estimated, because the true 5m/1h split is unknowable.
func (c catalog) price(e usageEvent) money {
	r, ok := resolveModel(c, e.model)
	if !ok {
		return unknownMoney()
	}
	rates := r.ratesFor(e.tier, e.tokens.context())
	if rates == nil {
		return unknownMoney()
	}
	var m money
	m.addKnown(0) // resolved: a zero is a real zero
	priceClasses(&m, *rates, e.tokens)
	addEstimatedClass(&m, rates.cacheWrite5m, e.tokens.unclassifiedCacheWrite)
	priceWebSearch(&m, r, e.webSearch)
	return m
}

func unknownMoney() money {
	var m money
	m.addUnknown()
	return m
}

// priceClasses sums each priced token class in a fixed order so floating-point
// addition stays deterministic.
func priceClasses(m *money, r tokenRates, t claudeTokens) {
	addClass(m, r.input, t.input)
	addClass(m, r.output, t.output)
	addClass(m, r.cacheRead, t.cacheRead)
	addClass(m, r.cacheWrite5m, t.cacheWrite5m)
	addClass(m, r.cacheWrite1h, t.cacheWrite1h)
}

func priceWebSearch(m *money, r modelRate, requests int64) {
	if requests <= 0 {
		return
	}
	if r.webSearch == nil {
		m.addUnknown()
		return
	}
	m.addKnown(float64(requests) * *r.webSearch)
}

func addClass(m *money, rate *float64, n int64) {
	if n == 0 {
		return
	}
	if rate == nil {
		m.addUnknown()
		return
	}
	m.addKnown(float64(n) * *rate)
}

// addEstimatedClass prices a class whose split is unknown, at an assumed rate.
// A nil rate leaves the part unknown: without any basis, an estimate would
// invent a number, which is worse than an honest gap.
func addEstimatedClass(m *money, rate *float64, n int64) {
	if n == 0 {
		return
	}
	if rate == nil {
		m.addUnknown()
		return
	}
	m.addEstimated(float64(n) * *rate)
}

// resolveModel finds a rate for a recorded model id. The catalog uses many
// aliases (provider prefixes, version/date suffixes), so it tries the exact
// name first, then normalized candidates.
func resolveModel(c catalog, name string) (modelRate, bool) {
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
// absent field stays distinguishable from a listed zero.
type litellmEntry struct {
	Input        *float64           `json:"input_cost_per_token"`
	Output       *float64           `json:"output_cost_per_token"`
	CacheRead    *float64           `json:"cache_read_input_token_cost"`
	CacheWrite5m *float64           `json:"cache_creation_input_token_cost"`
	CacheWrite1h *float64           `json:"cache_creation_input_token_cost_above_1hr"`
	InAbove      *float64           `json:"input_cost_per_token_above_200k_tokens"`
	OutAbove     *float64           `json:"output_cost_per_token_above_200k_tokens"`
	CrAbove      *float64           `json:"cache_read_input_token_cost_above_200k_tokens"`
	Cw5Above     *float64           `json:"cache_creation_input_token_cost_above_200k_tokens"`
	InBatch      *float64           `json:"input_cost_per_token_batches"`
	OutBatch     *float64           `json:"output_cost_per_token_batches"`
	CrBatch      *float64           `json:"cache_read_input_token_cost_batches"`
	Cw5Batch     *float64           `json:"cache_creation_input_token_cost_batches"`
	Search       map[string]float64 `json:"search_context_cost_per_query"`
}

func (e litellmEntry) toRate() modelRate {
	return modelRate{
		standard: tokenRates{
			input: e.Input, output: e.Output, cacheRead: e.CacheRead,
			cacheWrite5m: e.CacheWrite5m, cacheWrite1h: e.CacheWrite1h,
		},
		batch: tokenRates{
			input: e.InBatch, output: e.OutBatch, cacheRead: e.CrBatch,
			cacheWrite5m: e.Cw5Batch,
		},
		above: tokenRates{
			input: e.InAbove, output: e.OutAbove, cacheRead: e.CrAbove,
			cacheWrite5m: e.Cw5Above,
		},
		hasAbove:  e.InAbove != nil || e.OutAbove != nil || e.CrAbove != nil || e.Cw5Above != nil,
		webSearch: searchRate(e.Search),
	}
}

// searchRate picks a per-query price. Anthropic lists one price for every
// context size; a split catalog falls back deterministically to medium.
func searchRate(m map[string]float64) *float64 {
	for _, key := range []string{"search_context_size_medium", "search_context_size_low", "search_context_size_high"} {
		if v, ok := m[key]; ok {
			return &v
		}
	}
	return nil
}

// parseCatalog reads the catalog object. Entries that are not priced models
// (the sample spec, comment rows) are skipped instead of failing the parse.
func parseCatalog(r io.Reader) (catalog, error) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r).Decode(&raw); err != nil {
		return nil, err
	}
	c := catalog{}
	for name, v := range raw {
		if rate, ok := pricedEntry(v); ok {
			c[name] = rate
		}
	}
	if len(c) == 0 {
		return nil, fmt.Errorf("rate catalog has no priced models")
	}
	return c, nil
}

// pricedEntry decodes one catalog value, reporting false for non-model rows.
func pricedEntry(raw json.RawMessage) (modelRate, bool) {
	var e litellmEntry
	if err := json.Unmarshal(raw, &e); err != nil {
		return modelRate{}, false
	}
	if e.Input == nil && e.Output == nil {
		return modelRate{}, false
	}
	return e.toRate(), true
}

// loadCatalog resolves the catalog: env override, fresh cache, network fetch,
// then a stale cache as the last resort. The returned error means no catalog is
// available at all, so callers report unknown costs.
func loadCatalog() (catalog, error) { return loadCatalogFrom(ratesURL) }

// loadCatalogFrom is loadCatalog with an injectable source URL, so tests drive
// the real fetch chain against a local server instead of the network.
func loadCatalogFrom(url string) (catalog, error) {
	if p := os.Getenv(ratesEnv); p != "" {
		return parseCatalogFile(p)
	}
	p, err := ratesCachePath()
	if err != nil {
		return nil, err
	}
	if c, ok := readFreshCache(p); ok {
		return c, nil
	}
	// A failed fetch must not stall every run: remember the attempt and retry
	// only after the cooldown. The stale cache still serves as the fallback.
	if !fetchCoolingDown(p) {
		if c, err := fetchAndCache(p, url); err == nil {
			return c, nil
		}
		_ = markFetchAttempt(p)
	}
	return parseCatalogFile(p)
}

// fetchCoolingDown reports a recent failed fetch, so a broken network cannot
// delay every status run.
func fetchCoolingDown(path string) bool {
	info, err := os.Stat(attemptPath(path))
	if err != nil {
		return false
	}
	return time.Since(info.ModTime()) < ratesRetryCooldown
}

// markFetchAttempt records a failed fetch. A write failure must not fail the
// command, so the caller ignores the error.
func markFetchAttempt(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(attemptPath(path), nil, 0o644)
}

func attemptPath(path string) string { return path + ".attempt" }

func readFreshCache(path string) (catalog, bool) {
	fresh, err := cacheFresh(path)
	if err != nil || !fresh {
		return nil, false
	}
	c, err := parseCatalogFile(path)
	return c, err == nil
}

func fetchAndCache(path, url string) (catalog, error) {
	raw, err := fetchRates(url)
	if err != nil {
		return nil, err
	}
	c, err := parseCatalog(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	_ = writeRatesCache(path, raw)
	return c, nil
}

func ratesCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".wtm", "rates.json"), nil
}

func parseCatalogFile(path string) (catalog, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return parseCatalog(f)
}

func cacheFresh(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) < ratesCacheTTL, nil
}

func fetchRates(url string) ([]byte, error) {
	client := &http.Client{Timeout: ratesFetchTimeout}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rate catalog: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}

// writeRatesCache persists the catalog atomically; a failed cache write must
// not fail the command, so the error is the caller's to ignore.
func writeRatesCache(path string, raw []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
