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
	var ws workspace
	err = withProgress(g, statusProgressOptions(g), func(p *progress) error {
		var lerr error
		if ws, lerr = loadWorkspace(g, p, false); lerr != nil {
			return lerr
		}
		ws.loadUsage(p)
		return nil
	})
	if err != nil {
		return err
	}
	now := nowFunc()
	current := currentWorktreePath(ws)
	if g.json {
		return printStatusJSON(ws.wts, now, ws.ttl, current)
	}
	renderStatus(ws, wide, now, current, g.color)
	return nil
}

// statusProgressOptions erases the line before the table renders: the table is
// the product, and a leftover frame above it is noise. An explicit
// --progress always keeps the line, because the user asked for the record.
func statusProgressOptions(g *globals) progressOptions {
	return progressOptions{transient: g.progress != progressAlways}
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
// A malformed value is a test-setup error, so warnNowEnv discloses it once
// before any renderer starts: a raw warning inside the frame is erased.
func nowFunc() time.Time {
	if t, err := parseNowEnv(); err == nil {
		return t
	}
	return time.Now()
}

// parseNowEnv parses the WTM_NOW seam. An absent value is a parse error, so
// nowFunc falls back to the wall clock without special-casing it.
func parseNowEnv() (time.Time, error) {
	return time.Parse(time.RFC3339, os.Getenv("WTM_NOW"))
}

// warnNowEnv runs at dispatch, before any subcommand can open a renderer.
func warnNowEnv() {
	raw := os.Getenv("WTM_NOW")
	if raw == "" {
		return
	}
	if _, err := parseNowEnv(); err != nil {
		warnf("WTM_NOW %q is not RFC3339: using the wall clock", raw)
	}
}

// createdAtEnv is the WTM_CREATED_AT test seam. Fixtures cannot seed a
// filesystem birth time, so the scenario harness pins it; production never
// sets it. A malformed value is a test-setup error, so warnCreatedAtEnv
// discloses it once while worktreeCreated falls through to the FS fact.
const createdAtEnv = "WTM_CREATED_AT"

// parseCreatedAtEnv parses the creation seam. Absent and malformed values both
// report ok=false, so the caller uses the filesystem birth time.
func parseCreatedAtEnv() (time.Time, bool) {
	raw := os.Getenv(createdAtEnv)
	if raw == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	return t, err == nil
}

// warnCreatedAtEnv runs at dispatch beside warnNowEnv: a raw warning inside a
// renderer frame would be erased by the next frame.
func warnCreatedAtEnv() {
	raw := os.Getenv(createdAtEnv)
	if raw == "" {
		return
	}
	if _, ok := parseCreatedAtEnv(); !ok {
		warnf("%s %q is not RFC3339: using the filesystem birth time", createdAtEnv, raw)
	}
}

// loadUsage attaches every worktree's cost/active-time summary. The catalog is
// loaded only when the repo owns a Claude session, so a Pi-only repo never
// reaches the network. An unreadable session degrades to a warning: status is
// read-only and must still report what it knows.
func (ws *workspace) loadUsage(p *progress) {
	subs := loadSubscriptions()
	warnParentScopeDisabled(subs, p)
	ws.parseUsage(ws.loadCatalog(p), subs, p)
}

// loadCatalog resolves the price catalog, naming its source in the progress
// label so a network wait reads as work, not as a hang.
func (ws *workspace) loadCatalog(p *progress) catalog {
	if !ws.idx.hasHarness(ws.wts, claudeAgent.name) {
		return nil
	}
	p.phase("loading rate catalog", 0)
	cat, err := loadCatalogReporting(p)
	if err != nil {
		warnProgress(p, "claude cost unavailable: %s", err)
		// A nil catalog makes every harness report unknown, which is the honest
		// answer when no catalog is available at all.
		return nil
	}
	return cat
}

// usageResult carries one worker's outcome, so warnings emit from the parent in
// worktree order after the join (a worker must not write stderr; parallel.go).
type usageResult struct {
	usage *worktreeUsage
	err   error
}

// parseUsage reads every worktree's sessions concurrently: each worktree owns
// disjoint session files, and the shared proof is read-only, so the folds stay
// independent. The label follows the worktree a worker picked up; results
// attach by index after the join, keeping the render order tied to git's order,
// not to completion order.
func (ws *workspace) parseUsage(cat catalog, subs subscriptions, p *progress) {
	p.phase("parsing sessions", len(ws.wts))
	if len(ws.wts) == 0 {
		return
	}
	usages := mapParallel(len(ws.wts), scanWorkers, func(i int) usageResult {
		path := ws.wts[i].Path
		p.detail(displayPath(path))
		u, err := buildUsage(path, ws.idx, cat, subs)
		p.advance(1)
		// Keep a total loss so the loss reads as unknown, never as no usage.
		if u.hasEvents() || u.partial {
			return usageResult{usage: &u, err: err}
		}
		return usageResult{err: err}
	})
	for i, r := range usages {
		ws.wts[i].Usage = r.usage
		if r.err != nil {
			warnProgress(p, "usage for %s: %s", displayPath(ws.wts[i].Path), r.err)
		}
	}
}

// cost is the worktree's price pair; empty when it has no events.
func (w worktree) cost() costPair {
	if w.Usage == nil {
		return costPair{}
	}
	return w.Usage.cost()
}
