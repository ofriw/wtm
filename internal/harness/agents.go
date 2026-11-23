package harness

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/temp"
	"wtm/internal/ui"
)

// agents.go — the agent-harness registry plus every operation shared by all
// harnesses (Pi.dev, Claude Code). One descriptor per harness; seeding,
// indexing and session purge never duplicate per-harness logic.

// ProjectMCPFile is the repo-root project MCP config. Pi's pi-mcp-adapter and
// Claude Code both read it, so it is a shared constant, not a per-harness one.
const ProjectMCPFile = ".mcp.json"

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
func agents() []agent { return []agent{PiAgent, ClaudeAgent} }

// capability is the agent's integration descriptor: registering the agent in
// agents() adds its cell token, --wide column and JSON key automatically.
func (a agent) capability() core.Capability {
	return core.Capability{
		ID: a.name, Token: a.statusToken,
		Probe: func(in core.CapInput) core.CapState { return core.PresentIf(a.wired(in)) },
	}
}

// wired reports whether the harness is set up for a worktree: any statusFiles
// path is present, either as a validated MCP config or as a plain file. MCP
// configs are validated because a corrupt one must not read healthy; a harness
// settings file is only a presence signal, so plain existence is enough.
func (a agent) wired(in core.CapInput) bool {
	for _, rel := range a.statusFiles {
		present, recognized := in.MCP[rel]
		if present || (!recognized && fs.Exists(filepath.Join(in.Root, rel))) {
			return true
		}
	}
	return false
}

