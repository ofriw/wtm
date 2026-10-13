package main

import (
	"testing"
	"time"
)

// usage_test.go — external contracts for the cost/active-time domain: cost
// accumulation, unknown propagation, interval union and the display grammar.

func at(sec int) time.Time { return time.Date(2026, 1, 1, 0, 0, sec, 0, time.UTC) }

// pairPricer adapts a list-only pricer to the costPair fold contract: unit
// tests care about the pay-as-you-go value, so billed mirrors list.
func pairPricer(f func(usageEvent) money) func(usageEvent) costPair {
	return func(e usageEvent) costPair {
		m := f(e)
		return costPair{list: m, billed: m}
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

func TestHarnessAndModelOrder(t *testing.T) {
	var u worktreeUsage
	price := pairPricer(func(e usageEvent) money {
		var m money
		m.addKnown(map[string]float64{"cheap": 1, "pricey": 5}[e.model])
		return m
	})
	u.addSession("claude", []usageEvent{{at: at(0), model: "cheap"}}, price)
	u.addSession("pi", []usageEvent{{at: at(0), model: "cheap"}, {at: at(0), model: "pricey"}}, price)
	if names := u.harnessNames(); len(names) != 2 || names[0] != "pi" || names[1] != "claude" {
		t.Fatalf("harnessNames = %v, want [pi claude]", names)
	}
	if names := u.harnesses["pi"].modelNames(); names[0] != "pricey" || names[1] != "cheap" {
		t.Fatalf("modelNames = %v, want cost-descending [pricey cheap]", names)
	}
}

func TestTimeGrammar(t *testing.T) {
	age := []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{6 * time.Hour, "6h"},
		{12 * 24 * time.Hour, "12d"},
		{8 * 30 * 24 * time.Hour, "8M"},
		{3 * 365 * 24 * time.Hour, "3y"},
	}
	for _, tc := range age {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	active := []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Minute, "45m"},
		{2*time.Hour + 14*time.Minute, "2h 14m"},
	}
	for _, tc := range active {
		if got := humanActive(tc.d); got != tc.want {
			t.Errorf("humanActive(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
	// The detail TIME column always names the hour.
	if got := humanClock(45 * time.Minute); got != "0h 45m" {
		t.Errorf("humanClock(45m) = %q, want 0h 45m", got)
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
