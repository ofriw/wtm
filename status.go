package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// worktree is the shared model: one git worktree plus its harness inventory.
type worktree struct {
	Path     string
	Branch   string
	Upstream upstream // configured tracking branch; zero value = none
	Main     bool
	Dirty    bool // uncommitted changes; surfaced for gc (picker label, forced-removal warning), never a filter
	LastUsed time.Time

	// Temp marks an ephemeral checkout and carries the window that reclaims it.
	Temp         bool
	TempTTL      time.Duration
	TempCreated  time.Time
	TempIdentity string

	Config    bool
	DB        bool
	MCP       bool
	MCPNative bool
}

// unused reports whether gc may reclaim a worktree: a temp one past its own
// idle window, a permanent one past the global unusedTTL. It is the single
// candidate predicate for status and gc.
func (w worktree) unused(now time.Time, ttl time.Duration) bool {
	if w.Temp {
		return tempExpired(w.TempCreated, w.LastUsed, w.TempTTL, now)
	}
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
}

// tempExpiresAt is the deadline a temp worktree is measured against.
// tempDeadline owns the formula. Zero for a permanent worktree.
func (w worktree) tempExpiresAt() time.Time {
	if !w.Temp {
		return time.Time{}
	}
	return tempDeadline(w.TempCreated, w.LastUsed, w.TempTTL)
}

func (w worktree) status(now time.Time, ttl time.Duration) string {
	if w.unused(now, ttl) {
		return "UNUSED"
	}
	return "ACTIVE"
}

func (w worktree) branchName() string {
	if w.Branch == "" {
		return "(detached)"
	}
	return w.Branch
}

