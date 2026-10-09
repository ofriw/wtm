package main

import (
	"bufio"
	"encoding/json"
	"errors"
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
	path       string
	group      string
	root       string
	rootTarget string
	owner      func(*bufio.Reader) (string, error)
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

// unverifiedSession is unattributable activity. A zero age can hide any use;
// directory symlinks have zero age because appends do not change directory mtimes.
type unverifiedSession struct {
	Path    string
	ModTime time.Time
}

func (u unverifiedSession) blocksRemoval(knownNewest time.Time) bool {
	return u.ModTime.IsZero() || u.ModTime.After(knownNewest)
}

// sessionScan shares ownership and traversal rules for all harnesses. Fresh idle
// checks collect evidence for per-candidate decisions; ordinary discovery warns.
type sessionScan struct {
	idx        sessionIndex
	unverified []unverifiedSession
	pol        sessionErrPolicy
	collect    bool
}

func indexSessions(ags []agent, pol sessionErrPolicy) (sessionIndex, error) {
	s := sessionScan{idx: sessionIndex{}, pol: pol}
	if err := s.scan(ags); err != nil {
		return nil, err
	}
	return s.idx, nil
}

// indexSessionsStrict refreshes both harnesses. Structural failures abort;
// unknown owners remain evidence rather than being silently treated as absent.
func indexSessionsStrict(p *progress) (sessionIndex, []unverifiedSession, error) {
	s := sessionScan{idx: sessionIndex{}, pol: sessionErrPolicy{failClosed: true, p: p}, collect: true}
	err := s.scan(agents())
	return s.idx, s.unverified, err
}

func (s *sessionScan) scan(ags []agent) error {
	for _, a := range ags {
		if err := s.walk(a); err != nil {
			return err
		}
	}
	return nil
}

func (s *sessionScan) walk(a agent) error {
	if a.configDir() == "" {
		return nil
	}
	base, err := sessionRoot(filepath.Join(a.configDir(), a.sessionSubdir))
	if err != nil {
		return s.pol.sessionErr(fmt.Errorf("session root: %w", err))
	}
	if base == "" {
		return nil
	}
	return s.walkRoot(a, base)
}

func (s *sessionScan) walkRoot(a agent, base string) error {
	target, err := filepath.EvalSymlinks(base)
	if err != nil {
		return s.pol.sessionErr(fmt.Errorf("resolve sessions %s: %w", base, err))
	}
	return filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return s.pol.sessionErr(fmt.Errorf("walk sessions %s: %w", p, err))
		}
		return s.add(a, base, target, p, d)
	})
}

// Resolve only the root symlink; resolving ancestors rewrites macOS /var paths.
// A missing root is empty, but a dangling root symlink is a structural failure.
func sessionRoot(base string) (string, error) {
	info, err := os.Lstat(base)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		base, err = filepath.EvalSymlinks(base)
		if err != nil {
			return "", err
		}
	}
	return sessionDirectory(base)
}

func sessionDirectory(base string) (string, error) {
	info, err := os.Stat(base)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("sessions path is not a directory: %s", base)
	}
	return base, nil
}

func (s *sessionScan) add(a agent, base, target, p string, d fs.DirEntry) error {
	if d.Type()&os.ModeSymlink != 0 {
		return s.unknown(p, nil)
	}
	if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
		return nil
	}
	if !d.Type().IsRegular() {
		return s.unknown(p, nil)
	}
	cwd, err := a.readSessionOwner(p)
	if err != nil || !filepath.IsAbs(cwd) {
		return s.unknown(p, err)
	}
	key := canonical(cwd)
	s.idx[key] = append(s.idx[key], sessionRef{
		path: p, group: sessionGroup(base, p), root: base, rootTarget: target, owner: a.sessionOwner,
	})
	return nil
}

