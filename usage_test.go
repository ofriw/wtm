package main

import (
	"math/rand"
	"slices"
	"testing"
	"time"
)

// usage_test.go — external contracts for the cost/active-time domain: cost
// accumulation, unknown propagation, interval union and the display grammar.

func at(sec int) time.Time { return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC) }

// pairPricer adapts a list-only pricer to the costPair fold contract: unit
// tests care about the pay-as-you-go value, so billed mirrors list.
func pairPricer(f func(usageEvent) money) func(usageEvent) pricedEvent {
	return func(e usageEvent) pricedEvent {
		m := f(e)
		return pricedEvent{total: costPair{list: m, billed: m}}
	}
}

func TestMoneyCellAndJSON(t *testing.T) {
	var unknown money
	unknown.addUnknown()
	var estimated money
	estimated.addEstimated(1.23)
	cases := []struct {
		name    string
		m       money
		cell    string
		jsonNil bool
	}{
		{"absent is dash and null", money{}, "-", true},
		{"unknown is unknown and null", unknown, "unknown", true},
		{"estimated is tilde and a number", estimated, "~$1.23", false},
		{"zero is a real zero", money{any: true}, "$0.00", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.m.cell(); got != tc.cell {
				t.Errorf("cell = %q, want %q", got, tc.cell)
			}
			if got := tc.m.jsonValue() == nil; got != tc.jsonNil {
				t.Errorf("jsonValue nil = %v, want %v", got, tc.jsonNil)
			}
		})
	}
	// A known cost survives rounding noise.
	m := money{}
	m.addKnown(41.2)
	if got := m.cell(); got != "$41.20" {
		t.Errorf("cell = %q, want $41.20", got)
	}
}

// TestMoneyUnknownIsSticky pins that a partial total never looks complete.
func TestMoneyUnknownIsSticky(t *testing.T) {
	var m money
	m.addKnown(1.5)
	m.addUnknown()
	m.addKnown(2.0)
	if m.cell() != "unknown" {
		t.Fatalf("cell = %q, want unknown", m.cell())
	}
	if m.jsonValue() != nil {
		t.Fatalf("unknown total must serialize as null")
	}
}

// TestMoneyEstimatedIsStickyAndQualified pins that an assumption survives
// folding, that a later exact contribution keeps the flag, and that unknown
// still dominates an estimate.
func TestMoneyEstimatedIsStickyAndQualified(t *testing.T) {
	var m money
	m.addEstimated(1.0)
	m.addKnown(0.5)
	if got := m.cell(); got != "~$1.50" {
		t.Fatalf("cell = %q, want ~$1.50", got)
	}
	if !m.isEstimated() || m.jsonValue() == nil {
		t.Fatalf("estimated total lost its flag or its number: %+v", m)
	}
	m.addUnknown()
	if got := m.cell(); got != "unknown" {
		t.Fatalf("unknown must dominate estimated: %q", got)
	}
}

// TestCostPairEstimatedListRendersBesideExactBilled pins the subscription
// grammar: an exact zero billed beside an estimated list keeps both marks.
func TestCostPairEstimatedListRendersBesideExactBilled(t *testing.T) {
	p := costPair{billed: knownZero(), subscription: true}
	p.list.addEstimated(1.23)
	if got := p.cell(); got != "$0.00/~$1.23" {
		t.Fatalf("cell = %q, want $0.00/~$1.23", got)
	}
}

// TestCostPairShowsListIgnoresEstimateFlag pins the list rule: only priced-ness
// and amount show a list, never the estimated flag. An estimated list beside
// an equal exact billed stays single; a proven equal price shows no list.
func TestCostPairShowsListIgnoresEstimateFlag(t *testing.T) {
	equal := costPair{subscription: true}
	equal.billed.addKnown(1.23)
	equal.list.addEstimated(1.23)
	if equal.showsList() {
		t.Errorf("equal amounts must not show a list, estimated flag aside: %+v", equal)
	}
	if got := equal.cell(); got != "$1.23" {
		t.Errorf("cell = %q, want $1.23", got)
	}
	exact := costPair{subscription: true}
	exact.billed.addKnown(2.5)
	exact.list.addKnown(2.5)
	if exact.showsList() {
		t.Errorf("proven equal price must not show a list: %+v", exact)
	}
	unproven := costPair{}
	unproven.billed.addKnown(0)
	unproven.list.addKnown(9.99)
	if unproven.showsList() {
		t.Errorf("unproven pair must not show a list: %+v", unproven)
	}
	knownUnknown := costPair{subscription: true}
	knownUnknown.billed.addKnown(0)
	knownUnknown.list.addUnknown()
	if !knownUnknown.showsList() {
		t.Errorf("known billed beside unknown list must show: %+v", knownUnknown)
	}
	bothUnknown := costPair{subscription: true}
	bothUnknown.billed.addUnknown()
	bothUnknown.list.addUnknown()
	if bothUnknown.showsList() {
		t.Errorf("two unpriced sides must not show a list: %+v", bothUnknown)
	}
}

