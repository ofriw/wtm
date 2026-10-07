package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pi.go — the only file that knows the Pi.dev agent layout.

// Pi reads per-project MCP servers from two mutually-exclusive configs:
// pi-mcp-adapter reads `.mcp.json` (repo root); Pi's built-in MCP reads
// `.pi/mcp.json`, gated by project trust. Either may hold secrets, so wtm seeds
// both even when git ignores them.
const (
	piMCPFile       = ".mcp.json"
	piNativeMCPFile = ".pi/mcp.json"
)

// piMCPFiles is the SSOT for the MCP configs wtm recognizes: harness seeding and
// status presence both key off it. A func returning a fresh slice, so no
// caller can mutate shared state.
func piMCPFiles() []string { return []string{piMCPFile, piNativeMCPFile} }

// mcpConfigs inventories recognized configs once so merged and native status agree.
// An existing but unparseable config warns and counts as absent: status must
// never report a corrupt file as a healthy MCP setup. The warning goes through
// the progress handle because this runs during discovery, under the renderer.
func mcpConfigs(root string, p *progress) map[string]bool {
	configs := make(map[string]bool, 2)
	for _, rel := range piMCPFiles() {
		path := filepath.Join(root, rel)
		if !exists(path) {
			configs[rel] = false
			continue
		}
		var v any
		if err := readJSON(path, &v); err != nil {
			warnProgress(p, "%s is not valid JSON (%s); treating as absent", path, err)
			configs[rel] = false
			continue
		}
		configs[rel] = true
	}
	return configs
}

func anyMCPConfig(configs map[string]bool) bool {
	for _, present := range configs {
		if present {
			return true
		}
	}
	return false
}

// piAgentDir honors PI_CODING_AGENT_DIR so tests can sandbox the agent dir.
func piAgentDir() string {
	if d := os.Getenv("PI_CODING_AGENT_DIR"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent")
}

func copyIgnoredPiFile(srcRoot, dstRoot string, p string, d fs.DirEntry) error {
	if d.IsDir() {
		return nil
	}
	rel, err := filepath.Rel(srcRoot, p)
	if err != nil {
		return err
	}
	ignoreRoot := srcRoot
	if exists(filepath.Join(dstRoot, ".git")) {
		ignoreRoot = dstRoot
	}
	if !isIgnored(ignoreRoot, rel) || exists(filepath.Join(dstRoot, rel)) {
		return nil
	}
	return copyFile(p, filepath.Join(dstRoot, rel))
}

// copyPiHarness carries per-worktree Pi state that git deliberately does not:
// recognized MCP configs seed whenever the source has one and the checkout does
// not — an explicit exception to the ignore gate (pinned by
// add-pi-committed/add-native-mcp: agent wiring follows the worktree, ignored
// or not) — and remaining files under .pi/ are copied per-file only when git
// ignores them — tracked ones already arrived with the checkout,
// untracked-and-unignored ones are never touched.
func copyPiHarness(srcRoot, dstRoot string) error {
	for _, rel := range piMCPFiles() {
		src, dst := filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel)
		if exists(src) && !exists(dst) {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
	}
	return copyIgnoredPiTree(srcRoot, dstRoot)
}

// copyIgnoredPiTree copies the .pi tree per-file through the ignore gate,
// except the native MCP config: the explicit seed in copyPiHarness owns it,
// so the walk skips it and one rule owns every file.
func copyIgnoredPiTree(srcRoot, dstRoot string) error {
	piDir := filepath.Join(srcRoot, ".pi")
	if !exists(piDir) {
		return nil
	}
	native := filepath.Join(srcRoot, piNativeMCPFile)
	return filepath.WalkDir(piDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == native {
			return nil
		}
		return copyIgnoredPiFile(srcRoot, dstRoot, p, d)
	})
}

// sessionCWD reads the cwd from the first JSONL line. pi's session-format.md
// defines that header as the SSOT; wtm deliberately does not reimplement the
// encoded session-dir naming (`--Users-...--`).
// WHY bufio.Reader+cap, not Scanner: Scanner caps lines at 64 KiB and would
// silently drop long headers, hiding live sessions as UNUSED and risking
// wrongful GC. Oversize headers return "" (no match) rather than a guess.
const maxSessionHeader = 4 << 20 // 4 MiB cap on the first JSONL line

func sessionCWD(p string) string {
	cwd, _ := readSessionCWD(p)
	return cwd
}

func readSessionCWD(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, maxSessionHeader+1)).ReadString('\n')
	if err != nil && err != io.EOF {
		return "", err
	}
	if len(line) == 0 || len(line) > maxSessionHeader {
		return "", fmt.Errorf("invalid session header size: %s", p)
	}
	return decodeSessionCWD(p, line)
}

func decodeSessionCWD(p, line string) (string, error) {
	var h struct {
		CWD string `json:"cwd"`
	}
	if err := json.Unmarshal([]byte(line), &h); err != nil {
		return "", fmt.Errorf("session header %s: %w", p, err)
	}
	if h.CWD == "" {
		return "", fmt.Errorf("session header has no cwd: %s", p)
	}
	return h.CWD, nil
}

