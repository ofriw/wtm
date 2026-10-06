package main

import (
	"path/filepath"
	"strings"
)

// capability.go — the integration registry. The grouped INTEGRATIONS cell, the
// opt-in --wide columns and the JSON contract all derive from capabilities(),
// so a new integration is one descriptor, never a renderer or schema edit.

// capState is one worktree's relationship to a capability. Applicability ("the
// project uses this capability nowhere") is a view concern and never stored, so
// a probe sees only the worktree in front of it.
type capState int

const (
	capAbsent capState = iota
	capPartial
	capPresent
)

// capInput is the per-worktree evidence a probe reads. It carries the parsed
// MCP inventory so probes never re-parse a config.
type capInput struct {
	root string
	mcp  map[string]bool
}

// capability is one detectable integration: a stable id (also the JSON key and
// the --wide header), a short token the grouped cell shows, and its probe. token
// must be a prefix of id, so the grouped cell and --wide share one vocabulary.
type capability struct {
	id    string
	token string
	probe func(capInput) capState
}

// capabilities is the SSOT. The fixed project integrations come first, then
// one per agent — so registering an agent in agents() also adds its token,
// wide column and JSON key with no edits here.
func capabilities() []capability {
	caps := []capability{chunkhoundCapability, mcpCapability}
	for _, a := range agents() {
		caps = append(caps, a.capability())
	}
	return caps
}

var (
	// chunkhound is partial when only one of config/db exists: a half-set-up
	// workspace is a state the old CONFIG/DB columns could not express.
	chunkhoundCapability = capability{
		id: "chunkhound", token: "chunk",
		probe: func(in capInput) capState {
			cfg := exists(filepath.Join(in.root, chunkhoundConfigFile))
			db := exists(chunkhoundDBPath(in.root))
			return presence(cfg && db, cfg || db)
		},
	}
	// mcp is MCP support of any kind: every recognized MCP config counts
	// equally (the shared project file, a harness's native config), so a
	// worktree wired through any one of them reads mcp=yes. The config that
	// wires a harness additionally lights that harness's own capability.
	mcpCapability = capability{
		id: "mcp", token: "mcp",
		probe: func(in capInput) capState { return binary(anyMCPConfig(in.mcp)) },
	}
)

func binary(ok bool) capState {
	if ok {
		return capPresent
	}
	return capAbsent
}

// presence reports partial when only some of a capability's artifacts exist.
func presence(full, some bool) capState {
	switch {
	case full:
		return capPresent
	case some:
		return capPartial
	default:
		return capAbsent
	}
}

// capabilityStates classifies one worktree for every registered capability.
func capabilityStates(in capInput) map[string]capState {
	states := make(map[string]capState, len(capabilities()))
	for _, c := range capabilities() {
		states[c.id] = c.probe(in)
	}
	return states
}

// activeCapabilities keeps, in registry order, the capabilities at least one
// worktree carries. A capability no worktree carries is not applicable to the
// project: it stays out of the grouped cell but reports "n/a" in wide/JSON.
func activeCapabilities(wts []worktree) []capability {
	var active []capability
	for _, c := range capabilities() {
		if anyWorktreeHas(wts, c.id) {
			active = append(active, c)
		}
	}
	return active
}

func anyWorktreeHas(wts []worktree, id string) bool {
	for _, w := range wts {
		if w.Caps[id] != capAbsent {
			return true
		}
	}
	return false
}

func applicable(active []capability, id string) bool {
	for _, c := range active {
		if c.id == id {
			return true
		}
	}
	return false
}

// isCapabilityHeader reports whether a grid header names a registered
// capability, so ui styling keys off the registry instead of a column list.
// Headers are always upper-cased from capability ids, so exact match suffices.
func isCapabilityHeader(header string) bool {
	for _, c := range capabilities() {
		if header == strings.ToUpper(c.id) {
			return true
		}
	}
	return false
}

// partialMarker flags a partially wired integration in the grouped cell.
const partialMarker = "~"

// groupedToken renders a capability the worktree carries: the token when fully
// wired, token~ when partial. Absent capabilities never reach here.
func (c capability) groupedToken(s capState) string {
	if s == capPartial {
		return c.token + partialMarker
	}
	return c.token
}

// wideText is the --wide/JSON state word. inUse false yields "n/a", which
// keeps "this project does not use it" distinct from an applicable-but-absent
// "-".
func wideText(s capState, inUse bool) string {
	if !inUse {
		return "n/a"
	}
	switch s {
	case capPresent:
		return "yes"
	case capPartial:
		return "partial"
	default:
		return "-"
	}
}

// column is the opt-in --wide projection: one column per capability, generated
// from the registry so the schema never grows by hand.
func (c capability) column() worktreeColumn {
	return worktreeColumn{
		header: strings.ToUpper(c.id),
		cell: func(w worktree, ctx columnContext) string {
			return wideText(w.Caps[c.id], applicable(ctx.active, c.id))
		},
		// Rank 1 elides capability columns before any core column gives up
		// width; the grouped INTEGRATIONS cell covers narrow terminals, so
		// --wide columns are a luxury, not a necessity. Floor 0 makes the
		// elision whole-column: a capability column is dropped, never rendered
		// as a sliver, so a budget that fits no capability column yields the
		// core columns alone.
		shrinkRank: 1,
	}
}
