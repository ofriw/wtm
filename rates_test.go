package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// rates_test.go — the catalog and pricing contracts: missing rates are unknown,
// listed zeros are real, tiers and long context select the right rate.

const testCatalog = `{
  "m1": {
    "input_cost_per_token": 0.000001,
    "output_cost_per_token": 0.000002,
    "cache_read_input_token_cost": 0.0000001,
    "cache_creation_input_token_cost": 0.00000125,
    "cache_creation_input_token_cost_above_1hr": 0.000002,
    "input_cost_per_token_batches": 0.0000005,
    "output_cost_per_token_batches": 0.000001,
    "cache_read_input_token_cost_batches": 0.00000005,
    "cache_creation_input_token_cost_batches": 0.000000625,
    "input_cost_per_token_above_200k_tokens": 0.000002,
    "output_cost_per_token_above_200k_tokens": 0.000004,
    "cache_creation_input_token_cost_above_1hr_above_200k_tokens": 0.000003,
    "search_context_cost_per_query": {"search_context_size_medium": 0.01}
  },
  "claude-opus-4-6": {"input_cost_per_token": 0.000005, "output_cost_per_token": 0.000025},
  "sample_spec": {"input_cost_per_token": "float"}
}`

func testRates(t *testing.T) catalog {
	t.Helper()
	c, err := parseCatalog(strings.NewReader(testCatalog))
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}
	return c
}

func assertCost(t *testing.T, p pricedEvent, want float64, known bool) {
	t.Helper()
	m := p.total.list
	if m.known() != known {
		t.Fatalf("known = %v, want %v (money %+v)", m.known(), known, m)
	}
	if known && math.Abs(m.amount-want) > 1e-9 {
		t.Fatalf("amount = %v, want %v", m.amount, want)
	}
}

func TestParseCatalogSkipsUnpricedEntries(t *testing.T) {
	c := testRates(t)
	if _, ok := c["sample_spec"]; ok {
		t.Fatal("sample_spec must be skipped")
	}
	if _, ok := c["m1"]; !ok {
		t.Fatal("m1 missing")
	}
}

// TestParseCatalogNoPricedModels pins the empty-catalog sentinel: a document
// with only the sample spec and comment rows is a recognised failure, not a
// generic parse error.
func TestParseCatalogNoPricedModels(t *testing.T) {
	_, err := parseCatalog(strings.NewReader(`{"sample_spec":{"input_cost_per_token":"float"},"#comment":{"note":"ignore"}}`))
	if !errors.Is(err, errNoPricedModels) {
		t.Fatalf("err = %v, want errNoPricedModels", err)
	}
}

// TestPriceWebSearchWithoutSearchRateIsUnknown pins the missing-rate rule: a
// query the model entry cannot price taints the total instead of adding zero.
func TestPriceWebSearchWithoutSearchRateIsUnknown(t *testing.T) {
	c := catalog{"m": {standard: tokenRates{input: fptr(0.000001)}}}
	p := c.price(usageEvent{model: "m", tier: "standard", tokens: promptTokens{input: 1}, webSearch: 1})
	if !p.total.bothUnpriced() {
		t.Fatalf("price = %+v, want both sides unknown: the search rate is absent", p.total)
	}
	if got := p.total.cell(); got != "unknown" {
		t.Errorf("cell = %q, want unknown", got)
	}
	if got := p.total.list.jsonValue(); got != nil {
		t.Errorf("json = %v, want null", *got)
	}
}

// TestPriceTokenClasses pins each class at its own rate: a cache write must not
// use the cache read rate, and a 1-hour write must not use the 5-minute rate.
// The context stays below 200k so the long-context rate does not apply.
func TestPriceTokenClasses(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: promptTokens{
		input: 80_000, output: 100_000, cacheRead: 10_000, cacheWrite5m: 10_000, cacheWrite1h: 10_000,
	}}
	assertCost(t, c.price(e), 0.08+0.2+0.001+0.0125+0.02, true)
}

func TestPriceBatchTier(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "batch", tokens: promptTokens{
		input: 80_000, output: 100_000, cacheRead: 10_000, cacheWrite5m: 10_000,
	}}
	// The batch 1-hour class is absent in the fixture and unused here.
	assertCost(t, c.price(e), 0.04+0.1+0.0005+0.00625, true)
}

func TestPriceLongContext(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: promptTokens{input: 250_000, output: 1_000_000}}
	// Context exceeds 200k, so the above-200k rates apply.
	assertCost(t, c.price(e), 0.5+4, true)
}

