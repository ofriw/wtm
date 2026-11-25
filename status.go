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
	// One clock read for the whole run: usage clamping, ages, the table and the
	// JSON must all agree. Reading before withProgress means a slow rate-catalog
	// fetch cannot split the report instant.
	now := nowFunc()
	ws, err := loadStatusWorkspace(g, now)
	if err != nil {
		return err
	}
	return printStatus(ws, wide, now, g)
}

func loadStatusWorkspace(g *globals, now time.Time) (workspace, error) {
	var ws workspace
	err := withProgress(g, statusProgressOptions(g), func(p *progress) error {
		var lerr error
		if ws, lerr = loadWorkspace(g, p, false, now); lerr != nil {
			return lerr
		}
		return ws.loadUsage(p, now)
	})
	if err != nil {
		return workspace{}, err
	}
	return ws, nil
}

// printStatus emits the report: JSON when the contract requests it, otherwise
// the table, legend and detail block.
func printStatus(ws workspace, wide bool, now time.Time, g *globals) error {
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
// One fit serves the renderer and the legend, so both describe the same grid.
func renderStatus(ws workspace, wide bool, now time.Time, current, colorMode string) {
	budget := widthBudget()
	cols := statusSchema(wide, ws.wts)
	fit := fitTable(cols, ws.wts, columnContext{now: now, ttl: ws.ttl}, budget)
	// One per-run seen set shared by the table grid and the detail block, so a
	// header hidden in both warns once (mirrors emitUsageWarning).
	seen := map[string]bool{}
	printStatusTable(fit, colorMode, seen)
	printCostLegend(fit.legend(), colorMode, budget)
	printStatusDetail(ws.wts, current, now, colorMode, budget, seen)
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
// before any renderer starts: a raw warning inside the frame is erased. As a
// package variable it is also the injection seam that pins the one-clock-per-run
// contract.
var nowFunc = func() time.Time {
	if t, ok := parseNowEnv(); ok {
		return t
	}
	return time.Now()
}

// nowEnv is the WTM_NOW clock seam. The container test suite pins it so
// worktree ages and the gc TTL boundary are byte-stable; production never sets
// it.
const nowEnv = "WTM_NOW"

// parseNowEnv parses the WTM_NOW seam. An absent or malformed value reports
// ok=false, so nowFunc falls back to the wall clock.
func parseNowEnv() (time.Time, bool) {
	return parseRFC3339(os.Getenv(nowEnv))
}

// warnNowEnv runs at dispatch, before any subcommand can open a renderer.
func warnNowEnv() {
	raw := os.Getenv(nowEnv)
	if raw == "" {
		return
	}
	if _, ok := parseNowEnv(); !ok {
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
	return parseRFC3339(raw)
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

// loadUsage attaches every worktree's cost/active-time summary. The harness
// check runs once: a Pi-only repo never reaches the network and never opens
// Claude Code's account file. An unreadable session degrades to a warning:
// status is read-only and must still report what it knows.
func (ws *workspace) loadUsage(p *progress, now time.Time) error {
	// One deepest-owner partition for the run: the harness checks and every
	// worker fold read it instead of re-scanning the session index per call.
	parts := partitionSessions(ws.wts, ws.idx)
	hasClaude := parts.hasHarness(claudeAgent.name)
	hasPi := parts.hasHarness(piAgent.name)
	subs := loadSubscriptions(hasClaude)
	// An unread account keeps list price; silence would overstate the bill.
	if subs.claudeErr != nil {
		warnProgress(p, "claude subscription unproven: %s; claude costs use list price", subs.claudeErr)
	}
	if subs.piErr != nil && hasPi {
		warnProgress(p, "pi subscription unproven: %s; pi costs use list price", subs.piErr)
	}
	warnParentScopeDisabled(subs, p)
	cat := ws.loadCatalog(p, hasClaude, now)
	if err := p.abort(); err != nil {
		return err
	}
	// The caller owns the report instant, so usage and every renderer share it.
	return ws.parseUsage(cat, subs, now, p, parts)
}

// loadCatalog resolves the price catalog, naming its source in the progress
// label so a network wait reads as work, not as a hang.
func (ws *workspace) loadCatalog(p *progress, hasClaude bool, now time.Time) catalog {
	if !hasClaude {
		return nil
	}
	p.phase("loading rate catalog", 0)
	cat, err := loadCatalogReporting(p, now)
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
	usage    *worktreeUsage
	err      error
	warnings []error
}

// parseUsage reads every worktree's sessions concurrently: each worktree owns
// disjoint session files, and the shared proof is read-only, so the folds stay
// independent. The label follows the worktree a worker picked up; results
// attach by index after the join, keeping the render order tied to git's order,
// not to completion order.
func (ws *workspace) parseUsage(cat catalog, subs subscriptions, now time.Time, p *progress, parts worktreePartitions) error {
	p.phase("parsing sessions", len(ws.wts))
	if len(ws.wts) == 0 {
		return nil
	}
	usages := mapParallel(len(ws.wts), scanWorkers, func(i int) usageResult {
		return ws.usageResultFor(i, cat, subs, now, p, parts)
	})
	return ws.emitUsageResults(usages, p)
}

// usageResultFor prices one worktree. A worker never writes stderr
// (parallel.go), so warnings travel in the result and the parent emits them
// after the join.
func (ws *workspace) usageResultFor(i int, cat catalog, subs subscriptions, now time.Time, p *progress, parts worktreePartitions) usageResult {
	if err := p.abort(); err != nil {
		return usageResult{err: err}
	}
	path := ws.wts[i].Path
	p.detail(displayPath(path))
	u, warnings, err := buildUsageSessionsReport(path, parts[canonical(path)], cat, subs, now)
	p.advance(1)
	// Keep a total loss so the loss reads as unknown, never as no usage.
	if u.hasEvents() || u.partial {
		return usageResult{usage: &u, err: err, warnings: warnings}
	}
	return usageResult{err: err, warnings: warnings}
}

// emitUsageResults attaches every worker's usage and emits its warnings in
// worktree order: results attach by index, so the render order follows git's
// order, not completion order.
func (ws *workspace) emitUsageResults(usages []usageResult, p *progress) error {
	seen := map[string]bool{}
	for i, r := range usages {
		if err := p.abort(); err != nil {
			return err
		}
		ws.wts[i].Usage = r.usage
		if r.err != nil {
			warnProgress(p, "usage for %s: %s", displayPath(ws.wts[i].Path), r.err)
		}
		for _, warning := range r.warnings {
			emitUsageWarning(p, warning, seen)
		}
	}
	return nil
}

// emitUsageWarning discloses one worker warning once per run. The cause text is
// identical for every worktree that shares a settings file or repeats a corrupt
// record, so a repeat would only bury the first. `usage for <wt>` lines stay
// per-worktree because their text names the worktree.
func emitUsageWarning(p *progress, warning error, seen map[string]bool) {
	if seen[warning.Error()] {
		return
	}
	seen[warning.Error()] = true
	warnProgress(p, "%s", warning)
}

// cost is the worktree's price pair; empty when it has no events.
func (w worktree) cost() costPair {
	if w.Usage == nil {
		return costPair{}
	}
	return w.Usage.cost()
}