// ConfigDir resolves the harness state dir: the env override when set, else
// home joined with configDirRel. An unset HOME yields "".
func (a agent) ConfigDir() string {
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
func MCPConfigFiles() []string {
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

// MCPConfigs inventories recognized configs once so merged and native status
// agree. An existing but unparseable config warns and counts as absent: status
// must never report a corrupt file as a healthy MCP setup. The warning goes
// through the progress handle because this runs during discovery, under the
// renderer.
func MCPConfigs(root string, p *ui.Progress) map[string]bool {
	files := MCPConfigFiles()
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
func validMCPConfig(path string, p *ui.Progress) bool {
	if !fs.Exists(path) {
		return false
	}
	var v any
	if err := fs.ReadJSON(path, &v); err != nil {
		ui.WarnProgress(p, "%s is not valid JSON (%s); treating as absent", path, err)
		return false
	}
	return true
}

// CopyHarness carries every harness's per-worktree state that git deliberately
// does not. Recognized MCP configs seed whenever the source has one and the
// checkout does not — an explicit exception to the ignore gate (agent wiring
// follows the worktree, ignored or not). Each harness's remaining seed
// files/dirs copy per-file only when git ignores them: tracked ones already
// arrived with the checkout, untracked-and-unignored ones are never touched.
func CopyHarness(srcRoot, dstRoot string) error {
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
	for _, rel := range MCPConfigFiles() {
		src, dst := filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel)
		if fs.Exists(src) && !fs.Exists(dst) {
			if err := fs.CopyFile(src, dst); err != nil {
				return err
			}
		}
	}
	return nil
}

func (a agent) copySeed(srcRoot, dstRoot string) error {
	for _, rel := range a.seedFiles {
		if !fs.Exists(filepath.Join(srcRoot, rel)) {
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
	if !fs.Exists(dir) {
		return nil
	}
	skip := seedSkipPaths(srcRoot, a)
	return filepath.WalkDir(dir, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		return copySeedEntry(srcRoot, dstRoot, skip, p, d)
	})
}

// copySeedEntry is one seed-walk step: skip paths owned by an MCP seed or by
// seedSkip, and copy ignored regular files.
func copySeedEntry(srcRoot, dstRoot string, skip map[string]bool, p string, d iofs.DirEntry) error {
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
	if fs.Exists(filepath.Join(dstRoot, rel)) {
		return nil
	}
	ignoreRoot := srcRoot
	if fs.Exists(filepath.Join(dstRoot, ".git")) {
		ignoreRoot = dstRoot
	}
	if !git.IsIgnored(ignoreRoot, rel) {
		return nil
	}
	return fs.CopyFile(filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel))
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

// sessionScan shares ownership and traversal rules for all harnesses. Fresh idle
// checks collect evidence for per-candidate decisions; ordinary discovery warns.
type sessionScan struct {
	idx        core.SessionIndex
	unverified []temp.UnverifiedSession
	pol        core.SessionErrPolicy
	collect    bool
}

// IndexSessions builds the session index for the given agents.
func IndexSessions(ags []agent, pol core.SessionErrPolicy) (core.SessionIndex, error) {
	s := sessionScan{idx: core.SessionIndex{}, pol: pol}
	if err := s.scan(ags); err != nil {
		return nil, err
	}
	return s.idx, nil
}

// IndexSessionsStrict refreshes both harnesses. Structural failures abort;
// unknown owners remain evidence rather than being silently treated as absent.
func IndexSessionsStrict(p *ui.Progress) (core.SessionIndex, []temp.UnverifiedSession, error) {
	s := sessionScan{idx: core.SessionIndex{}, pol: core.SessionErrPolicy{FailClosed: true, P: p}, collect: true}
	err := s.scan(agents())
	return s.idx, s.unverified, err
}

// DiscoverySessions adapts the harness indexer to core's discovery hook type.
func DiscoverySessions(failClosed bool) core.SessionDiscovery {
	return func(p *ui.Progress) (core.SessionIndex, []temp.UnverifiedSession, error) {
		idx, err := IndexSessions(agents(), core.SessionErrPolicy{FailClosed: failClosed, P: p})
		return idx, nil, err
	}
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
	if a.ConfigDir() == "" {
		return nil
	}
	base, err := sessionRoot(filepath.Join(a.ConfigDir(), a.sessionSubdir))
	if err != nil {
		return s.pol.SessionErr(fmt.Errorf("session root: %w", err))
	}
	if base == "" {
		return nil
	}
	return s.walkRoot(a, base)
}

func (s *sessionScan) walkRoot(a agent, base string) error {
	target, err := filepath.EvalSymlinks(base)
	if err != nil {
		return s.pol.SessionErr(fmt.Errorf("resolve sessions %s: %w", base, err))
	}
	return filepath.WalkDir(base, func(p string, d iofs.DirEntry, err error) error {
		if err != nil {
			return s.pol.SessionErr(fmt.Errorf("walk sessions %s: %w", p, err))
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

func (s *sessionScan) add(a agent, base, target, p string, d iofs.DirEntry) error {
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
	key := fs.Canonical(cwd)
	s.idx[key] = append(s.idx[key], core.SessionRef{
		Path: p, Group: sessionGroup(base, p), Root: base, RootTarget: target, Owner: a.sessionOwner,
	})
	return nil
}

func (s *sessionScan) unknown(p string, err error) error {
	if s.collect {
		s.unverified = append(s.unverified, temp.UnverifiedSession{Path: p, ModTime: statFollowMtime(p)})
		return nil
	}
	if err != nil {
		return s.pol.SessionErr(fmt.Errorf("read session %s: %w", p, err))
	}
	ui.WarnProgress(s.pol.P, "unverified session ownership: %s", p)
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

// PurgeSessions deletes only confirmed owned files. Unknown and unrelated
// history must survive lossy directory collisions; remove directories only
// when empty. GC purges by default; --keep-sessions opts out.
func PurgeSessions(wtPath string, idx core.SessionIndex) (int, error) {
	n := 0
	for _, r := range idx[fs.Canonical(wtPath)] {
		if err := verifyPurgeSession(wtPath, r); err != nil {
			return n, err
		}
		if err := os.Remove(r.Path); err != nil {
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
func verifyPurgeSession(wtPath string, r core.SessionRef) error {
	if err := verifySessionBoundary(r, r.Path); err != nil {
		return err
	}
	if _, err := core.SessionMtime(r.Path); err != nil {
		return err
	}
	if r.Owner == nil {
		return fmt.Errorf("unknown session parser: %s", r.Path)
	}
	cwd, err := (agent{sessionOwner: r.Owner}).readSessionOwner(r.Path)
	if err != nil {
		return fmt.Errorf("read session %s: %w", r.Path, err)
	}
	if !filepath.IsAbs(cwd) || fs.Canonical(cwd) != fs.Canonical(wtPath) {
		return fmt.Errorf("session ownership changed or unknown: %s", r.Path)
	}
	return nil
}

// Pin the resolved root, then reject every descendant link before deletion.
func verifySessionBoundary(r core.SessionRef, p string) error {
	if r.Root == "" || r.RootTarget == "" {
		return fmt.Errorf("unknown session boundary: %s", p)
	}
	target, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return fmt.Errorf("resolve session root %s: %w", r.Root, err)
	}
	if target != r.RootTarget {
		return fmt.Errorf("session root changed: %s", r.Root)
	}
	rel, err := filepath.Rel(r.Root, p)
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("session outside indexed root: %s", p)
	}
	return verifySessionAncestors(r.Root, p)
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

func removeEmptySessionDirs(r core.SessionRef) error {
	// An empty group would make filepath.Dir("") resolve to "." and walk the
	// cleanup above the session root.
	if r.Group == "" {
		return nil
	}
	for dir := filepath.Dir(r.Path); dir != filepath.Dir(r.Group); dir = filepath.Dir(dir) {
		done, err := removeSessionDirIfEmpty(r, dir)
		if err != nil || done {
			return err
		}
	}
	return nil
}

// removeSessionDirIfEmpty deletes dir when it holds nothing, reporting done so
// the caller stops climbing. A vanished or repopulated dir ends the climb.
func removeSessionDirIfEmpty(r core.SessionRef, dir string) (bool, error) {
	if err := verifySessionBoundary(r, dir); err != nil {
		return true, boundaryCleanupErr(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		// WHY: a concurrent purge already removed the dir — cleanup is done,
		// not failed.
		return true, boundaryCleanupErr(err)
	}
	if len(entries) != 0 {
		return true, nil
	}
	return false, os.Remove(dir)
}

// boundaryCleanupErr maps boundary/IO errors onto the climb: a path that no
// longer exists ends cleanup, everything else is a real failure.
func boundaryCleanupErr(err error) error {
	if errors.Is(err, iofs.ErrNotExist) || os.IsNotExist(err) {
		return nil
	}
	return err
}