// TestPriceLongContextBoundary pins the threshold: context at exactly 200k
// uses standard rates; above it uses long-context rates.
func TestPriceLongContextBoundary(t *testing.T) {
	c := testRates(t)
	atThreshold := usageEvent{model: "m1", tier: "standard", tokens: promptTokens{input: 200_000}}
	assertCost(t, c.price(atThreshold), 0.2, true)
	above := usageEvent{model: "m1", tier: "standard", tokens: promptTokens{input: 200_001}}
	assertCost(t, c.price(above), 200_001*0.000002, true)
}

// TestPriceLongContextOneHourCacheWrite pins C1: a 1-hour cache write above
// 200k context bills at the above-200k 1-hour rate, not unknown.
func TestPriceLongContextOneHourCacheWrite(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: promptTokens{input: 250_000, cacheWrite1h: 10_000}}
	assertCost(t, c.price(e), 250_000*0.000002+10_000*0.000003, true)
}

func TestPriceWebSearch(t *testing.T) {
	c := testRates(t)
	assertCost(t, c.price(usageEvent{model: "m1", tier: "standard", webSearch: 3}), 0.03, true)
}

// TestPriceUnknown pins every unpriced path: missing model, unknown tier, and a
// cache-write lifetime that has no 5-minute rate to estimate from.
func TestPriceUnknown(t *testing.T) {
	c := testRates(t)
	cases := map[string]usageEvent{
		"missing model":               {model: "nope", tier: "standard", tokens: promptTokens{input: 1}},
		"unknown tier":                {model: "m1", tier: "priority", tokens: promptTokens{input: 1}},
		"cache write with no 5m rate": {model: "claude-opus-4-6", tier: "standard", tokens: promptTokens{unclassifiedCacheWrite: 100}},
		"non-batch rate for batches":  {model: "m1", tier: "batch", tokens: promptTokens{cacheWrite1h: 1}},
		"batch with long context":     {model: "m1", tier: "batch", tokens: promptTokens{input: 250_000}},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if p := c.price(e); p.total.list.known() {
				t.Fatalf("price = %+v, want unknown", p)
			}
		})
	}
}

// TestPriceUnclassifiedCacheWriteIsEstimated pins the one assumption wtm makes:
// tokens with no 5m/1h lifetime are priced at the 5-minute rate and flagged
// estimated, because a 1-hour write would cost more.
func TestPriceUnclassifiedCacheWriteIsEstimated(t *testing.T) {
	c := testRates(t)
	m := c.price(usageEvent{model: "m1", tier: "standard", tokens: promptTokens{unclassifiedCacheWrite: 100}})
	assertCost(t, m, 100*0.00000125, true)
	if !m.total.list.isEstimated() {
		t.Fatalf("price = %+v, want estimated", m)
	}
}

func TestResolveModelCandidates(t *testing.T) {
	c := testRates(t)
	for _, name := range []string{"m1", "provider/m1", "anthropic.claude-opus-4-6-v1", "claude-opus-4-6", "m1-v2-20240101", "m1:qualifier-v3"} {
		if _, ok := resolveModel(c, name); !ok {
			t.Errorf("resolveModel(%q) failed", name)
		}
	}
	if _, ok := resolveModel(c, "not-a-model"); ok {
		t.Error("unknown model resolved")
	}
}

// TestParseCatalogMissingVsZero pins the pointer distinction: an absent rate is
// unknown, while a listed zero is priced at zero.
func TestParseCatalogMissingVsZero(t *testing.T) {
	c, err := parseCatalog(strings.NewReader(`{"z": {"input_cost_per_token": 0, "output_cost_per_token": 0}}`))
	if err != nil {
		t.Fatal(err)
	}
	assertCost(t, c.price(usageEvent{model: "z", tier: "standard", tokens: promptTokens{input: 500}}), 0, true)
	// An absent output rate with nonzero output tokens is unknown.
	c2, _ := parseCatalog(strings.NewReader(`{"z": {"input_cost_per_token": 0.001}}`))
	if p := c2.price(usageEvent{model: "z", tier: "standard", tokens: promptTokens{output: 5}}); p.total.list.known() {
		t.Fatalf("absent rate must be unknown, got %+v", p)
	}
}

func TestLoadCatalogEnvOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	writeFile(t, path, testCatalog, 0o644)
	t.Setenv(ratesEnv, path)
	c, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Error("m1 missing from the pinned catalog")
	}
}