// resolveRoot returns the repo root: --root if given, else the main worktree
// of the repo containing the current directory.
func resolveRoot(g *globals) (string, error) {
	if g.root != "" {
		return canonicalAbs(g.root)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	root, err := repoRoot(cwd)
	if err != nil {
		return "", fmt.Errorf("no repository here; use --root: %w", err)
	}
	return canonical(root), nil
}

// loadWorkspace resolves the repo, TTL and inventory. discovery — the slow
// path — uses the renderer when showProgress or --progress always requests it.
// Read-only: it never mutates the temp registry, so status and promote create
// no ~/.wtm state and take no registry lock. Only gc reconciles, and only gc
// needs records cleaned before candidates are chosen.
func loadWorkspace(g *globals, showProgress bool) (string, time.Duration, []worktree, error) {
	root, err := resolveRoot(g)
	if err != nil {
		return "", 0, nil, err
	}
	ttl, err := unusedDuration()
	if err != nil {
		return "", 0, nil, err
	}
	var wts []worktree
	err = withProgress(g, showProgress, func(p *progress) error {
		var derr error
		wts, derr = discover(root, p)
		return derr
	})
	return root, ttl, wts, err
}

func buildWorktree(g gworktree, idx sessionIndex, ups map[string]upstream, temps tempStore, p *progress) worktree {
	w := worktreeInventory(g, idx, ups, p)
	applyTempRecord(&w, temps, p)
	return w
}

func worktreeInventory(g gworktree, idx sessionIndex, ups map[string]upstream, p *progress) worktree {
	pth := canonical(g.Path)
	configs := mcpConfigs(pth, p)
	w := worktree{
		Path:      pth,
		Branch:    g.Branch,
		Upstream:  ups[g.Branch],
		Main:      g.Main,
		LastUsed:  worktreeLastUsed(pth, idx),
		Config:    exists(filepath.Join(pth, chunkhoundConfigFile)),
		DB:        exists(chunkhoundDBPath(pth)),
		MCP:       anyMCPConfig(configs),
		MCPNative: configs[piNativeMCPFile],
	}
	return w
}

// applyTempRecord marks w temporary when its record verifies. Every failure
// falls toward permanent, never toward reclaimable: a zero TTL would reclaim
// instantly, so an unparsable window is refused rather than defaulted.
func applyTempRecord(w *worktree, temps tempStore, p *progress) {
	rec, ok := temps[w.Path]
	if !ok {
		return
	}
	if err := verifyTempIdentity(w.Path, rec); err != nil {
		warnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	ttl, err := parseTTL(rec.TTL)
	if err != nil {
		warnProgress(p, "temp record for %s: %s; treating as permanent", displayPath(w.Path), err)
		return
	}
	w.Temp, w.TempTTL, w.TempCreated, w.TempIdentity = true, ttl, rec.CreatedAt, rec.Identity
}

// discover inventories every worktree of the repo rooted at root, reporting
// each slow step to p (a nil no-op when progress is off).
func discover(root string, p *progress) ([]worktree, error) {
	raw, idx, ups, temps, err := discoverInputs(root, p)
	if err != nil {
		return nil, err
	}
	wts := scanWorktrees(raw, idx, ups, temps, p)
	sortWorktrees(wts)
	return wts, nil
}

func discoverUpstreams(root string, p *progress) map[string]upstream {
	p.phase("reading upstreams", 0)
	ups, err := branchUpstreams(root)
	if err != nil {
		warnProgress(p, "upstreams unavailable: %s", err)
		return map[string]upstream{}
	}
	return ups
}

// discoverInputs walks the O(repo) sources discovery needs: the worktree list,
// one session index for the whole inventory, one upstream map for every branch,
// and the global temp store.
func discoverInputs(root string, p *progress) ([]gworktree, sessionIndex, map[string]upstream, tempStore, error) {
	p.phase("reading worktrees", 0)
	raw, err := worktrees(root)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	p.phase("indexing sessions", 0)
	idx, err := indexPiSessions()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	return raw, idx, discoverUpstreams(root, p), readTempStoreLenient(p), nil
}

// readTempStoreLenient reads the temp store for read-only commands. A corrupt
// store only loses temp metadata (the checkouts stay durable), so it must never
// break status: warn and proceed with no temp records.
func readTempStoreLenient(p *progress) tempStore {
	temps, err := readTempStore()
	if err != nil {
		warnProgress(p, "temp store: %s; no temp worktrees tracked", err)
		return tempStore{}
	}
	return temps
}

// scanWorktrees builds the inventory and reports per-worktree progress; the
// loop is split out so discover stays a readable sequence of phases.
func scanWorktrees(raw []gworktree, idx sessionIndex, ups map[string]upstream, temps tempStore, p *progress) []worktree {
	p.phase("scanning worktrees", len(raw))
	wts := make([]worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx, ups, temps, p))
		p.advance(1)
		p.detail(displayPath(g.Path))
	}
	return wts
}

func updateNewest(target *time.Time, path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("activity stat %s: %w", path, err)
	}
	if info.ModTime().After(*target) {
		*target = info.ModTime()
	}
	return nil
}

func scanGitModTimes(path string, newest *time.Time, strict bool) error {
	if err := scanIndexTime(path, newest); err != nil && strict {
		return err
	}
	if err := scanCommitTime(path, newest); err != nil && strict {
		return err
	}
	return scanChangedFileTimes(path, newest, strict)
}

func scanCommitTime(path string, newest *time.Time) error {
	out, err := git(path, "log", "-1", "--format=%ct")
	if err != nil {
		return err
	}
	s, err := strconv.ParseInt(out, 10, 64)
	if err != nil {
		return fmt.Errorf("parse git commit time: %w", err)
	}
	if commit := time.Unix(s, 0); commit.After(*newest) {
		*newest = commit
	}
	return nil
}

// changedActivityFiles includes staged files but not staged deletions: their
// activity survives in the private index, while missing unstaged files fail closed.
func changedActivityFiles(path string) (string, error) {
	out, err := git(path, "--no-optional-locks", "ls-files", "-m", "-o", "--exclude-standard", "-z")
	if err != nil {
		return "", err
	}
	staged, err := git(path, "--no-optional-locks", "diff", "--cached", "--name-only", "--diff-filter=d", "-z")
	return out + "\x00" + staged, err
}

