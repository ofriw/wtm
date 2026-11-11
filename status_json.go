package main

import (
	"encoding/json"
	"time"
)

// status_json.go — the status machine contract: the JSON shape, its optional
// cost fields and the projection from usage accumulators. Rendering never
// leaks into this file.

type jsonWorktree struct {
	Path          string                      `json:"path"`
	Branch        string                      `json:"branch"`
	Upstream      string                      `json:"upstream"`
	Main          bool                        `json:"main"`
	Current       bool                        `json:"current"`
	LastUsed      *string                     `json:"lastUsed"`
	Status        string                      `json:"status"`
	AgeSeconds    *int64                      `json:"ageSeconds"`
	Cost          *float64                    `json:"cost"`
	CostEstimated bool                        `json:"costEstimated,omitempty"`
	ListCost      *optCost                    `json:"listCost,omitempty"`
	ListCostEst   bool                        `json:"listCostEstimated,omitempty"`
	ActiveSeconds *int64                      `json:"activeSeconds,omitempty"`
	Usage         map[string]jsonHarnessUsage `json:"usage,omitempty"`
	// Partial reports a total that lost an unreadable session, so a partial
	// sum never reads as complete. Omitted when the total is whole.
	Partial bool `json:"partial,omitempty"`
	// InputOutputRatio is the worktree-level input:output ratios; omitted when
	// the worktree recorded no output to divide by.
	InputOutputRatio *jsonInputOutputRatio `json:"inputOutputRatio,omitempty"`
	Integrations     map[string]string     `json:"integrations"`
	// Temp metadata carries the reclaim countdown; expiresAt is null for a
	// permanent checkout, so a consumer never re-derives the deadline.
	Temp      bool    `json:"temp"`
	ExpiresAt *string `json:"expiresAt"`
}

// jsonHarnessUsage is one harness's machine contract. cost is always present
// (null when unpriced); activeSeconds and models follow the harness's events.
type jsonHarnessUsage struct {
	Cost          *float64                  `json:"cost"`
	CostEstimated bool                      `json:"costEstimated,omitempty"`
	ListCost      *optCost                  `json:"listCost,omitempty"`
	ListCostEst   bool                      `json:"listCostEstimated,omitempty"`
	ActiveSeconds int64                     `json:"activeSeconds"`
	Partial       bool                      `json:"partial,omitempty"`
	Models        map[string]jsonModelUsage `json:"models"`
}

type jsonModelUsage struct {
	Cost            *float64                 `json:"cost"`
	CostEstimated   bool                     `json:"costEstimated,omitempty"`
	ListCost        *optCost                 `json:"listCost,omitempty"`
	ListCostEst     bool                     `json:"listCostEstimated,omitempty"`
	ActiveSeconds   int64                    `json:"activeSeconds"`
	Partial         bool                     `json:"partial,omitempty"`
	Tiers           map[string]jsonTierUsage `json:"tiers,omitempty"`
	CacheMissTokens *int64                   `json:"cacheMissTokens,omitempty"`
	CacheHitRate    *float64                 `json:"cacheHitRate,omitempty"`
}

// jsonTierUsage is one token tier's machine contract: the token count plus the
// billed cost and optional list price, mirroring the model-level cost shape so
// the billed/list invariant holds identically at every level.
type jsonTierUsage struct {
	Tokens        int64    `json:"tokens"`
	Cost          *float64 `json:"cost"`
	CostEstimated bool     `json:"costEstimated,omitempty"`
	ListCost      *optCost `json:"listCost,omitempty"`
	ListCostEst   bool     `json:"listCostEstimated,omitempty"`
}

// jsonInputOutputRatio is the machine contract for the summary's three
// input:output ratios. The three share one denominator, so total = cached +
// uncached before rounding; each field is trimmed to 1e-6 independently (see
// roundRate), so the rounded values can differ by that much. The JSON carries
// each so a consumer never re-derives the split.
type jsonInputOutputRatio struct {
	Total    float64 `json:"total"`
	Cached   float64 `json:"cached"`
	Uncached float64 `json:"uncached"`
	// Partial marks shares computed from a total that lost records. The values
	// remain the readable subset; the flag says they may change.
	Partial bool `json:"partial,omitempty"`
}

