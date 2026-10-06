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

// agents.go — the agent-harness registry plus every operation shared by all
// harnesses (Pi.dev, Claude Code). One descriptor per harness; seeding,
// indexing and session purge never duplicate per-harness logic.

// projectMCPFile is the repo-root project MCP config. Pi's pi-mcp-adapter and
// Claude Code both read it, so it is a shared constant, not a per-harness one.
const projectMCPFile = ".mcp.json"

// agent describes one harness's per-worktree and per-user layout.
type agent struct {
	name          string
	statusToken   string   // grouped-cell token; a prefix of name, guarded by TestCapabilityTokens
	statusFiles   []string // repo-relative paths whose presence means the harness is wired
	mcpFiles      []string // MCP configs seeded even when git-ignored
	seedFiles     []string // repo-relative files seeded only when git-ignored
	seedDir       string   // tree walked per-file through the ignore gate
	seedSkip      []string // repo-relative paths inside seedDir never copied
	configDirEnv  string
	configDirRel  []string // under home
	sessionSubdir string
	sessionOwner  func(*bufio.Reader) (string, error)
}

// agents is the SSOT for known harnesses: seeding, indexing and MCP status
// all iterate it.
func agents() []agent { return []agent{piAgent, claudeAgent} }

// capability is the agent's integration descriptor: registering the agent in
// agents() adds its cell token, --wide column and JSON key automatically.
func (a agent) capability() capability {
	return capability{
		id: a.name, token: a.statusToken,
		probe: func(in capInput) capState { return binary(a.wired(in)) },
	}
}

// wired reports whether the harness is set up for a worktree: any statusFiles
// path is present, either as a validated MCP config or as a plain file. MCP
// configs are validated because a corrupt one must not read healthy; a harness
// settings file is only a presence signal, so plain existence is enough.
func (a agent) wired(in capInput) bool {
	for _, rel := range a.statusFiles {
		present, recognized := in.mcp[rel]
		if present || (!recognized && exists(filepath.Join(in.root, rel))) {
			return true
		}
	}
	return false
}

// configDir resolves the harness state dir: the env override when set, else
// home joined with configDirRel. An unset HOME yields "".
func (a agent) configDir() string {
	if d := os.Getenv(a.configDirEnv); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(append([]string{home}, a.configDirRel...)...)
}

// mcpConfigFiles is the deduped union of every agent's MCP configs. Both
// harnesses read the repo-root project file, so seeding must see it once.
func mcpConfigFiles() []string {
	seen := map[string]bool{}
	var files []string
	for _, a := range agents() {
		for _, rel := range a.mcpFiles {
			if !seen[rel] {
				seen[rel] = true
				files = append(files, rel)
			}
		}
	}
	return files
}

// mcpConfigs inventories recognized configs once so merged and native status
// agree. An existing but unparseable config warns and counts as absent: status
// must never report a corrupt file as a healthy MCP setup. The warning goes
// through the progress handle because this runs during discovery, under the
// renderer.
func mcpConfigs(root string, p *progress) map[string]bool {
	files := mcpConfigFiles()
	configs := make(map[string]bool, len(files))
	for _, rel := range files {
		configs[rel] = validMCPConfig(filepath.Join(root, rel), p)
	}
	return configs
}

// anyMCPConfig reports whether any recognized MCP config is present and valid:
// the SSOT for "this worktree has MCP support of any kind".
func anyMCPConfig(configs map[string]bool) bool {
	for _, present := range configs {
		if present {
			return true
		}
	}
	return false
}

// validMCPConfig reports whether path holds parseable JSON, warning through p
// when it exists but cannot be read as JSON.
func validMCPConfig(path string, p *progress) bool {
	if !exists(path) {
		return false
	}
	var v any
	if err := readJSON(path, &v); err != nil {
		warnProgress(p, "%s is not valid JSON (%s); treating as absent", path, err)
		return false
	}
	return true
}

// copyHarness carries every harness's per-worktree state that git deliberately
// does not. Recognized MCP configs seed whenever the source has one and the
// checkout does not — an explicit exception to the ignore gate (agent wiring
// follows the worktree, ignored or not). Each harness's remaining seed
// files/dirs copy per-file only when git ignores them: tracked ones already
// arrived with the checkout, untracked-and-unignored ones are never touched.
func copyHarness(srcRoot, dstRoot string) error {
	if err := seedMCPConfigs(srcRoot, dstRoot); err != nil {
		return err
	}
	for _, a := range agents() {
		if err := a.copySeed(srcRoot, dstRoot); err != nil {
			return fmt.Errorf("%s harness: %w", a.name, err)
		}
	}
	return nil
}