func scanChangedFileTimes(path string, newest *time.Time, strict bool) error {
	out, err := changedActivityFiles(path)
	if err != nil {
		return err
	}
	for _, rel := range strings.Split(out, "\x00") {
		if rel != "" && !strings.HasPrefix(rel, ".chunkhound") {
			if err := updateNewest(newest, filepath.Join(path, rel)); err != nil && strict {
				// A vanished changed file cannot be checked for activity: fail
				// closed and say how the user resolves the resulting deadlock.
				// errors.Is, not os.IsNotExist: updateNewest wraps with %w.
				if errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("%w; commit or restore the deleted file, or remove the checkout explicitly", err)
				}
				return err
			}
		}
	}
	return nil
}

// freshWorktreeActivity refreshes both sources under a fresh session index
// at now; discovery's cached index cannot authorize removal. Errors are fatal:
// removal must fail closed when activity is unreadable. Unverified sessions
// ride along so the caller can scope them per candidate instead of failing
// globally.
func freshWorktreeActivity(path string, now time.Time) (time.Time, []unverifiedSession, error) {
	idx, unverified, err := indexPiSessionsStrict()
	if err != nil {
		return time.Time{}, nil, fmt.Errorf("refresh pi sessions: %w", err)
	}
	last, err := readWorktreeActivityStrict(path, idx, now)
	if err != nil {
		return time.Time{}, nil, err
	}
	return last, unverified, nil
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first Pi
// session. It pins activity to now for display; errors are ignored because
// display cannot fail closed.
func worktreeLastUsed(path string, idx sessionIndex) time.Time {
	newest, _ := readWorktreeActivityLenient(path, idx, time.Now())
	return newest
}

// readWorktreeActivityLenient collects activity timestamps relative to now,
// ignoring errors. Used for display where failure must not block output.
func readWorktreeActivityLenient(path string, idx sessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, false)
}

// readWorktreeActivityStrict collects activity timestamps relative to now,
// failing on any error. Used immediately before idle-based removal where
// unreadable activity must prevent reclamation.
func readWorktreeActivityStrict(path string, idx sessionIndex, now time.Time) (time.Time, error) {
	return collectActivity(path, idx, now, true)
}

// collectActivity merges Pi sessions, the checkout backlink and Git
// modification times, clamping any future timestamp to now.
func collectActivity(path string, idx sessionIndex, now time.Time, strict bool) (time.Time, error) {
	newest, err := sessionLastUsed(path, idx, strict)
	if err != nil {
		return newest, err
	}
	if err := updateNewest(&newest, filepath.Join(path, ".git")); err != nil && strict {
		return newest, err
	}
	if err := scanGitModTimes(path, &newest, strict); err != nil && strict {
		return newest, err
	}
	if newest.After(now) {
		return now, nil
	}
	return newest, nil
}

// The private index records staging, including deletion with no remaining
// file. Read-only Git queries must not refresh it and restart the idle window.
func scanIndexTime(path string, newest *time.Time) error {
	dir, err := gitPrivateDir(path)
	if err != nil {
		return err
	}
	return updateNewest(newest, filepath.Join(dir, "index"))
}

// sortWorktrees orders main first, then most recently used, then path — a
// total order, so machine output never depends on git's listing order.
func sortWorktrees(wts []worktree) {
	sort.Slice(wts, func(i, j int) bool {
		a, b := wts[i], wts[j]
		if a.Main != b.Main {
			return a.Main
		}
		if !a.LastUsed.Equal(b.LastUsed) {
			return a.LastUsed.After(b.LastUsed)
		}
		return a.Path < b.Path
	})
}

func cmdStatus(g *globals, args []string) error {
	fs := subFlags("status", g)
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("status: unexpected argument %q", pos[0])
	}
	_, ttl, wts, err := loadWorkspace(g, false)
	if err != nil {
		return err
	}
	now := time.Now()
	if g.json {
		return printStatusJSON(wts, now, ttl)
	}
	printStatusTable(wts, now, ttl, g.color)
	return nil
}