// optCost is the optional list price beside a machine cost. The key is present
// only when a proven subscription makes the billed and list values differ (see
// listCostJSON); its nil pointer is therefore not a "no subscription" signal.
// When present, a nil v renders null: the subscription is proven but the list
// is unknown.
type optCost struct{ v *float64 }

func (o optCost) MarshalJSON() ([]byte, error) {
	if o.v == nil {
		return []byte("null"), nil
	}
	return json.Marshal(*o.v)
}

// listCostJSON emits the list price only when showsList() is true and at least
// one side is priced. Unproven output, a proven equal price, and a proven
// both-unknown partial all omit the key. When present, a null value means the
// list is unknown.
func listCostJSON(p costPair) *optCost {
	if !p.showsList() || p.bothUnpriced() {
		return nil
	}
	return &optCost{v: p.list.jsonValue()}
}

func printStatusJSON(wts []worktree, now time.Time, ttl time.Duration, current string) error {
	active := activeCapabilities(wts)
	out := make([]jsonWorktree, 0, len(wts))
	for _, w := range wts {
		out = append(out, jsonWorktreeFrom(w, now, ttl, active, current))
	}
	return printJSON(out)
}

// jsonWorktreeFrom projects one worktree into the machine contract.
func jsonWorktreeFrom(w worktree, now time.Time, ttl time.Duration, active []capability, current string) jsonWorktree {
	j := jsonWorktree{
		// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
		Path: w.Path, Branch: w.branchName(), Upstream: w.Upstream.Short, Main: w.Main,
		Current: w.Path == current, Status: w.status(now, ttl),
		Integrations: integrationStates(w, active),
	}
	applyCostJSON(&j, w.cost())
	j.LastUsed = rfc3339Ptr(w.LastUsed)
	j.AgeSeconds = ageSeconds(w, now)
	j.InputOutputRatio = inputOutputRatioJSON(w.Usage)
	applyUsageJSON(&j, w.Usage)
	applyTempJSON(&j, w)
	return j
}

// applyCostJSON projects the billed/list pair; listCost is emitted only when a
// proven subscription makes it differ, and its estimate flag qualifies it.
func applyCostJSON(j *jsonWorktree, cost costPair) {
	j.Cost = cost.billed.jsonValue()
	j.CostEstimated = cost.billed.isEstimated()
	if lc := listCostJSON(cost); lc != nil {
		j.ListCost = lc
		j.ListCostEst = cost.list.isEstimated()
	}
}

// applyTempJSON mirrors the table's TEMP cell: expiresAt is emitted only for a
// temp checkout, so a permanent one serializes temp:false/expiresAt:null.
func applyTempJSON(j *jsonWorktree, w worktree) {
	j.Temp = w.Temp
	if w.Temp {
		s := formatTempDeadline(w.tempExpiresAt())
		j.ExpiresAt = &s
	}
}

func rfc3339Ptr(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339)
	return &s
}

func ageSeconds(w worktree, now time.Time) *int64 {
	if w.Created.IsZero() {
		return nil
	}
	s := seconds(now.Sub(w.Created))
	return &s
}

// applyUsageJSON adds the event-derived fields; an eventless worktree omits
// them. A total loss keeps partial:true but no measured values: activeSeconds
// and usage would otherwise read as a false measured zero.
func applyUsageJSON(j *jsonWorktree, u *worktreeUsage) {
	if u == nil {
		return
	}
	j.Partial = u.partial
	if !u.hasEvents() {
		return
	}
	a := seconds(u.active())
	j.ActiveSeconds = &a
	j.Usage = usageJSON(u)
}

func usageJSON(u *worktreeUsage) map[string]jsonHarnessUsage {
	out := make(map[string]jsonHarnessUsage, len(u.harnesses))
	for name, h := range u.harnesses {
		out[name] = harnessUsageJSON(h)
	}
	return out
}