func TestUnionDuration(t *testing.T) {
	cases := []struct {
		name  string
		spans []span
		want  time.Duration
	}{
		{"empty", nil, 0},
		{"single point is zero", []span{{at(0), at(0)}}, 0},
		{"disjoint adds", []span{{at(0), at(10)}, {at(20), at(25)}}, 15 * time.Second},
		{"overlap counts once", []span{{at(0), at(10)}, {at(5), at(15)}}, 15 * time.Second},
		{"touching merges", []span{{at(0), at(10)}, {at(10), at(20)}}, 20 * time.Second},
		{"contained span ignored", []span{{at(0), at(20)}, {at(5), at(10)}}, 20 * time.Second},
		{"unsorted input", []span{{at(20), at(25)}, {at(0), at(10)}}, 15 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := unionDuration(tc.spans); got != tc.want {
				t.Errorf("unionDuration = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestWorktreeUsageAggregates pins the three active-time scopes and cost
// accumulation for two sessions in one worktree.
func TestWorktreeUsageAggregates(t *testing.T) {
	costs := map[string]float64{"a": 1.0, "b": 2.0}
	price := pairPricer(func(e usageEvent) money {
		var m money
		if c, ok := costs[e.model]; ok {
			m.addKnown(c)
		} else {
			m.addUnknown()
		}
		return m
	})
	var u worktreeUsage
	// Session 1: model a at t0..t10, model b at t5..t15.
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "a"}, {at: at(10), model: "a"}, {at: at(5), model: "b"}, {at: at(15), model: "b"},
	}, price)
	// Session 2: model a at t100..t110.
	u.addSession("claude", []usageEvent{{at: at(100), model: "a"}, {at: at(110), model: "a"}}, price)
	u.addSession("claude", []usageEvent{{at: at(105), model: "cc"}}, price)

	if !u.hasEvents() {
		t.Fatal("hasEvents = false, want true")
	}
	// Worktree span unions session 1 (0..15) and session 2 (100..110).
	if got := u.active(); got != 25*time.Second {
		t.Errorf("worktree active = %v, want 25s", got)
	}
	pi := u.harnesses["pi"]
	if got := pi.active(); got != 15*time.Second {
		t.Errorf("pi active = %v, want 15s", got)
	}
	if got := pi.models["a"].active(); got != 10*time.Second {
		t.Errorf("pi model a active = %v, want 10s", got)
	}
	claude := u.harnesses["claude"]
	if got := claude.active(); got != 10*time.Second {
		t.Errorf("claude active = %v, want 10s", got)
	}
	// claude cost = a(1)+a(1)+cc(unknown).
	if claude.cost.cell() != "unknown" {
		t.Errorf("claude cost = %q, want unknown", claude.cost.cell())
	}
	if pi.cost.cell() != "$6.00" {
		t.Errorf("pi cost = %q, want $6.00", pi.cost.cell())
	}
}

// TestModelRunsDoNotInheritIdleTime pins the run rule: consecutive same-model
// events chain (first→last), but a different model closes the run, so an
// interleaved model never inherits another model's idle time.
func TestModelRunsDoNotInheritIdleTime(t *testing.T) {
	known := pairPricer(func(usageEvent) money {
		var m money
		m.addKnown(0)
		return m
	})
	var u worktreeUsage
	// Session: a@0, b@10, a@20. Model b closes a's first run, so a is two points.
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "a"}, {at: at(10), model: "b"}, {at: at(20), model: "a"},
	}, known)
	if got := u.harnesses["pi"].models["a"].active(); got != 0 {
		t.Errorf("model a active = %v, want 0 (interleaved points)", got)
	}
	if got := u.harnesses["pi"].active(); got != 20*time.Second {
		t.Errorf("harness active = %v, want 20s", got)
	}
	// Consecutive same-model events still chain into one span.
	var v worktreeUsage
	v.addSession("pi", []usageEvent{{at: at(0), model: "a"}, {at: at(10), model: "a"}}, known)
	if got := v.harnesses["pi"].models["a"].active(); got != 10*time.Second {
		t.Errorf("chained run = %v, want 10s", got)
	}
}