func TestLoadCatalogUnreadableOverride(t *testing.T) {
	t.Setenv(ratesEnv, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := loadCatalog(); err == nil {
		t.Fatal("loadCatalog must fail when the pinned catalog is missing")
	}
}

func TestCacheFreshness(t *testing.T) {
	pinned := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Setenv("WTM_NOW", pinned.Format(time.RFC3339))
	path := filepath.Join(t.TempDir(), "rates.json")
	writeFile(t, path, testCatalog, 0o644)
	old := pinned.Add(-ratesCacheTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if fresh, err := cacheFresh(path); err != nil || fresh {
		t.Fatalf("stale cache fresh=%v err=%v, want false", fresh, err)
	}
	now := pinned.Add(-ratesCacheTTL + time.Hour)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	if fresh, err := cacheFresh(path); err != nil || !fresh {
		t.Fatalf("new cache fresh=%v err=%v, want true", fresh, err)
	}
}

// TestLoadCatalogStaleCacheFallback pins the cold-fetch guard: after a failed
// attempt, the next load skips the network and serves the stale cache instead
// of stalling. The cooldown is observed through the server, not the marker.
func TestLoadCatalogStaleCacheFallback(t *testing.T) {
	pinStaleCatalog(t, testCatalog)
	url, hits := serveRatesCounting(t, http.StatusInternalServerError, "boom")
	if _, err := loadCatalogFrom(url); err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	c, err := loadCatalogFrom(url)
	if err != nil {
		t.Fatalf("loadCatalogFrom: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Error("stale cache was not used")
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the retry must skip the network", hits.Load())
	}
}

// TestLoadCatalogFreshCacheSkipsNetwork pins the cache-first order: a fresh
// cache is served without touching the network.
func TestLoadCatalogFreshCacheSkipsNetwork(t *testing.T) {
	writeFile(t, isolatedRatesCache(t), testCatalog, 0o644)
	url, hits := serveRatesCounting(t, http.StatusOK, testCatalog)
	c, err := loadCatalogFrom(url)
	if err != nil {
		t.Fatalf("loadCatalogFrom: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Error("fresh cache was not served")
	}
	if hits.Load() != 0 {
		t.Errorf("fetches = %d, want 0: the network was touched", hits.Load())
	}
}

func TestFetchCoolingDown(t *testing.T) {
	pinned := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	t.Setenv("WTM_NOW", pinned.Format(time.RFC3339))
	path := filepath.Join(t.TempDir(), "rates.json")
	if cooling, _ := fetchCoolingDown(path); cooling {
		t.Fatal("no marker must not cool down")
	}
	cause := errors.New("rate catalog: HTTP 500")
	if err := markFetchAttempt(path, cause); err != nil {
		t.Fatal(err)
	}
	recent := pinned.Add(-ratesRetryCooldown / 2)
	if err := os.Chtimes(attemptPath(path), recent, recent); err != nil {
		t.Fatal(err)
	}
	cooling, got := fetchCoolingDown(path)
	if !cooling {
		t.Fatal("recent marker must cool down")
	}
	if got == nil || got.Error() != cause.Error() {
		t.Fatalf("cause = %v, want %v", got, cause)
	}
	// An empty marker from an older build still cools down, with a generic cause.
	writeFile(t, attemptPath(path), "", 0o644)
	if err := os.Chtimes(attemptPath(path), recent, recent); err != nil {
		t.Fatal(err)
	}
	cooling, got = fetchCoolingDown(path)
	if !cooling || !errors.Is(got, errRateCatalogCoolingDown) {
		t.Fatalf("empty marker cause = %v, want errRateCatalogCoolingDown", got)
	}
	old := pinned.Add(-ratesRetryCooldown - time.Minute)
	if err := os.Chtimes(attemptPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	if cooling, _ := fetchCoolingDown(path); cooling {
		t.Fatal("expired marker must not cool down")
	}
}

// serveRates runs a local catalog server: no external network is ever touched.
func serveRates(t *testing.T, status int, body string) string {
	url, _ := serveRatesCounting(t, status, body)
	return url
}

// serveRatesCounting is serveRates plus a request counter, so tests pin the
// fetch contract through the network boundary instead of internal markers.
func serveRatesCounting(t *testing.T, status int, body string) (string, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &hits
}

func isolatedRatesCache(t *testing.T) string {
	t.Helper()
	t.Setenv(ratesEnv, "") // A local override would bypass the fetch under test.
	// pinStaleCatalog stamps mtimes with the wall clock; a host-pinned WTM_NOW
	// would make a deliberately stale cache look fresh.
	t.Setenv("WTM_NOW", "")
	home := t.TempDir()
	setHomeEnv(t, home)
	return filepath.Join(home, ".wtm", "rates.json")
}

// writeFallbackMarker seeds a fallback cooldown marker and removes it after the
// test, so the private fallback directory never accumulates test state.
func writeFallbackMarker(t *testing.T, path string, rec rateAttemptRecord, mtime time.Time) string {
	t.Helper()
	dir, err := ensureFallbackDir()
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, fallbackAttemptName(path))
	writeFile(t, marker, string(data), 0o600)
	if err := os.Chtimes(marker, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })
	return marker
}

// pinStaleCatalog leaves a stale cache so the loader attempts a refresh.
func pinStaleCatalog(t *testing.T, body string) string {
	t.Helper()
	path := isolatedRatesCache(t)
	writeFile(t, path, body, 0o644)
	old := time.Now().Add(-ratesCacheTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRatesFetchSuccessCachesAndPrices pins the refresh path through the real
// loader: a 200 body is cached to disk and prices models, so a later offline
// run can serve it.
func TestRatesFetchSuccessCachesAndPrices(t *testing.T) {
	path := isolatedRatesCache(t)
	c, err := loadCatalogFrom(serveRates(t, http.StatusOK, testCatalog))
	assertFetchedCatalog(t, c, err)
	assertPersistedCatalog(t, path)
}

// TestRatesFetchErrorFallsBackToStale pins the outage path: an HTTP 500 with a
// stale cache on disk still serves old prices instead of unknown, and the
// failed attempt cools the retry down.
func TestRatesFetchErrorFallsBackToStale(t *testing.T) {
	pinStaleCatalog(t, testCatalog)
	url, hits := serveRatesCounting(t, http.StatusInternalServerError, "boom")
	c, err := loadCatalogFrom(url)
	if err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Fatal("stale cache was not used")
	}
	if _, err := loadCatalogFrom(url); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the retry must cool down", hits.Load())
	}
}

// TestRatesFetchErrorWithoutCache pins the cold-outage path: an HTTP 500 with
// no cache fails the load, so the caller reports unknown, never invented prices.
func TestRatesFetchErrorWithoutCache(t *testing.T) {
	isolatedRatesCache(t)
	url, hits := serveRatesCounting(t, http.StatusInternalServerError, "boom")
	if _, err := loadCatalogFrom(url); err == nil {
		t.Fatal("HTTP 500 with no cache must fail the load")
	}
	if _, err := loadCatalogFrom(url); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("second load = %v, want the recorded HTTP 500 cause", err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the retry must cool down", hits.Load())
	}
}

// TestModelCandidatesRejectsVersionFragments pins the dot-split guard: a
// version dot must not emit a fragment that could silently match a different
// catalog key, while a provider dot ("anthropic.claude-…") still splits.
func TestModelCandidatesRejectsVersionFragments(t *testing.T) {
	c := testRates(t)
	for _, name := range []string{"gpt-4.1", "gemini-2.5-flash", "m1.5"} {
		if _, ok := resolveModel(c, name); ok {
			t.Errorf("version-dotted %q resolved; a fragment must stay unknown", name)
		}
	}
	if _, ok := resolveModel(c, "anthropic.claude-opus-4-6-v1"); !ok {
		t.Error("provider-qualified dot must still split")
	}
}

// TestFixtureRatesParity pins the shipped catalog against the loader: the
// fixture parses, its model resolves, and a Claude token event plus a Pi
// self-priced event both price known, so catalog drift fails here instead of
// surfacing as unknown costs.
func TestFixtureRatesParity(t *testing.T) {
	f, err := os.Open("test/fixtures/rates.json")
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer func() { _ = f.Close() }()
	c, err := parseCatalog(f)
	if err != nil {
		t.Fatalf("parseCatalog fixture: %v", err)
	}
	if _, ok := resolveModel(c, "claude-sonnet-4-6"); !ok {
		t.Fatal("claude-sonnet-4-6 must resolve from the fixture")
	}
	// 100k input at 3e-6 plus 10k output at 1.5e-5.
	claude := usageEvent{model: "claude-sonnet-4-6", tier: "standard",
		tokens: promptTokens{input: 100_000, output: 10_000}}
	assertCost(t, c.price(claude), 0.45, true)
	pi := usageEvent{at: at(0), model: "m", cost: fptr(1.5)}
	p := usagePricer(agent{name: "pi"}, c, subscriptions{})(pi)
	if !p.total.list.known() || !p.total.billed.known() {
		t.Fatalf("pi self-priced event must stay known: %+v", p.total)
	}
}

// TestPriceLongContextPartialAboveFallsBack pins the per-class merge: a catalog
// entry with only the 1h-write above rate still prices other classes above
// 200k context at their standard rates, not unknown.
func TestPriceLongContextPartialAboveFallsBack(t *testing.T) {
	c := catalog{"mx": {standard: tokenRates{input: fptr(0.000001)}, above: tokenRates{cacheWrite1h: fptr(0.000003)}, hasAbove: true}}
	e := usageEvent{model: "mx", tier: "standard", tokens: promptTokens{input: 250_000, cacheWrite1h: 10_000}}
	assertCost(t, c.price(e), 250_000*0.000001+10_000*0.000003, true)
}

func assertFetchedCatalog(t *testing.T, c catalog, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("loadCatalogFrom: %v", err)
	}
	assertCost(t, c.price(usageEvent{model: "m1", tier: "standard", tokens: promptTokens{input: 1000}}), 0.001, true)
}

func assertCatalogBytes(t *testing.T, r io.Reader, want string) {
	t.Helper()
	got, err := io.ReadAll(r)
	if err != nil || string(got) != want {
		t.Fatalf("catalog = %q, err = %v, want %q", got, err, want)
	}
}

func assertPersistedCatalog(t *testing.T, path string) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	assertCatalogBytes(t, f, testCatalog)
	assertNoRatesTempLeak(t, path)
}

// assertNoRatesTempLeak pins that writeFileAtomic left no sibling temp file.
// Other entries (e.g. the .attempt marker) are legitimate, so this must not
// require an otherwise empty directory.
func assertNoRatesTempLeak(t *testing.T, path string) {
	t.Helper()
	leftover, err := filepath.Glob(path + ".tmp-*")
	if err != nil {
		t.Fatal(err)
	}
	if len(leftover) != 0 {
		t.Fatalf("temp files left after cache write: %v", leftover)
	}
}

// Delete-sharing readers retain old bytes while new openers see fetched bytes.
// This rejects in-place writes and Windows rename without POSIX semantics.
func TestRatesRefreshReplacesHeldSnapshot(t *testing.T) {
	const stale = `{"m1":{"input_cost_per_token":0.000009}}`
	path := pinStaleCatalog(t, stale)
	held, err := openShared(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	c, err := loadCatalogFrom(serveRates(t, http.StatusOK, testCatalog))
	assertFetchedCatalog(t, c, err)
	assertPersistedCatalog(t, path)
	assertCatalogBytes(t, held, stale)
}

// TestRatesCacheWriteFailureStillPrices pins that a failed cache write is
// non-fatal: the loader prices the fetched catalog, warns, and leaves no
// cooldown marker, so the next run can fetch and price again.
func TestRatesCacheWriteFailureStillPrices(t *testing.T) {
	path := isolatedRatesCache(t)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	url, hits := serveRatesCounting(t, http.StatusOK, testCatalog)
	errOut := captureStderr(t, func() {
		c, err := loadCatalogFrom(url)
		assertFetchedCatalog(t, c, err)
	})
	if !strings.Contains(errOut, "rate cache write failed") {
		t.Errorf("stderr must disclose the cache-write failure:\n%s", errOut)
	}
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		t.Fatalf("cache directory replaced or missing: info=%v err=%v", info, err)
	}
	assertNoRatesTempLeak(t, path)
	if _, err := os.Stat(attemptPath(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cache-write failure must not arm a cooldown: %v", err)
	}
	// The next run refetches and prices; storage failure must not hide costs.
	captureStderr(t, func() {
		c, err := loadCatalogFrom(url)
		assertFetchedCatalog(t, c, err)
	})
	assertRatesFetchCount(t, hits, 2)
}

// TestRatesCacheWriteFailureRidesProgressHandle pins that a renderer window
// receives the disclosure as a notice: a raw stderr write would be erased by
// the next frame.
func TestRatesCacheWriteFailureRidesProgressHandle(t *testing.T) {
	path, _ := blockRatesCacheWrite(t, false)
	url := serveRates(t, http.StatusOK, testCatalog)
	events := make(chan progressEvent, 16)
	p := &progress{ctx: context.Background(), events: events}
	errOut := captureStderr(t, func() {
		c, err := fetchAndCache(path, url, p)
		assertFetchedCatalog(t, c, err)
	})
	if errOut != "" {
		t.Errorf("renderer window leaked raw stderr: %q", errOut)
	}
	close(events)
	var notices []string
	for ev := range events {
		if ev.kind == evNotice {
			notices = append(notices, ev.text)
		}
	}
	if len(notices) != 1 || !strings.Contains(notices[0], "rate cache write failed") {
		t.Fatalf("notices = %q, want one cache-write warning", notices)
	}
}

// TestRatesCacheWriteFailureRefetchesEveryRun pins both blocked-cache shapes:
// a working network keeps pricing, and no cooldown marker suppresses the next
// fetch.
func TestRatesCacheWriteFailureRefetchesEveryRun(t *testing.T) {
	for _, blockedParent := range []bool{false, true} {
		name := "cache path is directory"
		if blockedParent {
			name = "cache parent is file"
		}
		t.Run(name, func(t *testing.T) {
			path, marker := blockRatesCacheWrite(t, blockedParent)
			url, hits := serveRatesCounting(t, http.StatusOK, testCatalog)
			for i := int32(1); i <= 2; i++ {
				errOut := captureStderr(t, func() {
					c, err := loadCatalogFrom(url)
					assertFetchedCatalog(t, c, err)
				})
				if !strings.Contains(errOut, "rate cache write failed") {
					t.Errorf("run %d stderr must disclose the cache-write failure:\n%s", i, errOut)
				}
				assertRatesFetchCount(t, hits, i)
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Errorf("run %d must not leave a cooldown marker: %v", i, err)
				}
			}
			assertNoRatesTempLeak(t, path)
		})
	}
}

func blockRatesCacheWrite(t *testing.T, blockedParent bool) (string, string) {
	t.Helper()
	path := isolatedRatesCache(t)
	if !blockedParent {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		return path, attemptPath(path)
	}
	writeFile(t, filepath.Dir(path), "block", 0o644)
	marker := fallbackAttemptPath(path)
	t.Cleanup(func() {
		if err := os.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove fallback marker: %v", err)
		}
	})
	return path, marker
}

