package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	// ratesMaxBytes bounds the network catalog read. The real catalog is a few
	// MB; a larger body is a wrong endpoint or a hostile response, and parsing a
	// truncated catalog would silently drop models. The cap+1 read lets the
	// loader detect the overflow without buffering an unbounded body.
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
)

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

// standardRates selects the standard set, or the long-context set merged with
// it when the prompt exceeds the threshold.
func (r modelRate) standardRates(above bool) *tokenRates {
	if !above {
		return &r.standard
	}
	// Merge per class: an above-200k field wins when listed, else the standard
	// rate prices that class. A whole-set switch would turn a partially-listed
	// catalog (e.g. only the 1h-write above rate) into unknown for every other
	// class.
	merged := r.above
	mergeRateSet(&merged, r.standard)
	return &merged
}

// mergeRateSet fills each unlisted override class from base.
func mergeRateSet(dst *tokenRates, base tokenRates) {
	mergeRate(&dst.input, base.input)
	mergeRate(&dst.output, base.output)
	mergeRate(&dst.cacheRead, base.cacheRead)
	mergeRate(&dst.cacheWrite5m, base.cacheWrite5m)
	mergeRate(&dst.cacheWrite1h, base.cacheWrite1h)
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

// price turns one Claude event into its price pair and per-tier breakdown. A
// resolved model with zero tokens is a known $0.00; a missing model, tier, or
// needed class is unknown. A record that omits the cache-write lifetime is
// priced at the 5-minute rate and marked estimated, because the true 5m/1h
// split is unknowable. The billed side mirrors the list here; the subscription
// substitution (usagePricer) replaces billed per tier.
func (c catalog) price(e usageEvent) pricedEvent {
	r, ok := resolveModel(c, e.model)
	if !ok {
		return unpriced()
	}
	rates := r.ratesFor(e.tier, e.tokens.context())
	if rates == nil {
		return unpriced()
	}
	p := pricedEvent{tiers: priceTiers(*rates, e.tokens)}
	p.total.list.addKnown(0) // resolved: a zero is a real zero
	p.total.list.add(p.tiers.totals())
	priceWebSearch(&p.total.list, r, e.webSearch)
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
func priceTiers(r tokenRates, t promptTokens) tokenTiers {
	var tiers tokenTiers
	addClass(&tiers[tierInput].list, r.input, t.input)
	addClass(&tiers[tierOutput].list, r.output, t.output)
	addClass(&tiers[tierCacheRead].list, r.cacheRead, t.cacheRead)
	addClass(&tiers[tierCacheWrite].list, r.cacheWrite5m, t.cacheWrite5m)
	addClass(&tiers[tierCacheWrite].list, r.cacheWrite1h, t.cacheWrite1h)
	addEstimatedClass(&tiers[tierCacheWrite].list, r.cacheWrite5m, t.unclassifiedCacheWrite)
	for i := range tiers {
		tiers[i].billed = tiers[i].list
	}
	return tiers
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
	m.addEstimated(float64(n)**rate, causeCacheWrite)
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
	Cw1Above     *float64           `json:"cache_creation_input_token_cost_above_1hr_above_200k_tokens"`
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
			cacheWrite5m: e.Cw5Above, cacheWrite1h: e.Cw1Above,
		},
		hasAbove:  e.InAbove != nil || e.OutAbove != nil || e.CrAbove != nil || e.Cw5Above != nil || e.Cw1Above != nil,
		webSearch: searchRate(e.Search),
	}
}

// searchRate picks a per-query price. Anthropic lists one price for every
// context size, so a split catalog resolves in a fixed order (medium, low,
// high) and returns nil when none is listed, which reads as unknown.
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
		return nil, errNoPricedModels
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
func loadCatalogReporting(p *progress) (catalog, error) {
	return loadCatalogFromNotify(ratesURL, p)
}

// loadCatalogFrom is loadCatalog with an injectable source URL, so tests drive
// the real fetch chain against a local server instead of the network.
func loadCatalogFrom(url string) (catalog, error) { return loadCatalogFromNotify(url, nil) }

