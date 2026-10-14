package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

func assertCost(t *testing.T, m money, want float64, known bool) {
	t.Helper()
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

// TestPriceTokenClasses pins each class at its own rate: a cache write must not
// use the cache read rate, and a 1-hour write must not use the 5-minute rate.
// The context stays below 200k so the long-context rate does not apply.
func TestPriceTokenClasses(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{
		input: 80_000, output: 100_000, cacheRead: 10_000, cacheWrite5m: 10_000, cacheWrite1h: 10_000,
	}}
	assertCost(t, c.price(e), 0.08+0.2+0.001+0.0125+0.02, true)
}

func TestPriceBatchTier(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "batch", tokens: claudeTokens{
		input: 80_000, output: 100_000, cacheRead: 10_000, cacheWrite5m: 10_000,
	}}
	// The batch 1-hour class is absent in the fixture and unused here.
	assertCost(t, c.price(e), 0.04+0.1+0.0005+0.00625, true)
}

func TestPriceLongContext(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{input: 250_000, output: 1_000_000}}
	// Context exceeds 200k, so the above-200k rates apply.
	assertCost(t, c.price(e), 0.5+4, true)
}

// TestPriceLongContextBoundary pins the threshold: context at exactly 200k
// uses standard rates; above it uses long-context rates.
func TestPriceLongContextBoundary(t *testing.T) {
	c := testRates(t)
	atThreshold := usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{input: 200_000}}
	assertCost(t, c.price(atThreshold), 0.2, true)
	above := usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{input: 200_001}}
	assertCost(t, c.price(above), 200_001*0.000002, true)
}

// TestPriceLongContextOneHourCacheWrite pins C1: a 1-hour cache write above
// 200k context bills at the above-200k 1-hour rate, not unknown.
func TestPriceLongContextOneHourCacheWrite(t *testing.T) {
	c := testRates(t)
	e := usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{input: 250_000, cacheWrite1h: 10_000}}
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
		"missing model":               {model: "nope", tier: "standard", tokens: claudeTokens{input: 1}},
		"unknown tier":                {model: "m1", tier: "priority", tokens: claudeTokens{input: 1}},
		"cache write with no 5m rate": {model: "claude-opus-4-6", tier: "standard", tokens: claudeTokens{unclassifiedCacheWrite: 100}},
		"non-batch rate for batches":  {model: "m1", tier: "batch", tokens: claudeTokens{cacheWrite1h: 1}},
		"batch with long context":     {model: "m1", tier: "batch", tokens: claudeTokens{input: 250_000}},
	}
	for name, e := range cases {
		t.Run(name, func(t *testing.T) {
			if m := c.price(e); m.known() {
				t.Fatalf("price = %+v, want unknown", m)
			}
		})
	}
}