func seedMCPConfigs(srcRoot, dstRoot string) error {
	for _, rel := range mcpConfigFiles() {
		src, dst := filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel)
		if exists(src) && !exists(dst) {
			if err := copyFile(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a agent) copySeed(srcRoot, dstRoot string) error {
	for _, rel := range a.seedFiles {
		if !exists(filepath.Join(srcRoot, rel)) {
			continue
		}
		if err := copyIgnoredFile(srcRoot, dstRoot, rel); err != nil {
			return err
		}
	}
	return a.copySeedDir(srcRoot, dstRoot)
}

// copySeedDir copies the harness tree per-file through the ignore gate. Paths
// owned by an MCP config seed and every seedSkip path are skipped, so exactly
// one rule owns each seeded file.
func (a agent) copySeedDir(srcRoot, dstRoot string) error {
	if a.seedDir == "" {
		return nil
	}
	dir := filepath.Join(srcRoot, a.seedDir)
	if !exists(dir) {
		return nil
	}
	skip := seedSkipPaths(srcRoot, a)
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return copySeedEntry(srcRoot, dstRoot, skip, p, d)
	})
}

// copySeedEntry is one seed-walk step: skip paths owned by an MCP seed or by
// seedSkip, and copy ignored regular files.
func copySeedEntry(srcRoot, dstRoot string, skip map[string]bool, p string, d fs.DirEntry) error {
	if skip[p] {
		if d.IsDir() {
			return filepath.SkipDir
		}
		return nil
	}
	if d.IsDir() {
		return nil
	}
	rel, rerr := filepath.Rel(srcRoot, p)
	if rerr != nil {
		return rerr
	}
	return copyIgnoredFile(srcRoot, dstRoot, rel)
}

// seedSkipPaths is the set of absolute source paths the seed walk never
// copies: the harness's own MCP configs (the explicit seed owns them) plus
// every seedSkip path.
func seedSkipPaths(srcRoot string, a agent) map[string]bool {
	skip := make(map[string]bool, len(a.mcpFiles)+len(a.seedSkip))
	for _, rel := range a.mcpFiles {
		skip[filepath.Join(srcRoot, rel)] = true
	}
	for _, rel := range a.seedSkip {
		skip[filepath.Join(srcRoot, rel)] = true
	}
	return skip
}

// copyIgnoredFile copies one repo-relative regular file through the ignore
// gate: only when git ignores it and the checkout lacks it. The ignore check
// runs in the checkout when it has its own .git, so its rules win.
func copyIgnoredFile(srcRoot, dstRoot, rel string) error {
	if exists(filepath.Join(dstRoot, rel)) {
		return nil
	}
	ignoreRoot := srcRoot
	if exists(filepath.Join(dstRoot, ".git")) {
		ignoreRoot = dstRoot
	}
	if !isIgnored(ignoreRoot, rel) {
		return nil
	}
	return copyFile(filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel))
}

// Ownership comes from harness records, never lossy encoded directory names.
// Bound each record's memory; oversized records have unknown ownership.
const maxSessionHeader = 4 << 20

// maxSessionOwnerRecords bounds the cwd scan: a cwd-less giant JSONL must
// not force a full scan per status. The owner arrives in the first records
// (queue metadata precedes it), so anything past the bound is unknown.
const maxSessionOwnerRecords = 64

func (a agent) readSessionOwner(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	return a.sessionOwner(bufio.NewReader(f))
}

func piSessionOwner(r *bufio.Reader) (string, error) {
	line, err := sessionRecord(r)
	if err == io.EOF {
		err = nil
	}
	return recordCWD(line), err
}