func lastUsedCell(w worktree) string {
	if w.LastUsed.IsZero() {
		return "never"
	}
	return w.LastUsed.Format("2006-01-02")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// upstreamCell is the status display for a branch's upstream; "-" when the
// branch has none, so whether gc will delete a remote branch is obvious.
func upstreamCell(w worktree) string {
	if !w.Upstream.present() {
		return "-"
	}
	return w.Upstream.Short
}

// dirtyCell is the gc picker's uncommitted-change signal. "-" keeps the column
// scannable and mirrors the inventory columns' neutral value.
func dirtyCell(w worktree) string {
	if w.Dirty {
		return "dirty"
	}
	return "-"
}

// displayPath shortens the home directory to ~ so the PATH column budgets for
// the path a human recognizes, not for an invariant absolute prefix.
func displayPath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if strings.HasPrefix(p, home+string(os.PathSeparator)) {
		return "~" + p[len(home):]
	}
	return p
}

// columnContext carries the volatile inputs a cell may need, so cell funcs
// stay pure and the registry stays the single source of truth.
type columnContext struct {
	now time.Time
	ttl time.Duration
}

// worktreeColumn is one grid column: its header, its projection from a
// worktree, and its fit policy. shrinkRank is the elision order (0 = pinned);
// floor is the smallest width a shrinkable column may be elided to. This
// registry is the single source of truth for the status table and the gc
// picker, so the two can never drift apart.
type worktreeColumn struct {
	header     string
	cell       func(worktree, columnContext) string
	rightAlign bool
	shrinkRank int
	floor      int
}

// cell funcs wrap the display formatters so the registry stays declarative.
func cellPath(w worktree, _ columnContext) string     { return displayPath(w.Path) }
func cellBranch(w worktree, _ columnContext) string   { return w.branchName() }
func cellUpstream(w worktree, _ columnContext) string { return upstreamCell(w) }
func cellLastUsed(w worktree, _ columnContext) string { return lastUsedCell(w) }
func cellStatus(w worktree, c columnContext) string   { return w.status(c.now, c.ttl) }

// cellTemp is the shared window cell: the remaining idle window for a temp
// worktree, "expired" once it has passed, "-" for a permanent one.
func cellTemp(w worktree, c columnContext) string {
	if !w.Temp {
		return "-"
	}
	return remainingWindowLabel(w.tempExpiresAt(), c.now)
}
func cellConfig(w worktree, _ columnContext) string      { return yesNo(w.Config) }
func cellDB(w worktree, _ columnContext) string          { return yesNo(w.DB) }
func cellMCP(w worktree, _ columnContext) string         { return yesNo(w.MCP) }
func cellUncommitted(w worktree, _ columnContext) string { return dirtyCell(w) }

var (
	colPath        = worktreeColumn{header: "PATH", cell: cellPath, shrinkRank: 1, floor: 12}
	colBranch      = worktreeColumn{header: "BRANCH", cell: cellBranch, shrinkRank: 3, floor: 8}
	colUpstream    = worktreeColumn{header: "UPSTREAM", cell: cellUpstream, shrinkRank: 2, floor: 8}
	colLastUsed    = worktreeColumn{header: "LAST USED", cell: cellLastUsed, rightAlign: true, shrinkRank: 4, floor: 10}
	colStatus      = worktreeColumn{header: "STATUS", cell: cellStatus}
	colTemp        = worktreeColumn{header: "TEMP", cell: cellTemp}
	colConfig      = worktreeColumn{header: "CONFIG", cell: cellConfig}
	colDB          = worktreeColumn{header: "DB", cell: cellDB}
	colMCP         = worktreeColumn{header: "MCP", cell: cellMCP}
	colUncommitted = worktreeColumn{header: "UNCOMMITTED", cell: cellUncommitted}
)

