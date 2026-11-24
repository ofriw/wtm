package core

import (
	"slices"
	"strings"
)

// capability.go — the integration registry. The grouped INTEGRATIONS cell, the
// opt-in --wide columns and the JSON contract all derive from Capabilities(),
// so a new integration is one descriptor, never a renderer or schema edit. The
// descriptors themselves (chunkhound, mcp, agents) are registered at the
// composition root (cli.Run → harness.RegisterCapabilities); this package owns
// the model and evaluation.

// CapState is one worktree's relationship to a capability. Applicability ("the
// project uses this capability nowhere") is a view concern and never stored, so
// a probe sees only the worktree in front of it.
type CapState int

const (
	CapAbsent CapState = iota
	CapPartial
	CapPresent
)

// CapInput is the per-worktree evidence a probe reads. It carries the parsed
// MCP inventory so probes never re-parse a config.
type CapInput struct {
	Root string
	MCP  map[string]bool
}

// Capability is one detectable integration: a stable id (also the JSON key and
// the --wide header), a short token the grouped cell shows, and its probe. Token
// must be a prefix of id, so the grouped cell and --wide share one vocabulary.
type Capability struct {
	ID    string
	Token string
	Probe func(CapInput) CapState
}

var capabilityRegistry []Capability

// RegisterCapability appends a capability, panicking on a core-column header
// collision at registration — not only when the harness happens to be linked.
// The check uses the real column header (assertNoCollision), so a header that
// stops equaling the upper-cased ID still surfaces here and at registry build.
func RegisterCapability(c Capability) {
	assertNoCollision(c, c.Column(), AllColumns())
	capabilityRegistry = append(capabilityRegistry, c)
}

// requireCapabilities panics on an empty registry: a silent empty integration
// set would hide a missing composition-root registration.
func requireCapabilities() []Capability {
	if len(capabilityRegistry) == 0 {
		panic("wtm: no capabilities registered; call harness.RegisterCapabilities() at the composition root")
	}
	return capabilityRegistry
}

// Capabilities is the SSOT for production readers. It fails fast on an empty
// registry (same panic as requireCapabilities) and returns a fresh copy so
// callers cannot mutate the live registry through the returned slice.
func Capabilities() []Capability { return slices.Clone(requireCapabilities()) }

// PresentIf reports present when ok.
func PresentIf(ok bool) CapState {
	if ok {
		return CapPresent
	}
	return CapAbsent
}

// Presence reports partial when only some of a capability's artifacts exist.
func Presence(full, some bool) CapState {
	switch {
	case full:
		return CapPresent
	case some:
		return CapPartial
	default:
		return CapAbsent
	}
}

// capabilityStates classifies one worktree for every registered capability.
func capabilityStates(in CapInput) map[string]CapState {
	caps := requireCapabilities()
	states := make(map[string]CapState, len(caps))
	for _, c := range caps {
		states[c.ID] = c.Probe(in)
	}
	return states
}

// ActiveCapabilities keeps, in registry order, the capabilities at least one
// worktree carries. A capability no worktree carries is not applicable to the
// project: it stays out of the grouped cell but reports "n/a" in wide/JSON.
func ActiveCapabilities(wts []Worktree) []Capability {
	var active []Capability
	for _, c := range requireCapabilities() {
		if anyWorktreeHas(wts, c.ID) {
			active = append(active, c)
		}
	}
	return active
}

func anyWorktreeHas(wts []Worktree, id string) bool {
	for _, w := range wts {
		if w.Caps[id] != CapAbsent {
			return true
		}
	}
	return false
}

// Applicable reports whether id is in the active capability set.
func Applicable(active []Capability, id string) bool {
	for _, c := range active {
		if c.ID == id {
			return true
		}
	}
	return false
}

// IsCapabilityHeader reports whether a grid header names a registered
// capability, so ui styling keys off the registry instead of a column list.
// Headers are always upper-cased from capability ids, so exact match suffices.
func IsCapabilityHeader(header string) bool {
	for _, c := range requireCapabilities() {
		if header == strings.ToUpper(c.ID) {
			return true
		}
	}
	return false
}

// PartialMarker is the suffix GroupedToken appends to a partially wired
// integration. cli's grouped-cell renderer keys the partial color off it, so
// the two cannot disagree about what "partial" looks like.
const PartialMarker = "~"

// GroupedToken renders a capability the worktree carries: the token when fully
// wired, token~ when partial. Absent capabilities never reach here.
func (c Capability) GroupedToken(s CapState) string {
	if s == CapPartial {
		return c.Token + PartialMarker
	}
	return c.Token
}

// Wide-state words produced by wideText; cli colors capability cells by
// comparing against these rather than re-spelling the literals.
const (
	StateWordNA      = "n/a"
	StateWordYes     = "yes"
	StateWordPartial = "partial"
	StateWordAbsent  = "-"
)

// wideText is the --wide/JSON state word. inUse false yields StateWordNA, which
// keeps "this project does not use it" distinct from an applicable-but-absent
// StateWordAbsent.
func (c Capability) wideText(s CapState, inUse bool) string {
	if !inUse {
		return StateWordNA
	}
	switch s {
	case CapPresent:
		return StateWordYes
	case CapPartial:
		return StateWordPartial
	default:
		return StateWordAbsent
	}
}

// Column is the opt-in --wide projection: one column per capability, generated
// from the registry so the schema never grows by hand.
func (c Capability) Column() WorktreeColumn {
	return WorktreeColumn{
		ID:     c.ID,
		Header: strings.ToUpper(c.ID),
		Cell: func(w Worktree, ctx ColumnContext) string {
			return c.wideText(w.Caps[c.ID], Applicable(ctx.Active, c.ID))
		},
		// Rank 1 elides capability columns before any core column gives up
		// width; the grouped INTEGRATIONS cell covers narrow terminals, so
		// --wide columns are a luxury, not a necessity. Floor 0 makes the
		// elision whole-column: a capability column is dropped, never rendered
		// as a sliver, so a budget that fits no capability column yields the
		// core columns alone.
		ShrinkRank: 1,
	}
}
