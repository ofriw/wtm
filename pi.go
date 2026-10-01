package main

import (
	"bufio"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// pi.go — the only file that knows the Pi.dev agent layout.

// piMCPFile is pi-mcp-adapter's per-project MCP config, read by Pi at startup.
const piMCPFile = ".mcp.json"

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
// .mcp.json is copied when the source has one and the checkout did not, and
// files under .pi/ are copied per-file only when git ignores them — tracked
// ones already arrived with the checkout, untracked-and-unignored ones are
// never touched.
func copyPiHarness(srcRoot, dstRoot string) error {
	if src := filepath.Join(srcRoot, piMCPFile); exists(src) {
		if dst := filepath.Join(dstRoot, piMCPFile); !exists(dst) {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
	}
	piDir := filepath.Join(srcRoot, ".pi")
	if !exists(piDir) {
		return nil
	}
	return filepath.WalkDir(piDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
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
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(f).ReadString('\n')
	if len(line) == 0 || len(line) > maxSessionHeader {
		return ""
	}
	_ = err // trailing data without newline (io.EOF) still parses
	var h struct {
		CWD string `json:"cwd"`
	}
	if json.Unmarshal([]byte(line), &h) != nil {
		return ""
	}
	return h.CWD
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
			key := canonical(cwd)
			idx[key] = append(idx[key], p)
		}
	}
	return idx, nil
}

// lastUsed is the newest mtime among the worktree's Pi session files (looked
// up in idx); zero when the worktree has never hosted a session.
func lastUsed(wtPath string, idx sessionIndex) time.Time {
	var newest time.Time
	for _, p := range idx[canonical(wtPath)] {
		if info, err := os.Stat(p); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
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