func assertRatesFetchCount(t *testing.T, hits *atomic.Int32, want int32) {
	t.Helper()
	if got := hits.Load(); got != want {
		t.Fatalf("fetches = %d, want %d", got, want)
	}
}

// TestMarkFetchAttemptFallsBackWhenCacheDirUnwritable pins the marker fallback:
// when HOME/.wtm cannot be created (it is a file), a per-cache marker under the
// OS temp directory still arms the cooldown.
func TestMarkFetchAttemptFallsBackWhenCacheDirUnwritable(t *testing.T) {
	path := isolatedRatesCache(t)
	t.Cleanup(func() { _ = os.Remove(fallbackAttemptPath(path)) })
	if err := os.WriteFile(filepath.Dir(path), []byte("block"), 0o644); err != nil {
		t.Fatal(err)
	}
	url, hits := serveRatesCounting(t, http.StatusInternalServerError, "boom")
	if _, err := loadCatalogFrom(url); err == nil {
		t.Fatal("a cold outage with an unwritable cache directory must fail")
	}
	if _, err := loadCatalogFrom(url); err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("second load = %v, want the recorded HTTP 500 cause", err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the fallback marker must arm the cooldown", hits.Load())
	}
}

// TestFetchCoolingDownIgnoresFutureMarker pins the clock guard: a marker with a
// future mtime is invalid, not an arbitrarily long cooldown.
func TestFetchCoolingDownIgnoresFutureMarker(t *testing.T) {
	path := isolatedRatesCache(t)
	marker := writeFallbackMarker(t, path, rateAttemptRecord{Message: "future"}, time.Now().Add(time.Hour))
	if cooling, _ := fetchCoolingDown(path); cooling {
		t.Fatal("a future marker must not cool down")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("future marker must be removed: %v", err)
	}
}