// statusColumns is the full status schema, always present so positional
// parsers never see a different column set.
var statusColumns = []worktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colStatus, colTemp, colConfig, colDB, colMCP}

// allColumns is every defined column; the shared fit policy is derived from it
// so a column's rank/floor cannot drift from the tables that use it.
var allColumns = []worktreeColumn{colPath, colBranch, colUpstream, colLastUsed, colStatus, colTemp, colConfig, colDB, colMCP, colUncommitted}

var (
	// shrinkPriority orders the columns elided when a table must fit a budget;
	// earlier headers give up width first. Unranked columns are pinned.
	shrinkPriority = columnShrinkPriority(allColumns)
	// shrinkFloor is the smallest width a shrinkable column is elided to before
	// an impossibly tight budget forces it further down to one cell.
	shrinkFloor = columnShrinkFloor(allColumns)
)

func columnHeaders(cols []worktreeColumn) []string {
	headers := make([]string, len(cols))
	for i, c := range cols {
		headers[i] = c.header
	}
	return headers
}

func columnRightAligns(cols []worktreeColumn) []string {
	var right []string
	for _, c := range cols {
		if c.rightAlign {
			right = append(right, c.header)
		}
	}
	return right
}

func columnShrinkPriority(cols []worktreeColumn) []string {
	var shrinkable []worktreeColumn
	for _, c := range cols {
		if c.shrinkRank > 0 {
			shrinkable = append(shrinkable, c)
		}
	}
	sort.SliceStable(shrinkable, func(i, j int) bool { return shrinkable[i].shrinkRank < shrinkable[j].shrinkRank })
	return columnHeaders(shrinkable)
}

func columnShrinkFloor(cols []worktreeColumn) map[string]int {
	floors := make(map[string]int)
	for _, c := range cols {
		if c.shrinkRank > 0 {
			floors[c.header] = c.floor
		}
	}
	return floors
}

// gridRows projects worktrees through a column selection; it is the only place
// a table's cells are produced, so status and the gc picker share one schema.
func gridRows(cols []worktreeColumn, wts []worktree, ctx columnContext) [][]string {
	rows := make([][]string, 0, len(wts))
	for _, w := range wts {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = c.cell(w, ctx)
		}
		rows = append(rows, row)
	}
	return rows
}

func printStatusTable(wts []worktree, now time.Time, ttl time.Duration, colorMode string) {
	cols := statusColumns
	printTable(columnHeaders(cols), gridRows(cols, wts, columnContext{now: now, ttl: ttl}), colorMode, statusCellStyle, columnRightAligns(cols))
}

type jsonWorktree struct {
	Path      string  `json:"path"`
	Branch    string  `json:"branch"`
	Upstream  string  `json:"upstream"`
	Main      bool    `json:"main"`
	LastUsed  *string `json:"lastUsed"`
	Status    string  `json:"status"`
	Temp      bool    `json:"temp"`
	ExpiresAt *string `json:"expiresAt"`
	Config    bool    `json:"config"`
	DB        bool    `json:"db"`
	MCP       bool    `json:"mcp"`
	MCPNative bool    `json:"mcpNative"`
}

func printStatusJSON(wts []worktree, now time.Time, ttl time.Duration) error {
	out := make([]jsonWorktree, 0, len(wts))
	for _, w := range wts {
		j := jsonWorktree{
			// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
			Path: w.Path, Branch: w.branchName(), Upstream: w.Upstream.Short, Main: w.Main, Status: w.status(now, ttl),
			Temp: w.Temp, Config: w.Config, DB: w.DB, MCP: w.MCP, MCPNative: w.MCPNative,
		}
		if !w.LastUsed.IsZero() {
			s := w.LastUsed.UTC().Format(time.RFC3339)
			j.LastUsed = &s
		}
		if w.Temp {
			s := formatTempDeadline(w.tempExpiresAt())
			j.ExpiresAt = &s
		}
		out = append(out, j)
	}
	return printJSON(out)
}