func loadCatalogFromNotify(url string, p *progress) (catalog, error) {
	if path := os.Getenv(ratesEnv); path != "" {
		p.detail(string(catalogEnv))
		return parseCatalogFile(path)
	}
	path, err := ratesCachePath()
	if err != nil {
		return nil, err
	}
	return loadFromCachePath(path, url, p)
}

// loadFromCachePath resolves a fresh cache, then a fetch, then the stale cache
// as the last resort. When the refresh fails, now or inside a cooldown, and no
// fallback catalog exists, the recorded cause is returned: it names the real
// failure (status, size, network), where the missing cache would only report
// ENOENT.
func loadFromCachePath(path, url string, p *progress) (catalog, error) {
	if c, ok := readFreshCache(path); ok {
		p.detail(string(catalogCache))
		return c, nil
	}
	// A failed fetch starts a cooldown to avoid repeated stalls. The stale cache
	// still serves as the fallback.
	var fetchErr error
	if cooling, cause := fetchCoolingDown(path); cooling {
		fetchErr = cause
		if !errors.Is(cause, errRateCatalogCoolingDown) {
			fetchErr = fmt.Errorf("%w: %w", errRateCatalogCoolingDown, cause)
		}
	} else {
		p.detail(string(catalogFetching))
		if c, err := fetchAndCache(path, url, p); err == nil {
			p.detail(string(catalogFetched))
			return c, nil
		} else {
			fetchErr = err
		}
		_ = markFetchAttempt(path, fetchErr)
	}
	c, err := parseCatalogFile(path)
	if err == nil {
		p.detail(string(catalogStale))
		return c, nil
	}
	if fetchErr != nil {
		return nil, fetchErr
	}
	return nil, err
}

// fetchCoolingDown reports a recent failed fetch and its recorded cause. Both
// marker locations count: the primary beside the cache and the fallback for a
// run whose cache directory is unwritable. ensureFallbackDir validates the
// fallback directory before its marker is read.
func fetchCoolingDown(path string) (bool, error) {
	if cooling, cause := recentAttemptCause(attemptPath(path)); cooling {
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
	return recentAttemptCause(filepath.Join(dir, fallbackAttemptName(path)))
}

// recentAttemptCause reports a fresh marker and its cause. A future mtime is
// invalid, not a long cooldown; a non-regular file is never opened. Expired or
// invalid markers are removed so they cannot leak forever.
func recentAttemptCause(path string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return false, nil
	}
	if !info.Mode().IsRegular() {
		_ = os.Remove(path)
		return false, nil
	}
	age := nowFunc().Sub(info.ModTime())
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
// failure must not prevent pricing, so callers ignore the error. When the cache
// directory is unwritable (a file named .wtm, a read-only home), a marker in the
// fallback directory still arms the cooldown after ensureFallbackDir validates
// the directory.
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

func readFreshCache(path string) (catalog, bool) {
	fresh, err := cacheFresh(path)
	if err != nil || !fresh {
		return nil, false
	}
	c, err := parseCatalogFile(path)
	return c, err == nil
}

func fetchAndCache(path, url string, p *progress) (catalog, error) {
	raw, err := fetchRates(url)
	if err != nil {
		return nil, err
	}
	c, err := parseCatalog(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	// A failed cache write must not hide prices: keep the fetched catalog for
	// this run and retry the fetch next run instead of serving unknown costs.
	if err := writeFileAtomic(path, raw, 0o644); err != nil {
		warnProgress(p, "rate cache write failed: %s; prices will be fetched again next run", err)
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

// parseCatalogFile reads through readFileShared: on Windows a plain os.Open
// handle blocks a concurrent writeFileAtomic replace, so the refresh would be
// silently dropped (open_windows.go, rename_windows.go).
func parseCatalogFile(path string) (catalog, error) {
	data, err := readFileShared(path)
	if err != nil {
		return nil, err
	}
	return parseCatalog(bytes.NewReader(data))
}

func cacheFresh(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return nowFunc().Sub(info.ModTime()) < ratesCacheTTL, nil
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
	data, err := io.ReadAll(io.LimitReader(resp.Body, ratesMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > ratesMaxBytes {
		return nil, fmt.Errorf("rate catalog: response exceeds %d bytes: %w", ratesMaxBytes, errRateCatalogTooLarge)
	}
	return data, nil
}