// TestFetchCoolingDownRejectsNonRegularMarker pins the file-type guard: a
// directory at the marker path is never opened as a marker.
func TestFetchCoolingDownRejectsNonRegularMarker(t *testing.T) {
	path := isolatedRatesCache(t)
	marker := fallbackAttemptPath(path)
	if err := os.MkdirAll(marker, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(marker) })
	if cooling, _ := fetchCoolingDown(path); cooling {
		t.Fatal("a non-regular marker must not cool down")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid marker must be removed: %v", err)
	}
}

// TestFetchCoolingDownRemovesExpiredMarker pins cleanup: an expired marker
// cannot leak in the fallback directory forever.
func TestFetchCoolingDownRemovesExpiredMarker(t *testing.T) {
	path := isolatedRatesCache(t)
	marker := writeFallbackMarker(t, path, rateAttemptRecord{Message: "old"}, time.Now().Add(-ratesRetryCooldown-time.Minute))
	if cooling, _ := fetchCoolingDown(path); cooling {
		t.Fatal("an expired marker must not cool down")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired marker must be removed: %v", err)
	}
}

// TestReadAttemptCauseSanitizesControlCharacters pins the terminal-injection
// guard: marker text is stripped of control bytes before it reaches stderr.
func TestReadAttemptCauseSanitizesControlCharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "marker")
	writeFile(t, path, `{"message":"a\u001b[31mb\nc"}`, 0o600)
	err := readAttemptCause(path)
	if err == nil {
		t.Fatal("marker must produce a cause")
	}
	if got := err.Error(); got != "a[31mbc" {
		t.Fatalf("cause = %q, want control bytes stripped", got)
	}
}