func (s *sessionScan) unknown(p string, err error) error {
	if s.collect {
		s.unverified = append(s.unverified, unverifiedSession{Path: p, ModTime: statFollowMtime(p)})
		return nil
	}
	if err != nil {
		return s.pol.sessionErr(fmt.Errorf("read session %s: %w", p, err))
	}
	warnProgress(s.pol.p, "unverified session ownership: %s", p)
	return nil
}

func statFollowMtime(p string) time.Time {
	if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
		return info.ModTime()
	}
	return time.Time{}
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
	newest, _ := sessionLastUsed(wtPath, idx, false)
	return newest
}

// sessionLastUsed fails closed for fresh removal checks. Display can retain
// known activity, but must disclose every file it could not inspect.
func sessionLastUsed(wtPath string, idx sessionIndex, strict bool) (time.Time, error) {
	var newest time.Time
	for _, r := range idx[canonical(wtPath)] {
		mtime, err := sessionMtime(r.path)
		if err != nil {
			if strict {
				return newest, err
			}
			warnProgress(nil, "%v", err)
		} else if mtime.After(newest) {
			newest = mtime
		}
	}
	return newest, nil
}

// An indexed transcript replaced with a link or directory is no longer verified.
func sessionMtime(p string) (time.Time, error) {
	info, err := os.Lstat(p)
	if err != nil {
		return time.Time{}, fmt.Errorf("stat session %s: %w", p, err)
	}
	if !info.Mode().IsRegular() {
		return time.Time{}, fmt.Errorf("session is not a regular file: %s", p)
	}
	return info.ModTime(), nil
}

// purgeSessions deletes only confirmed owned files. Unknown and unrelated
// history must survive lossy directory collisions; remove directories only
// when empty. GC purges by default; --keep-sessions opts out.
func purgeSessions(wtPath string, idx sessionIndex) (int, error) {
	n := 0
	for _, r := range idx[canonical(wtPath)] {
		if err := verifyPurgeSession(wtPath, r); err != nil {
			return n, err
		}
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

// Cached consent is not proof: use the original harness parser on current data.
func verifyPurgeSession(wtPath string, r sessionRef) error {
	if err := verifySessionBoundary(r, r.path); err != nil {
		return err
	}
	if _, err := sessionMtime(r.path); err != nil {
		return err
	}
	if r.owner == nil {
		return fmt.Errorf("unknown session parser: %s", r.path)
	}
	cwd, err := (agent{sessionOwner: r.owner}).readSessionOwner(r.path)
	if err != nil {
		return fmt.Errorf("read session %s: %w", r.path, err)
	}
	if !filepath.IsAbs(cwd) || canonical(cwd) != canonical(wtPath) {
		return fmt.Errorf("session ownership changed or unknown: %s", r.path)
	}
	return nil
}

// Pin the resolved root, then reject every descendant link before deletion.
func verifySessionBoundary(r sessionRef, p string) error {
	if r.root == "" || r.rootTarget == "" {
		return fmt.Errorf("unknown session boundary: %s", p)
	}
	target, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return fmt.Errorf("resolve session root %s: %w", r.root, err)
	}
	if target != r.rootTarget {
		return fmt.Errorf("session root changed: %s", r.root)
	}
	rel, err := filepath.Rel(r.root, p)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("session outside indexed root: %s", p)
	}
	return verifySessionAncestors(r.root, p)
}

func verifySessionAncestors(root, p string) error {
	for current := p; current != root; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if err != nil {
			return fmt.Errorf("inspect session path %s: %w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("session path replaced by symlink: %s", current)
		}
	}
	return nil
}

func removeEmptySessionDirs(r sessionRef) error {
	// An empty group would make filepath.Dir("") resolve to "." and walk the
	// cleanup above the session root.
	if r.group == "" {
		return nil
	}
	for dir := filepath.Dir(r.path); dir != filepath.Dir(r.group); dir = filepath.Dir(dir) {
		if err := verifySessionBoundary(r, dir); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
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