// TestPriceUnclassifiedCacheWriteIsEstimated pins the one assumption wtm makes:
// tokens with no 5m/1h lifetime are priced at the 5-minute rate and flagged
// estimated, because a 1-hour write would cost more.
func TestPriceUnclassifiedCacheWriteIsEstimated(t *testing.T) {
	c := testRates(t)
	m := c.price(usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{unclassifiedCacheWrite: 100}})
	assertCost(t, m, 100*0.00000125, true)
	if !m.isEstimated() {
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
	assertCost(t, c.price(usageEvent{model: "z", tier: "standard", tokens: claudeTokens{input: 500}}), 0, true)
	// An absent output rate with nonzero output tokens is unknown.
	c2, _ := parseCatalog(strings.NewReader(`{"z": {"input_cost_per_token": 0.001}}`))
	if m := c2.price(usageEvent{model: "z", tier: "standard", tokens: claudeTokens{output: 5}}); m.known() {
		t.Fatalf("absent rate must be unknown, got %+v", m)
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
	path := filepath.Join(t.TempDir(), "rates.json")
	writeFile(t, path, testCatalog, 0o644)
	old := time.Now().Add(-ratesCacheTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if fresh, err := cacheFresh(path); err != nil || fresh {
		t.Fatalf("stale cache fresh=%v err=%v, want false", fresh, err)
	}
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	if fresh, err := cacheFresh(path); err != nil || !fresh {
		t.Fatalf("new cache fresh=%v err=%v, want true", fresh, err)
	}
}

// TestLoadCatalogStaleCacheFallback pins the cold-fetch guard: a recent failed
// attempt skips the network and serves the stale cache instead of stalling.
func TestLoadCatalogStaleCacheFallback(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	t.Setenv(ratesEnv, "")
	path := filepath.Join(home, ".wtm", "rates.json")
	writeFile(t, path, testCatalog, 0o644)
	old := time.Now().Add(-ratesCacheTTL - time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	writeFile(t, attemptPath(path), "", 0o644)
	c, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Error("stale cache was not used")
	}
}

// TestLoadCatalogFreshCacheSkipsNetwork pins the cache-first order: a fresh
// cache is served without a fetch, so no attempt marker may appear (a regressed
// fetch writes one when it fails).
func TestLoadCatalogFreshCacheSkipsNetwork(t *testing.T) {
	home := t.TempDir()
	setHomeEnv(t, home)
	t.Setenv(ratesEnv, "")
	path := filepath.Join(home, ".wtm", "rates.json")
	writeFile(t, path, testCatalog, 0o644)
	c, err := loadCatalog()
	if err != nil {
		t.Fatalf("loadCatalog: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Error("fresh cache was not served")
	}
	if _, err := os.Stat(attemptPath(path)); err == nil {
		t.Error("attempt marker written: the network was touched")
	}
}

func TestFetchCoolingDown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rates.json")
	if fetchCoolingDown(path) {
		t.Fatal("no marker must not cool down")
	}
	writeFile(t, attemptPath(path), "", 0o644)
	if !fetchCoolingDown(path) {
		t.Fatal("recent marker must cool down")
	}
	old := time.Now().Add(-ratesRetryCooldown - time.Minute)
	if err := os.Chtimes(attemptPath(path), old, old); err != nil {
		t.Fatal(err)
	}
	if fetchCoolingDown(path) {
		t.Fatal("expired marker must not cool down")
	}
}

// serveRates runs a local catalog server: no external network is ever touched.
func serveRates(t *testing.T, status int, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// pinFreshCache writes a stale-but-present cache at the real cache path and
// returns that path. Stale, so a fetch is attempted rather than served fresh.
func pinFreshCache(t *testing.T) string {
	t.Helper()
	t.Setenv(ratesEnv, "") // a pinned local catalog would bypass the fetch under test
	home := t.TempDir()
	setHomeEnv(t, home)
	path := filepath.Join(home, ".wtm", "rates.json")
	writeFile(t, path, testCatalog, 0o644)
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
	t.Setenv(ratesEnv, "") // a pinned local catalog would bypass the fetch under test
	setHomeEnv(t, t.TempDir())
	c, err := loadCatalogFrom(serveRates(t, http.StatusOK, testCatalog))
	if err != nil {
		t.Fatalf("loadCatalogFrom: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(os.Getenv("HOME"), ".wtm", "rates.json"))
	if err != nil || string(raw) != testCatalog {
		t.Fatalf("cache = %q, err = %v, want the fetched body", raw, err)
	}
	assertCost(t, c.price(usageEvent{model: "m1", tier: "standard", tokens: claudeTokens{input: 1000}}), 0.001, true)
}

// TestRatesFetchErrorFallsBackToStale pins the outage path: an HTTP 500 with a
// stale cache on disk still serves old prices instead of unknown, and records
// the failed attempt so the retry cools down.
func TestRatesFetchErrorFallsBackToStale(t *testing.T) {
	path := pinFreshCache(t)
	c, err := loadCatalogFrom(serveRates(t, http.StatusInternalServerError, "boom"))
	if err != nil {
		t.Fatalf("stale fallback: %v", err)
	}
	if _, ok := c["m1"]; !ok {
		t.Fatal("stale cache was not used")
	}
	if _, err := os.Stat(attemptPath(path)); err != nil {
		t.Fatal("failed fetch did not record the attempt")
	}
}

// TestRatesFetchErrorWithoutCache pins the cold-outage path: an HTTP 500 with
// no cache fails the load, so the caller reports unknown, never invented prices.
func TestRatesFetchErrorWithoutCache(t *testing.T) {
	t.Setenv(ratesEnv, "") // a pinned local catalog would bypass the fetch under test
	setHomeEnv(t, t.TempDir())
	path := filepath.Join(os.Getenv("HOME"), ".wtm", "rates.json")
	if _, err := loadCatalogFrom(serveRates(t, http.StatusInternalServerError, "boom")); err == nil {
		t.Fatal("HTTP 500 with no cache must fail the load")
	}
	if _, err := os.Stat(attemptPath(path)); err != nil {
		t.Fatal("failed fetch did not record the attempt")
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

// TestPriceLongContextPartialAboveFallsBack pins the per-class merge: a catalog
// entry with only the 1h-write above rate still prices other classes above
// 200k context at their standard rates, not unknown.
func TestPriceLongContextPartialAboveFallsBack(t *testing.T) {
	c := catalog{"mx": {standard: tokenRates{input: fptr(0.000001)}, above: tokenRates{cacheWrite1h: fptr(0.000003)}, hasAbove: true}}
	e := usageEvent{model: "mx", tier: "standard", tokens: claudeTokens{input: 250_000, cacheWrite1h: 10_000}}
	assertCost(t, c.price(e), 250_000*0.000001+10_000*0.000003, true)
}