// TestSanitizeAttemptMessageExactCap pins the length cap: output never exceeds
// maxAttemptMessageBytes and never splits a multi-byte rune to fit.
func TestSanitizeAttemptMessageExactCap(t *testing.T) {
	msg := strings.Repeat("x", maxAttemptMessageBytes-1) + "\U0001F600" + strings.Repeat("y", 16)
	got := sanitizeAttemptMessage(msg)
	if len(got) > maxAttemptMessageBytes {
		t.Fatalf("len = %d, want <= %d", len(got), maxAttemptMessageBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatalf("cause %q is not valid UTF-8: a rune was split", got)
	}
}

// TestClearAttemptMarkersRemovesBoth pins cleanup after a successful fetch.
func TestClearAttemptMarkersRemovesBoth(t *testing.T) {
	path := isolatedRatesCache(t)
	writeFile(t, attemptPath(path), `{"message":"old"}`, 0o600)
	fallback := writeFallbackMarker(t, path, rateAttemptRecord{Message: "old"}, time.Now())
	clearAttemptMarkers(path)
	for _, marker := range []string{attemptPath(path), fallback} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("marker %s survived cleanup: %v", marker, err)
		}
	}
}

// TestFetchRatesRejectsOversizedBody pins the read cap: a body over the limit
// is refused with the size sentinel and never returned for parsing.
func TestFetchRatesRejectsOversizedBody(t *testing.T) {
	url, _ := serveRatesCounting(t, http.StatusOK, strings.Repeat("x", ratesMaxBytes+1))
	data, err := fetchRates(url)
	if data != nil || !errors.Is(err, errRateCatalogTooLarge) {
		t.Fatalf("fetchRates = %d bytes, %v; want nil, errRateCatalogTooLarge", len(data), err)
	}
}

