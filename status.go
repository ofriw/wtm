package main

import (
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
	Main     bool
	Dirty    bool // uncommitted changes; surfaced for gc (picker label, forced-removal warning), never a filter
	LastUsed time.Time
	Config   bool
	DB       bool
	MCP      bool
}

func (w worktree) unused(now time.Time, ttl time.Duration) bool {
	return w.LastUsed.IsZero() || now.Sub(w.LastUsed) > ttl
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

func loadWorkspace(g *globals) (string, time.Duration, []worktree, error) {
	root, err := resolveRoot(g)
	if err != nil {
		return "", 0, nil, err
	}
	ttl, err := unusedDuration()
	if err != nil {
		return "", 0, nil, err
	}
	wts, err := discover(root)
	return root, ttl, wts, err
}

func buildWorktree(g gworktree, idx sessionIndex) worktree {
	p := canonical(g.Path)
	return worktree{
		Path:   p,
		Branch: g.Branch,
		Main:   g.Main,
		// Dirty is no longer pre-computed: status never shows it and the gc
		// picker is the only consumer (set lazily in gcSelection's candidate loop).
		LastUsed: worktreeLastUsed(p, idx),
		Config:   exists(filepath.Join(p, chunkhoundConfigFile)),
		DB:       exists(chunkhoundDBPath(p)),
		MCP:      exists(filepath.Join(p, piMCPFile)),
	}
}

// discover inventories every worktree of the repo rooted at root.
func discover(root string) ([]worktree, error) {
	raw, err := worktrees(root)
	if err != nil {
		return nil, err
	}
	// One session walk for the whole inventory.
	idx, err := indexPiSessions()
	if err != nil {
		return nil, err
	}
	wts := make([]worktree, 0, len(raw))
	for _, g := range raw {
		wts = append(wts, buildWorktree(g, idx))
	}
	sortWorktrees(wts)
	return wts, nil
}

func updateNewest(target *time.Time, path string) {
	if info, err := os.Stat(path); err == nil && info.ModTime().After(*target) {
		*target = info.ModTime()
	}
}

func scanGitModTimes(path string, newest *time.Time) {
	if out, err := git(path, "log", "-1", "--format=%ct"); err == nil {
		if s, err := strconv.ParseInt(out, 10, 64); err == nil {
			if commit := time.Unix(s, 0); commit.After(*newest) {
				*newest = commit
			}
		}
	}
	if out, err := git(path, "ls-files", "-m", "-o", "--exclude-standard", "-z"); err == nil {
		for _, rel := range strings.Split(out, "\x00") {
			if rel != "" && !strings.HasPrefix(rel, ".chunkhound") {
				updateNewest(newest, filepath.Join(path, rel))
			}
		}
	}
}

// worktreeLastUsed includes creation and Git changes so a new or actively
// edited worktree cannot be classified as abandoned before its first Pi session.
func worktreeLastUsed(path string, idx sessionIndex) time.Time {
	now := time.Now()
	newest := lastUsed(path, idx)
	updateNewest(&newest, filepath.Join(path, ".git"))
	scanGitModTimes(path, &newest)
	if newest.After(now) {
		return now
	}
	return newest
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
	pos, err := parseSub(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return usagef("status: unexpected argument %q", pos[0])
	}
	_, ttl, wts, err := loadWorkspace(g)
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

// statusTableHeaders is the single source of truth for the status schema;
// tests and the right-align choice key off it.
var statusTableHeaders = []string{"PATH", "BRANCH", "LAST USED", "STATUS", "CONFIG", "DB", "MCP"}

// statusRightAlign right-aligns dates so their digits line up for scanning.
var statusRightAlign = []string{"LAST USED"}

func statusRows(wts []worktree, now time.Time, ttl time.Duration) [][]string {
	rows := make([][]string, 0, len(wts))
	for _, w := range wts {
		rows = append(rows, []string{
			displayPath(w.Path), w.branchName(), lastUsedCell(w), w.status(now, ttl),
			yesNo(w.Config), yesNo(w.DB), yesNo(w.MCP),
		})
	}
	return rows
}

func printStatusTable(wts []worktree, now time.Time, ttl time.Duration, colorMode string) {
	printTable(statusTableHeaders, statusRows(wts, now, ttl), colorMode, statusCellStyle, statusRightAlign)
}

type jsonWorktree struct {
	Path     string  `json:"path"`
	Branch   string  `json:"branch"`
	Main     bool    `json:"main"`
	LastUsed *string `json:"lastUsed"`
	Status   string  `json:"status"`
	Config   bool    `json:"config"`
	DB       bool    `json:"db"`
	MCP      bool    `json:"mcp"`
}

func printStatusJSON(wts []worktree, now time.Time, ttl time.Duration) error {
	out := make([]jsonWorktree, 0, len(wts))
	for _, w := range wts {
		j := jsonWorktree{
			// WHY: branchName() is the SSOT for detached display so JSON matches the table's "(detached)".
			Path: w.Path, Branch: w.branchName(), Main: w.Main, Status: w.status(now, ttl),
			Config: w.Config, DB: w.DB, MCP: w.MCP,
		}
		if !w.LastUsed.IsZero() {
			s := w.LastUsed.UTC().Format(time.RFC3339)
			j.LastUsed = &s
		}
		out = append(out, j)
	}
	return printJSON(out)
}
