package main

import (
	"os"
	"time"
)

// status.go — the status command entry point and usage orchestration. The
// table, detail block and JSON contract live in status_table.go,
// status_detail.go and status_json.go.

func cmdStatus(g *globals, args []string) error {
	wide, err := parseStatusFlags(g, args)
	if err != nil {
		return err
	}
	ws, err := loadWorkspace(g, false, false)
	if err != nil {
		return err
	}
	ws.loadUsage()
	now := nowFunc()
	current := currentWorktreePath(ws)
	if g.json {
		return printStatusJSON(ws.wts, now, ws.ttl, current)
	}
	renderStatus(ws, wide, now, current, g.color)
	return nil
}

// renderStatus prints the table, its legend then the detail block in that order.
func renderStatus(ws workspace, wide bool, now time.Time, current, colorMode string) {
	budget := widthBudget()
	cols := statusSchema(wide, ws.wts)
	printStatusTable(cols, ws.wts, now, ws.ttl, colorMode, budget)
	printCostLegend(costLegendNeeded(cols, ws.wts, current, now, ws.ttl, budget), colorMode, budget)
	printStatusDetail(ws.wts, current, now, colorMode, budget)
}

// parseStatusFlags parses and validates the status flags. --wide shapes only
// the table; the JSON contract always reports every integration, so the
// combination is a usage error, not a silent no-op.
func parseStatusFlags(g *globals, args []string) (bool, error) {
	fs := subFlags("status", g)
	wide := fs.Bool("wide", false, "show one column per integration")
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return false, err
	}
	if len(pos) > 0 {
		return false, usagef("status: unexpected argument %q", pos[0])
	}
	if *wide && g.json {
		return false, usagef("status: --wide has no effect with --json (JSON already reports all integrations)")
	}
	return *wide, nil
}

// nowFunc is the clock seam. Production reads the wall clock; the container
// harness pins WTM_NOW so every clock read is deterministic in goldens: status
// ages, worktreeLastUsed and the gc TTL decision all draw from this one seam.
// A malformed value is a test-setup error, so disclose it instead of hiding it.
func nowFunc() time.Time {
	s := os.Getenv("WTM_NOW")
	if s == "" {
		return time.Now()
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		warnf("WTM_NOW %q is not RFC3339: using the wall clock", s)
		return time.Now()
	}
	return t
}

// loadUsage attaches every worktree's cost/active-time summary. The catalog is
// loaded only when the repo owns a Claude session, so a Pi-only repo never
// reaches the network. An unreadable session degrades to a warning: status is
// read-only and must still report what it knows.
func (ws *workspace) loadUsage() {
	cat := ws.usageCatalog()
	// Read the credential proof once: it is a per-run snapshot, not per-worktree.
	subs := loadSubscriptions()
	for i := range ws.wts {
		ws.attachUsage(i, cat, subs)
	}
}

// usageCatalog loads the price catalog only for a repo with Claude sessions, so
// a Pi-only repo never reaches the network.
func (ws *workspace) usageCatalog() catalog {
	if !ws.idx.hasHarness(ws.wts, claudeAgent.name) {
		return nil
	}
	cat, err := loadCatalog()
	if err != nil {
		warnf("claude cost unavailable: %s", err)
		// A nil catalog makes every harness report unknown, which is the honest
		// answer when no catalog is available at all.
		return nil
	}
	return cat
}

// attachUsage stores one worktree's usage, keeping a total loss so the loss
// reads as unknown, never as no usage.
func (ws *workspace) attachUsage(i int, cat catalog, subs subscriptions) {
	u, err := buildUsage(ws.wts[i].Path, ws.idx, cat, subs)
	if err != nil {
		warnf("usage for %s: %s", displayPath(ws.wts[i].Path), err)
	}
	if u.hasEvents() || u.partial {
		ws.wts[i].Usage = &u
	}
}

// cost is the worktree's price pair; empty when it has no events.
func (w worktree) cost() costPair {
	if w.Usage == nil {
		return costPair{}
	}
	return w.Usage.cost()
}