// sessionIndex maps a canonical worktree cwd to its Pi session files. It is
// built once per command via indexPiSessions so discovery costs a single walk
// of the agent dir instead of one per worktree.
type sessionIndex map[string][]string

// indexPiSessions walks the agent sessions dir once, grouping live session
// files by their header cwd (canonicalized via sessionCWD, the format SSOT).
// Files whose header yields no cwd are skipped. Per-key order follows os.ReadDir
// (sorted), so callers observe a stable sequence within a key. A missing
// sessions dir yields an empty index (no error), matching prior lenient behavior.
func indexPiSessions() (sessionIndex, error) {
	base := filepath.Join(piAgentDir(), "sessions")
	dirs, err := os.ReadDir(base)
	if err != nil {
		if os.IsNotExist(err) {
			return sessionIndex{}, nil
		}
		return nil, err
	}
	idx := sessionIndex{}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(base, d.Name()))
		if err != nil {
			continue
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			p := filepath.Join(base, d.Name(), f.Name())
			cwd := sessionCWD(p)
			if cwd == "" {
				continue
			}
			addSessionPath(idx, cwd, p)
		}
	}
	return idx, nil
}

func sessionLastUsed(wtPath string, idx sessionIndex, strict bool) (time.Time, error) {
	var newest time.Time
	for _, p := range idx[canonical(wtPath)] {
		if err := updateNewest(&newest, p); err != nil && strict {
			return newest, err
		}
	}
	return newest, nil
}

// Removal must fail closed: unreadable sessions can hide recent activity.
// Unattributable files do not abort the walk; they are collected as
// unverified so each removal candidate is judged against only the files
// that could be its own hidden activity. Structural failures (a sessions
// path that is not a directory, walk callback errors) stay globally fatal.
func indexPiSessionsStrict() (sessionIndex, []unverifiedSession, error) {
	base := filepath.Join(piAgentDir(), "sessions")
	if err := verifySessionsDir(base); err != nil {
		if os.IsNotExist(err) {
			return sessionIndex{}, nil, nil
		}
		return nil, nil, err
	}
	idx := sessionIndex{}
	var unverified []unverifiedSession
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return indexStrictSession(idx, &unverified, p, d)
	})
	return idx, unverified, err
}

func verifySessionsDir(base string) error {
	info, err := os.Lstat(base)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("pi sessions path is not a directory: %s", base)
	}
	return nil
}

// unverifiedSession is activity that cannot be attributed to a worktree.
// ModTime follows file symlinks; directory symlinks have unknown age because
// appending a contained session does not update the directory mtime.
// Zero means unknown age and must fail closed.
type unverifiedSession struct {
	Path    string
	ModTime time.Time
}

// blocksRemoval reports whether this file could be the candidate's hidden
// recent activity: only activity newer than everything already known about
// the candidate can flip its verdict. Unknown age fails closed.
func (u unverifiedSession) blocksRemoval(knownNewest time.Time) bool {
	if u.ModTime.IsZero() {
		return true
	}
	return u.ModTime.After(knownNewest)
}

func indexStrictSession(idx sessionIndex, unverified *[]unverifiedSession, p string, d fs.DirEntry) error {
	if d.Type()&os.ModeSymlink != 0 {
		*unverified = append(*unverified, unverifiedSession{Path: p, ModTime: statFollowMtime(p)})
		return nil
	}
	if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
		return nil
	}
	cwd, err := readSessionCWD(p)
	// A relative cwd cannot be attributed: canonical() anchors it to wtm's own
	// cwd, so the session lands on a key that is never this worktree while
	// counting as verified. Unattributable must fail closed, not silently pass.
	if err != nil || !filepath.IsAbs(cwd) {
		*unverified = append(*unverified, unverifiedSession{Path: p, ModTime: statFollowMtime(p)})
		return nil
	}
	addSessionPath(idx, cwd, p)
	return nil
}

func statFollowMtime(p string) time.Time {
	if info, err := os.Stat(p); err == nil && !info.IsDir() {
		return info.ModTime()
	}
	return time.Time{}
}

func addSessionPath(idx sessionIndex, cwd, p string) {
	key := canonical(cwd)
	idx[key] = append(idx[key], p)
}

// purgePiSessions deletes this worktree's session files and the session dir
// once empty, reading files from idx (built once for all gc removals). GC
// purges by default; --keep-sessions opts out.
func purgePiSessions(wtPath string, idx sessionIndex) (int, error) {
	n := 0
	for _, p := range idx[canonical(wtPath)] {
		if err := os.Remove(p); err != nil {
			return n, err
		}
		n++
		if rest, err := os.ReadDir(filepath.Dir(p)); err == nil && len(rest) == 0 {
			_ = os.Remove(filepath.Dir(p))
		}
	}
	return n, nil
}