// TestSpawnSpanExtendsActiveTime pins that an event with an end (a spawn child)
// contributes its running time to the model and the session, not a single point.
func TestSpawnSpanExtendsActiveTime(t *testing.T) {
	known := pairPricer(func(usageEvent) money {
		var m money
		m.addKnown(0)
		return m
	})
	var u worktreeUsage
	u.addSession("pi", []usageEvent{
		{at: at(0), model: "parent"},
		{at: at(10), end: at(70), model: "child"},
	}, known)
	if got := u.harnesses["pi"].models["child"].active(); got != 60*time.Second {
		t.Errorf("child active = %v, want 60s", got)
	}
	if got := u.harnesses["pi"].active(); got != 70*time.Second {
		t.Errorf("harness active = %v, want 70s", got)
	}
}

func TestMergedModelOrder(t *testing.T) {
	var u worktreeUsage
	price := pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(map[string]float64{"cheap": 1, "pricey": 5}[e.model])
		return m
	})
	u.addSession("claude", []usageEvent{{at: at(0), model: "cheap"}}, price)
	u.addSession("pi", []usageEvent{{at: at(0), model: "cheap"}, {at: at(0), model: "pricey"}}, price)
	models := u.mergedModels()
	if names := u.sortedModelNames(models); len(names) != 2 || names[0] != "pricey" || names[1] != "cheap" {
		t.Fatalf("sortedModelNames = %v, want cost-descending [pricey cheap]", names)
	}
}