// TestFetchRatesAcceptsExactSizeLimit pins the boundary: the cap rejects only
// a body larger than the limit, never exactly the limit.
func TestFetchRatesAcceptsExactSizeLimit(t *testing.T) {
	url, _ := serveRatesCounting(t, http.StatusOK, strings.Repeat("x", ratesMaxBytes))
	data, err := fetchRates(url)
	if err != nil || len(data) != ratesMaxBytes {
		t.Fatalf("fetchRates = %d bytes, %v; want the exact cap accepted", len(data), err)
	}
}

// TestRatesOversizedWithoutCacheFails pins the cold path: an oversized body
// with no fallback catalog reports the size error, not a missing-file error.
func TestRatesOversizedWithoutCacheFails(t *testing.T) {
	isolatedRatesCache(t)
	url, hits := serveRatesCounting(t, http.StatusOK, strings.Repeat("x", ratesMaxBytes+1))
	if _, err := loadCatalogFrom(url); !errors.Is(err, errRateCatalogTooLarge) {
		t.Fatalf("err = %v, want errRateCatalogTooLarge", err)
	}
	if _, err := loadCatalogFrom(url); !errors.Is(err, errRateCatalogTooLarge) {
		t.Fatalf("cooled-down err = %v, want the recorded size cause", err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the retry must cool down", hits.Load())
	}
}

// TestRatesOversizedFallsBackToStale pins the fallback order: a stale cache
// still prices when the fetched body breaks the size cap.
func TestRatesOversizedFallsBackToStale(t *testing.T) {
	pinStaleCatalog(t, testCatalog)
	url, hits := serveRatesCounting(t, http.StatusOK, strings.Repeat("x", ratesMaxBytes+1))
	c, err := loadCatalogFrom(url)
	if err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Fatal("stale cache was not used")
	}
	if _, err := loadCatalogFrom(url); err != nil {
		t.Fatalf("second load: %v", err)
	}
	if hits.Load() != 1 {
		t.Errorf("fetches = %d, want 1: the retry must cool down", hits.Load())
	}
}