// harnessUsageJSON projects one harness's totals and its per-model contracts.
func harnessUsageJSON(h *harnessUsage) jsonHarnessUsage {
	models := make(map[string]jsonModelUsage, len(h.models))
	for model, m := range h.models {
		models[model] = modelUsageJSON(m)
	}
	hu := jsonHarnessUsage{Cost: h.cost.billed.jsonValue(), CostEstimated: h.cost.billed.isEstimated(), ActiveSeconds: seconds(h.active()), Partial: h.partial, Models: models}
	if lc := listCostJSON(h.cost); lc != nil {
		hu.ListCost = lc
		hu.ListCostEst = h.cost.list.isEstimated()
	}
	return hu
}

// modelUsageJSON projects one model's totals, derived cache metrics and list price.
func modelUsageJSON(m *modelUsage) jsonModelUsage {
	mu := jsonModelUsage{Cost: m.cost.billed.jsonValue(), CostEstimated: m.cost.billed.isEstimated(), ActiveSeconds: seconds(m.active()), Partial: m.partial, Tiers: tiersJSON(m.tokens, m.tiers)}
	mu.CacheMissTokens, mu.CacheHitRate = cacheJSON(m.tokens)
	if lc := listCostJSON(m.cost); lc != nil {
		mu.ListCost = lc
		mu.ListCostEst = m.cost.list.isEstimated()
	}
	return mu
}

// tiersJSON projects a model's token tiers into the machine contract. It returns
// nil when no tier recorded tokens, so a cost-only legacy record (an older Pi
// session or spawn result) omits the breakdown instead of four empty tiers.
func tiersJSON(tokens promptTokens, tiers tokenTiers) map[string]jsonTierUsage {
	if tokens.total() == 0 {
		return nil
	}
	out := make(map[string]jsonTierUsage, numTiers)
	for t := tier(0); t < numTiers; t++ {
		tu := jsonTierUsage{Tokens: tokens.tier(t), Cost: tiers[t].billed.jsonValue(), CostEstimated: tiers[t].billed.isEstimated()}
		if lc := listCostJSON(tiers[t]); lc != nil {
			tu.ListCost = lc
			tu.ListCostEst = tiers[t].list.isEstimated()
		}
		out[tierMeta[t]] = tu
	}
	return out
}

// cacheJSON projects a model's derived cache metrics into the machine contract.
// Both keys are omitted when the model recorded no prompt tokens. The hit rate
// is also omitted when the guard trips — hits with no recorded build make the
// ratio undefined — so the contract carries no false 100%.
func cacheJSON(t promptTokens) (*int64, *float64) {
	if t.context() == 0 {
		return nil, nil
	}
	miss := t.miss()
	if rate, ok := t.hitRate(); ok {
		r := roundRate(rate)
		return &miss, &r
	}
	return &miss, nil
}

// set populates one field by JSON key. The switch keeps the struct type-safe
// while the meta list drives the enumeration, so display and JSON share one
// source of truth for the ratio set.
func (r *jsonInputOutputRatio) set(key string, v float64) {
	switch key {
	case "total":
		r.Total = v
	case "cached":
		r.Cached = v
	case "uncached":
		r.Uncached = v
	}
}

// inputOutputRatioJSON projects the worktree's input:output ratios. It returns
// nil when no output was recorded: a ratio without a denominator is undefined,
// so the contract stays silent rather than emit an infinity.
func inputOutputRatioJSON(u *worktreeUsage) *jsonInputOutputRatio {
	if u == nil {
		return nil
	}
	t := u.tokens()
	if !t.ratiosDefined() {
		return nil
	}
	r := jsonInputOutputRatio{Partial: u.partial}
	for _, d := range detailRatioMeta {
		v, _ := t.outputRatio(d.num(t))
		r.set(d.jsonKey, roundRate(v))
	}
	return &r
}

// integrationStates is the stable JSON contract: every capability id is always
// present, so adding an agent adds a key without changing the shape consumers
// switch on.
func integrationStates(w worktree, active []capability) map[string]string {
	m := make(map[string]string, len(capabilities()))
	for _, c := range capabilities() {
		m[c.id] = wideText(w.Caps[c.id], applicable(active, c.id))
	}
	return m
}