// TestTimeGrammar pins the one duration grammar every time cell uses: the
// non-zero units from years to minutes, seconds only below a minute.
func TestTimeGrammar(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{45 * time.Minute, "45m"},
		{2*time.Hour + 14*time.Minute, "2h 14m"},
		{6 * time.Hour, "6h"},
		{55*time.Hour + time.Minute, "2d 7h 1m"},
		{12 * 24 * time.Hour, "12d"},
		{8 * 30 * 24 * time.Hour, "8M"},
		{3 * 365 * 24 * time.Hour, "3y"},
	}
	for _, tc := range cases {
		if got := humanDuration(tc.d); got != tc.want {
			t.Errorf("humanDuration(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestWorktreeCostUnknownAcrossHarnesses pins that one unpriced harness makes
// the worktree total unknown, so a partial sum never looks complete.
func TestWorktreeCostUnknownAcrossHarnesses(t *testing.T) {
	var u worktreeUsage
	known := pairPricer(func(usageEvent) money {
		var m money
		m.addKnown(2)
		return m
	})
	unknown := pairPricer(func(usageEvent) money {
		var m money
		m.addUnknown()
		return m
	})
	u.addSession("pi", []usageEvent{{at: at(0), model: "a"}}, known)
	u.addSession("claude", []usageEvent{{at: at(1), model: "b"}}, unknown)
	if got := u.cost().cell(); got != "unknown" {
		t.Errorf("worktree cost = %q, want unknown", got)
	}
}

// TestEstimatedZeroRendersTildeZero pins the overage grammar: a subscription
// $0 inside the allowance reads "~$0.00" and stays estimated.
func TestEstimatedZeroRendersTildeZero(t *testing.T) {
	m := estimatedZero()
	if got := m.cell(); got != "~$0.00" {
		t.Fatalf("cell = %q, want ~$0.00", got)
	}
	if !m.isEstimated() {
		t.Fatalf("estimatedZero lost its flag: %+v", m)
	}
	if m.jsonValue() == nil {
		t.Fatal("estimated zero must serialize as a number")
	}
}

// TestFoldEventUnbilledKeepsSpanNotCost pins the mask guard: a non-billed event
// still extends the run, while both cost sides stay absent and unknown-free.
func TestFoldEventUnbilledKeepsSpanNotCost(t *testing.T) {
	per := map[string]*modelUsage{}
	var h harnessUsage
	price := func(usageEvent) pricedEvent {
		var c costPair
		c.billed.addKnown(1)
		c.list.addKnown(1)
		return pricedEvent{total: c}
	}
	prev := foldEvent(per, &h, usageEvent{at: at(0), model: "m"}, false, "", price)
	foldEvent(per, &h, usageEvent{at: at(10), model: "m"}, false, prev, price)
	if got := per["m"].active(); got != 10*time.Second {
		t.Errorf("model active = %v, want 10s", got)
	}
	if got := h.cost.cell(); got != "-" {
		t.Errorf("harness cost = %q, want absent -", got)
	}
}

// TestClaudeTurnKey pins the turn identity: only the both-empty case is
// keyless, so unrelated id-less records never collapse.
func TestClaudeTurnKey(t *testing.T) {
	for _, tc := range []struct{ msg, req, want string }{
		{"", "", ""},
		{"a", "", "a:"},
		{"", "r", ":r"},
		{"a", "r", "a:r"},
	} {
		if got := claudeTurnKey(tc.msg, tc.req); got != tc.want {
			t.Errorf("claudeTurnKey(%q,%q) = %q, want %q", tc.msg, tc.req, got, tc.want)
		}
	}
}

// TestFoldSessionShortMaskBills pins the fail-to-bill rule: a billed mask
// shorter than the event list bills the uncovered tail, so a truncated mask
// never silently zeroes real cost. Spans fold every event either way.
func TestFoldSessionShortMaskBills(t *testing.T) {
	events := []usageEvent{
		{at: at(1), model: "m", cost: fptr(1)},
		{at: at(2), model: "m", cost: fptr(2)},
	}
	price := pairPricer(func(e usageEvent) money {
		var m money
		if e.cost != nil {
			m.addKnown(*e.cost)
		} else {
			m.addUnknown()
		}
		return m
	})
	var u worktreeUsage
	u.addBilledSession("pi", events, []bool{false}, price)
	if got := u.cost().cell(); got != "$2.00" {
		t.Errorf("cost = %q, want $2.00 (short mask bills the tail)", got)
	}
	if got := u.active(); got != time.Second {
		t.Errorf("active = %v, want 1s (spans ignore the mask)", got)
	}
}

// TestDedupeEqualScoresElectFirst pins the beats-tie rule: two copies of one
// billed turn with equal cost elect the first only, so a repeated transcript
// never double-bills.
func TestDedupeEqualScoresElectFirst(t *testing.T) {
	a := usageEvent{at: at(1), model: "m", key: "k", cost: fptr(1)}
	b := usageEvent{at: at(2), model: "m", key: "k", cost: fptr(1)}
	masks := dedupeKeyedEvents([][]usageEvent{{a}, {b}})
	if !masks[0][0] || masks[1][0] {
		t.Fatalf("masks = %v, want the first equal copy billed only", masks)
	}
	same := dedupeKeyedEvents([][]usageEvent{{a, b}})
	if !same[0][0] || same[0][1] {
		t.Fatalf("same-session masks = %v, want the first equal copy billed only", same)
	}
}

// TestCostPairReasons pins the legend selectivity: each estimate kind reports
// only its own reason, both merge, and an exact total reports neither.
func TestCostPairReasons(t *testing.T) {
	var estM money
	estM.addEstimated(1)
	knownM := func(v float64) money { var m money; m.addKnown(v); return m }
	cacheOnly := costPair{list: estM, billed: estM}
	if !cacheOnly.needsCacheLegend() || cacheOnly.needsOverageLegend() {
		t.Errorf("cache-only reasons = %+v, want cache only", cacheOnly.reasons())
	}
	overageOnly := costPair{list: knownM(1), billed: estimatedZero(), subscription: true}
	if overageOnly.needsCacheLegend() || !overageOnly.needsOverageLegend() {
		t.Errorf("overage-only reasons = %+v, want overage only", overageOnly.reasons())
	}
	both := costPair{list: estM, billed: estimatedZero(), subscription: true}
	if r := both.reasons(); !r.cache || !r.overage {
		t.Errorf("both reasons = %+v, want cache+overage", r)
	}
	// A subscription hides a list whose amount equals the billed one, so its
	// estimate must not ask for a cache line the cell cannot show.
	hidden := costPair{list: estM, billed: knownM(1), subscription: true}
	if hidden.showsList() || hidden.needsCacheLegend() {
		t.Errorf("hidden-list reasons = %+v, want no cache line", hidden.reasons())
	}
	exact := costPair{list: knownM(1), billed: knownM(0), subscription: true}
	if r := exact.reasons(); r.any() {
		t.Errorf("exact reasons = %+v, want none", r)
	}
	if got := (estimateReasons{cache: true}.merge(estimateReasons{overage: true})); !got.cache || !got.overage {
		t.Errorf("merge = %+v, want both", got)
	}
}

// TestPiTierBreakdownFolds pins the per-tier accumulator for a self-priced
// harness: the record's token counts and class costs land in the model's
// displayed tiers, so the sub-rows a reader sees are the record's own numbers.
func TestPiTierBreakdownFolds(t *testing.T) {
	var u worktreeUsage
	price := usagePricer(agent{name: "pi"}, nil, subscriptions{})
	u.addSession("pi", []usageEvent{{
		at: at(0), model: "m", cost: fptr(0.30),
		tokens:   promptTokens{input: 1000, output: 500, cacheRead: 2000, cacheWrite5m: 100},
		tierCost: &tokenCosts{input: fptr(0.10), output: fptr(0.15), cacheRead: fptr(0.04), cacheWrite: fptr(0.01)},
	}}, price)
	m := u.harnesses["pi"].models["m"]
	for _, tc := range []struct {
		tier tier
		cell string
	}{{tierInput, "$0.10"}, {tierCacheRead, "$0.04"}, {tierCacheWrite, "$0.01"}, {tierOutput, "$0.15"}} {
		if got := m.tiers[tc.tier].cell(); got != tc.cell {
			t.Errorf("%s tier cell = %q, want %q", tierMeta[tc.tier], got, tc.cell)
		}
	}
	if got := m.tokens.tier(tierCacheWrite); got != 100 {
		t.Errorf("cache write tokens = %d, want 100", got)
	}
	if got := m.tokens.total(); got != 3600 {
		t.Errorf("total tokens = %d, want 3600", got)
	}
}

// TestCoverTiersDiscountsEveryTier pins the invariant: a proven subscription
// replaces the billed side of the total AND of every priced tier, while the
// list side stays visible and an unpriced tier stays absent (never a fake $0).
func TestCoverTiersDiscountsEveryTier(t *testing.T) {
	var p pricedEvent
	p.total.list.addKnown(3)
	p.total.billed = p.total.list
	p.tiers[tierInput].list.addKnown(1)
	p.tiers[tierInput].billed = p.tiers[tierInput].list
	p.tiers[tierOutput].list.addKnown(2)
	p.tiers[tierOutput].billed = p.tiers[tierOutput].list

	coverTiers(&p, knownZero)
	if got := p.total.cell(); got != "$0.00/$3.00" {
		t.Errorf("total cell = %q, want $0.00/$3.00", got)
	}
	if got := p.tiers[tierInput].cell(); got != "$0.00/$1.00" {
		t.Errorf("input cell = %q, want $0.00/$1.00", got)
	}
	if got := p.tiers[tierOutput].cell(); got != "$0.00/$2.00" {
		t.Errorf("output cell = %q, want $0.00/$2.00", got)
	}
	if p.tiers[tierCacheRead].list.any || p.tiers[tierCacheRead].billed.any {
		t.Error("an unpriced tier must stay absent, not become a fake $0.00")
	}
	if p.tiers[tierInput].billed.isEstimated() {
		t.Error("an allowance-covered tier is an exact zero, not an estimate")
	}

	// Overage possible: every priced tier reads as the estimated zero.
	var e pricedEvent
	e.total.list.addKnown(1)
	e.tiers[tierInput].list.addKnown(1)
	coverTiers(&e, estimatedZero)
	if got := e.tiers[tierInput].cell(); got != "~$0.00/$1.00" {
		t.Errorf("overage tier cell = %q, want ~$0.00/$1.00", got)
	}
}

// TestFormatTokensBoundary pins the unit floor: values round down so none
// cross into the next unit (999999 reads 999.9k, not 1000.0k).
func TestFormatTokensBoundary(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{999, "999"},
		{1000, "1.0k"},
		{999999, "999.9k"},
		{1000000, "1.0M"},
	}
	for _, tc := range cases {
		if got := formatTokens(tc.n); got != tc.want {
			t.Errorf("formatTokens(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

// TestMergedModelsCollision pins the merged-row rule: one model name in two
// harnesses reads as one detail row with summed cost and tokens.
func TestMergedModelsCollision(t *testing.T) {
	var u worktreeUsage
	price := pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(*e.cost)
		return m
	})
	u.addSession("pi", []usageEvent{{at: at(0), model: "m", cost: fptr(1)}}, price)
	u.addSession("claude", []usageEvent{{at: at(0), model: "m", cost: fptr(2)}}, price)
	models := u.mergedModels()
	if len(models) != 1 {
		t.Fatalf("merged models = %d, want 1", len(models))
	}
	if got := models["m"].cost.cell(); got != "$3.00" {
		t.Errorf("merged cost = %q, want $3.00", got)
	}
	if names := u.sortedModelNames(models); len(names) != 1 || names[0] != "m" {
		t.Errorf("sorted names = %q, want [m]", names)
	}
}

// FuzzUnionDuration pins the union invariants over random spans: the cover
// stays within the summed lengths, and neither input order nor duplication
// changes it.
func FuzzUnionDuration(f *testing.F) {
	f.Add([]byte{0, 10, 5, 15, 20, 5})
	f.Fuzz(func(t *testing.T, data []byte) {
		var spans []span
		var sum time.Duration
		for i := 0; i+3 < len(data); i += 4 {
			start := int(data[i])<<8 | int(data[i+1])
			length := int(data[i+2])<<8 | int(data[i+3])
			spans = append(spans, span{start: at(start % 3600), end: at(start%3600 + length%3600)})
			sum += time.Duration(length%3600) * time.Second
		}
		got := unionDuration(spans)
		if got < 0 || got > sum {
			t.Fatalf("unionDuration = %v outside [0, %v]", got, sum)
		}
		rev := slices.Clone(spans)
		slices.Reverse(rev)
		if unionDuration(rev) != got {
			t.Fatalf("order changed the union: %v vs %v", unionDuration(rev), got)
		}
		if unionDuration(slices.Concat(spans, spans)) != got {
			t.Fatal("duplication changed the union")
		}
	})
}

// FuzzMoneyFold pins the fold invariants over random accumulations: the total
// is order-independent, unknown dominates, and the amount sums every priced part.
func FuzzMoneyFold(f *testing.F) {
	f.Add([]byte{0, 1, 2, 100})
	f.Fuzz(func(t *testing.T, data []byte) {
		var parts []money
		var want float64
		var anyUnknown, anyEstimated bool
		for _, b := range data {
			var p money
			switch b % 3 {
			case 0:
				p.addKnown(float64(b))
				want += float64(b)
			case 1:
				p.addEstimated(float64(b))
				want += float64(b)
				anyEstimated = true
			default:
				p.addUnknown()
				anyUnknown = true
			}
			parts = append(parts, p)
		}
		var fwd, rev money
		for _, p := range parts {
			fwd.add(p)
		}
		for i := len(parts) - 1; i >= 0; i-- {
			rev.add(parts[i])
		}
		if fwd.cell() != rev.cell() {
			t.Fatalf("fold order changed the cell: %q vs %q", fwd.cell(), rev.cell())
		}
		if anyUnknown != fwd.unknown || anyUnknown != rev.unknown {
			t.Fatal("unknown must be sticky regardless of order")
		}
		if anyEstimated && !anyUnknown && (!fwd.isEstimated() || !rev.isEstimated()) {
			t.Fatal("estimated must survive the fold")
		}
		if !anyUnknown && (fwd.amount != want || rev.amount != want) {
			t.Fatalf("amount = %v/%v, want %v", fwd.amount, rev.amount, want)
		}
	})
}

// TestDedupeRandomOrder pins the order invariant: shuffling sessions bills
// each key exactly once and the max-score copy wins, so resume/fork repeats
// never double-bill and the richest record always prices the turn.
func TestDedupeRandomOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	copies := map[string][]usageEvent{
		// Cost presence beats token size (see TestDedupeCostPresenceBeatsTokenSize).
		"cost": {{at: at(1), model: "m", key: "cost", cost: fptr(2), tokens: promptTokens{input: 100}},
			{at: at(2), model: "m", key: "cost", tokens: promptTokens{input: 5000}}},
		"tokens": {{at: at(1), model: "m", key: "tokens", tokens: promptTokens{input: 100}},
			{at: at(2), model: "m", key: "tokens", tokens: promptTokens{input: 900}}},
		"web": {{at: at(1), model: "m", key: "web", tokens: promptTokens{input: 100}, webSearch: 1},
			{at: at(2), model: "m", key: "web", tokens: promptTokens{input: 100}, webSearch: 4}},
		"price": {{at: at(1), model: "m", key: "price", cost: fptr(1), tokens: promptTokens{input: 100}},
			{at: at(2), model: "m", key: "price", cost: fptr(7), tokens: promptTokens{input: 100}}},
	}
	best := map[string]dedupeScore{}
	for k, evs := range copies {
		for _, e := range evs {
			if s := scoreOf(e); best[k] == (dedupeScore{}) || s.beats(best[k]) {
				best[k] = s
			}
		}
	}
	base := [][]usageEvent{
		{copies["cost"][0], copies["tokens"][0], {at: at(3), model: "m"}},
		{copies["web"][0], copies["price"][0]},
		{copies["cost"][1], copies["tokens"][1], copies["web"][1], copies["price"][1]},
	}
	for trial := 0; trial < 20; trial++ {
		sessions := slices.Clone(base)
		rng.Shuffle(len(sessions), func(i, j int) { sessions[i], sessions[j] = sessions[j], sessions[i] })
		for si := range sessions {
			sessions[si] = slices.Clone(sessions[si])
			rng.Shuffle(len(sessions[si]), func(i, j int) { sessions[si][i], sessions[si][j] = sessions[si][j], sessions[si][i] })
		}
		masks := dedupeKeyedEvents(sessions)
		billed := map[string]int{}
		for si, events := range sessions {
			for ei, e := range events {
				if !masks[si][ei] {
					continue
				}
				if e.key == "" {
					continue // keyless events always bill; not part of the invariant
				}
				billed[e.key]++
				if s := scoreOf(e); s != best[e.key] {
					t.Fatalf("trial %d: key %q billed %+v, want best %+v", trial, e.key, s, best[e.key])
				}
			}
		}
		for k := range copies {
			if billed[k] != 1 {
				t.Fatalf("trial %d: key %q billed %d times, want once", trial, k, billed[k])
			}
		}
	}
}

// TestTokensAccrueOnlyForBilled pins that dedupe masks prune tokens with
// cost: unbilled events keep spans but add no tokens.
func TestTokensAccrueOnlyForBilled(t *testing.T) {
	var u worktreeUsage
	price := pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(*e.cost)
		return m
	})
	events := []usageEvent{
		{at: at(0), model: "m", cost: fptr(1), tokens: promptTokens{input: 100}},
		{at: at(10), model: "m", cost: fptr(1), tokens: promptTokens{input: 100}},
	}
	u.addBilledSession("pi", events, []bool{true, false}, price)
	m := u.harnesses["pi"].models["m"]
	if got := m.tokens.input; got != 100 {
		t.Errorf("billed tokens = %d, want 100", got)
	}
	if got := m.cost.cell(); got != "$1.00" {
		t.Errorf("billed cost = %q, want $1.00", got)
	}
}

// TestWebSearchJoinsTotalNotTiers pins the tier gap: web search prices the
// total, never a tier, so tiers can sum below the model total.
func TestWebSearchJoinsTotalNotTiers(t *testing.T) {
	c := testRates(t)
	p := c.price(usageEvent{model: "m1", tier: "standard", webSearch: 2})
	if !p.total.list.known() || p.total.list.amount == 0 {
		t.Fatalf("web-search total must be known nonzero, got %+v", p.total.list)
	}
	for i := range p.tiers {
		if p.tiers[i].list.any {
			t.Errorf("tier %d must stay absent for web-search-only event", i)
		}
	}
}

// TestDedupeCostPresenceBeatsTokenSize pins the cost-presence precedence: a
// self-priced copy that carries its own cost must win over a larger token-only
// copy of the same turn, so a reverse-diffed Pi price is never dropped for a
// cost-less duplicate.
func TestDedupeCostPresenceBeatsTokenSize(t *testing.T) {
	a := usageEvent{at: at(1), model: "m", key: "k", cost: fptr(2), tokens: promptTokens{input: 100}}
	b := usageEvent{at: at(2), model: "m", key: "k", tokens: promptTokens{input: 5000}}
	masks := dedupeKeyedEvents([][]usageEvent{{a}, {b}})
	if !masks[0][0] || masks[1][0] {
		t.Fatalf("masks = %v, want the cost-bearing copy billed only", masks)
	}
}