// Claude can write queue metadata before the first record with a cwd.
func claudeSessionOwner(r *bufio.Reader) (string, error) {
	for n := 0; n < maxSessionOwnerRecords; n++ {
		line, err := sessionRecord(r)
		if cwd := recordCWD(line); cwd != "" {
			return cwd, nil
		}
		if err == io.EOF {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	// WHY: past the bound the owner is unknown, not absent — the caller
	// skips unknown owners, so unrelated history survives.
	return "", nil
}

func recordCWD(line []byte) string {
	var h struct {
		CWD string `json:"cwd"`
	}
	if json.Unmarshal(line, &h) != nil {
		return ""
	}
	return h.CWD
}

// Drain oversized lines without retaining them, then resume at the next record.
func sessionRecord(r *bufio.Reader) ([]byte, error) {
	var line []byte
	oversized := false
	for {
		part, err := r.ReadSlice('\n')
		if len(line)+len(part) > maxSessionHeader {
			oversized, line = true, nil
		}
		if !oversized {
			line = append(line, part...)
		}
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// sessionIndex maps a canonical worktree cwd to its session files. It is built
// once per command via indexSessions so discovery costs one walk per harness
// instead of one per worktree.
type sessionIndex map[string][]sessionRef

// sessionRef carries an owned file and the boundary for empty-dir cleanup.
type sessionRef struct {
	path  string
	group string
}

// sessionErrPolicy decides what an unreadable session does. Reclamation fails
// closed: an incomplete index can misclassify a worktree UNUSED and GC it.
// Read-only discovery warns and reports what it knows.
type sessionErrPolicy struct {
	failClosed bool
	p          *progress
}

// sessionErr reports one unreadable session: fatal when failClosed, else a
// warning through the progress handle (plain stderr when it is nil).
func (pol sessionErrPolicy) sessionErr(err error) error {
	if pol.failClosed {
		return err
	}
	warnProgress(pol.p, "%v", err)
	return nil
}

// walkErr classifies a walk failure. A root listing failure is always fatal:
// it hides every worktree's activity. A child directory failure carries no
// attributable evidence (another user's project on a shared host) and is
// skipped; a file failure goes through sessionErr.
// LIMIT: a blocked subdir of your own root is skipped too. The wrongful-GC
// defense rests on the fatal root and fatal file rules, not on this skip.
func (pol sessionErrPolicy) walkErr(p string, d fs.DirEntry, err error) error {
	// The walk root is already handled by the caller; a non-root Lstat failure
	// still produces a non-nil d for the parent dir, so keep the dir semantics.
	if d != nil && d.IsDir() {
		return nil
	}
	return pol.sessionErr(fmt.Errorf("walk sessions: %s: %w", p, err))
}

// indexSessions walks every given harness's session root once, grouping live
// session files by their harness record cwd. Unknown owners are skipped. What an
// unreadable session does is the caller's call, via failClosed: reclamation must
// not act on an incomplete index, discovery may. Per-key order follows the walk
// (sorted), so callers observe a stable sequence within a key.
func indexSessions(ags []agent, pol sessionErrPolicy) (sessionIndex, error) {
	idx := sessionIndex{}
	for _, a := range ags {
		if err := a.indexSessions(idx, pol); err != nil {
			return nil, err
		}
	}
	return idx, nil
}

// indexSessions adds one harness's sessions to idx, skipping a missing root.
func (a agent) indexSessions(idx sessionIndex, pol sessionErrPolicy) error {
	if a.configDir() == "" {
		return nil
	}
	base, err := sessionRoot(filepath.Join(a.configDir(), a.sessionSubdir))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// A directory listing failure on the walk root means this harness
			// is unreadable (shared host, odd permissions). An empty index here
			// misclassifies every worktree as UNUSED and risks a wrongful GC, so
			// the error is fatal in every policy, not just reclamation.
			if p == base && d != nil && d.IsDir() {
				return fmt.Errorf("read sessions %s: %w", p, err)
			}
			return pol.walkErr(p, d, err)
		}
		return a.addSessionRef(idx, base, p, d, pol)
	})
}

// Resolve only a symlinked root: dotfile setups use these, while resolving
// ancestors would rewrite paths on systems such as macOS (/var -> /private/var).
func sessionRoot(base string) (string, error) {
	info, err := os.Lstat(base)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return filepath.EvalSymlinks(base)
	}
	return base, nil
}

func (a agent) addSessionRef(idx sessionIndex, base, p string, d fs.DirEntry, pol sessionErrPolicy) error {
	if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
		return nil
	}
	cwd, err := a.readSessionOwner(p)
	if err != nil {
		// A dangling symlink or a file removed mid-walk holds no sessions.
		if os.IsNotExist(err) {
			return nil
		}
		// WHY: an unreadable *.jsonl is provably a session wtm cannot
		// attribute, so it must never silently hide activity from GC.
		return pol.sessionErr(fmt.Errorf("read session %s: %w", p, err))
	}
	if cwd != "" {
		key := canonical(cwd)
		idx[key] = append(idx[key], sessionRef{path: p, group: sessionGroup(base, p)})
	}
	return nil
}

// sessionGroup bounds empty-dir cleanup without decoding harness path names.
func sessionGroup(base, p string) string {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(rel, string(os.PathSeparator))
	return filepath.Join(base, first)
}

// lastUsed is the newest mtime among the worktree's session files (looked up
// in idx); zero when the worktree has never hosted a session.
func lastUsed(wtPath string, idx sessionIndex) time.Time {
	var newest time.Time
	for _, r := range idx[canonical(wtPath)] {
		if info, err := os.Stat(r.path); err == nil && info.ModTime().After(newest) {
			newest = info.ModTime()
		}
	}
	return newest
}

// purgeSessions deletes only confirmed owned files. Unknown and unrelated
// history must survive lossy directory collisions; remove directories only
// when empty. GC purges by default; --keep-sessions opts out.
func purgeSessions(wtPath string, idx sessionIndex) (int, error) {
	n := 0
	for _, r := range idx[canonical(wtPath)] {
		if err := os.Remove(r.path); err != nil {
			return n, err
		}
		n++
		if err := removeEmptySessionDirs(r); err != nil {
			return n, err
		}
	}
	return n, nil
}

func removeEmptySessionDirs(r sessionRef) error {
	// An empty group would make filepath.Dir("") resolve to "." and walk the
	// cleanup above the session root.
	if r.group == "" {
		return nil
	}
	for dir := filepath.Dir(r.path); dir != filepath.Dir(r.group); dir = filepath.Dir(dir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			// WHY: a concurrent purge already removed the dir — cleanup is
			// done, not failed.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if len(entries) != 0 {
			return nil
		}
		if err := os.Remove(dir); err != nil {
			return err
		}
	}
	return nil
}