// releaseBarrier bounds how long the server holds a response, so a change in
// the fetch count cannot stall the test. It must stay below the client timeout
// (ratesFetchTimeout) so the client never wins the race.
const releaseBarrier = 2 * time.Second

// serveRatesBarrier holds every response until all expected loaders arrive, or
// the bound expires, so the loaders overlap at the network.
func serveRatesBarrier(t *testing.T, payloads []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), releaseBarrier)
	srv := httptest.NewServer(ratesBarrierHandler(ctx, payloads, new(atomic.Int32)))
	t.Cleanup(func() { cancel(); srv.Close() })
	return srv.URL
}

func ratesBarrierHandler(ctx context.Context, payloads []string, arrived *atomic.Int32) http.Handler {
	release := make(chan struct{})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := int(arrived.Add(1) - 1)
		if i >= len(payloads) {
			http.Error(w, "unexpected catalog request", http.StatusInternalServerError)
			return
		}
		if i == len(payloads)-1 {
			close(release)
		}
		select {
		case <-release:
		case <-ctx.Done():
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, payloads[i])
	})
}

type catalogLoadResult struct {
	catalog catalog
	err     error
}

func concurrentCatalogLoads(url string, loaders int) []catalogLoadResult {
	results := make(chan catalogLoadResult, loaders)
	for i := 0; i < loaders; i++ {
		go func() {
			c, err := loadCatalogFrom(url)
			results <- catalogLoadResult{c, err}
		}()
	}
	all := make([]catalogLoadResult, loaders)
	for i := range all {
		all[i] = <-results
	}
	return all
}

// TestRatesParallelLoadsKeepValidCatalog pins the loader contract under
// concurrency: every loader returns one complete catalog, and the cache holds
// exactly one complete payload. Fewer fetches (cache sharing) pass. An extra
// fetch is rejected by the barrier only while no cache exists; once a peer has
// cached, a rejected loader still passes by serving that cache.
func TestRatesParallelLoadsKeepValidCatalog(t *testing.T) {
	path := isolatedRatesCache(t)
	const loaders = 16
	payloads := parallelRatePayloads(loaders)
	for _, result := range concurrentCatalogLoads(serveRatesBarrier(t, payloads), loaders) {
		assertOneServedCatalog(t, result, payloads)
	}
	assertOneServedCache(t, path, payloads)
}

func parallelRatePayloads(n int) []string {
	payloads := make([]string, n)
	for i := range payloads {
		input := float64(i+1) / 1_000_000
		output := float64(i+1) / 100_000
		payloads[i] = fmt.Sprintf(`{"m%d":{"input_cost_per_token":%g,"output_cost_per_token":%g}}`, i, input, output)
	}
	return payloads
}

// assertOneServedCatalog requires the loader's catalog to price exactly one of
// the served payloads at that payload's rate, proving it got one whole catalog.
func assertOneServedCatalog(t *testing.T, result catalogLoadResult, payloads []string) {
	t.Helper()
	if result.err != nil {
		t.Fatalf("loadCatalogFrom: %v", result.err)
	}
	for i := range payloads {
		model := fmt.Sprintf("m%d", i)
		priced := result.catalog.price(usageEvent{model: model, tier: "standard", tokens: promptTokens{input: 1000, output: 1000}})
		if priced.total.list.known() {
			assertCost(t, priced, float64(i+1)*0.011, true)
			return
		}
	}
	t.Fatal("loader returned no complete served catalog")
}

// assertOneServedCache requires the on-disk cache to equal exactly one served
// payload, byte for byte, and to leave no temp file behind.
func assertOneServedCache(t *testing.T, path string, payloads []string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range payloads {
		if string(got) == payload {
			assertNoRatesTempLeak(t, path)
			return
		}
	}
	t.Fatalf("cache is not one complete served payload: %q", got)
}
