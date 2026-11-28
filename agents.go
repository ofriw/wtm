package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// agents.go — the agent-harness registry plus every operation shared by all
// harnesses (Pi.dev, Claude Code): discovery, owner indexing, and session
// purge. The usage report loader and attribution live in usage_report.go;
// event pricing lives in usage.go/rates.go.

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
	// parseUsage reads one session file into cost events, causal turn
	// activity, and the count of records it could not read (bad JSON,
	// timestamp, or oversized). A nonzero count lets the caller warn and taint
	// the total partial. catalogRated marks a harness whose records carry tokens
	// but no cost, so the public catalog must price them (Claude Code); a
	// self-pricing harness (Pi) adds cost to each event.
	parseUsage   func(*bufio.Reader) (sessionParse, error)
	catalogRated bool
}

// agents is the SSOT for known harnesses: seeding, indexing and MCP status
// all iterate it.
func agents() []agent { return []agent{piAgent, claudeAgent} }

// capability is the agent's integration descriptor: registering the agent in
// agents() adds its cell token, --wide column and JSON key automatically.
func (a agent) capability() capability {
	return capability{
		id: a.name, token: a.statusToken,
		probe: func(in capInput) capState { return presentIf(a.wired(in)) },
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

// configDirBase resolves the harness state base dir: the env override when set,
// else the user's home. An unset HOME yields "".
func (a agent) configDirBase() string {
	if d := os.Getenv(a.configDirEnv); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return home
}

// configDir resolves the harness state dir: the env override when set (used
// as-is, matching the original behavior), else the home base joined with
// configDirRel. An unresolvable base yields "".
func (a agent) configDir() string {
	if d := os.Getenv(a.configDirEnv); d != "" {
		return d
	}
	base := a.configDirBase()
	if base == "" {
		return ""
	}
	return filepath.Join(append([]string{base}, a.configDirRel...)...)
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
// maxSessionPrefix bounds the prefix used to classify a discarded record.
const (
	maxSessionHeader = 4 << 20
	maxSessionPrefix = 8 << 10
)

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
	line, _, _, err := sessionRecord(r)
	if err == io.EOF {
		err = nil
	}
	return recordCWD(line), err
}

// Claude can write queue metadata before the first record with a cwd.
func claudeSessionOwner(r *bufio.Reader) (string, error) {
	for n := 0; n < maxSessionOwnerRecords; n++ {
		line, _, _, err := sessionRecord(r)
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

// sessionRecord returns one record. An oversized record drains and reports
// oversized=true once with a bounded prefix for billing classification. Owner
// scans ignore the prefix: an oversized header proves nothing about ownership.
func sessionRecord(r *bufio.Reader) (line []byte, oversized bool, prefix []byte, err error) {
	for {
		part, rerr := r.ReadSlice('\n')
		if !oversized {
			if len(line)+len(part) > maxSessionHeader {
				prefix = append(prefix, line...)
				prefix = append(prefix, part...)
				if len(prefix) > maxSessionPrefix {
					prefix = prefix[:maxSessionPrefix]
				}
				oversized, line = true, nil
			} else {
				line = append(line, part...)
			}
		} else if len(prefix) < maxSessionPrefix {
			keep := min(len(part), maxSessionPrefix-len(prefix))
			prefix = append(prefix, part[:keep]...)
		}
		if rerr != bufio.ErrBufferFull {
			return line, oversized, prefix, rerr
		}
	}
}

// tailLossError marks a truncated final record: the parse kept the completed
// records before the cut, and only the tail is lost. It unwraps to
// io.ErrUnexpectedEOF so callers can tell record-level loss from a whole-session
// open failure with errors.Is.
type tailLossError struct{}

func (tailLossError) Error() string { return "session ends mid-record; completed records kept" }
func (tailLossError) Unwrap() error { return io.ErrUnexpectedEOF }

// sessionRecordErr maps an unterminated final record to the tail-loss sentinel:
// a killed writer leaves bytes without the JSONL newline; the parse keeps the
// completed records and reports the tail as a record-level loss, so the lost
// turn can never read as a complete total.
func sessionRecordErr(line []byte, oversized bool, err error) error {
	if err == io.EOF && (len(line) > 0 || oversized) {
		return tailLossError{}
	}
	return err
}

// sessionIndex maps a canonical worktree cwd to its session files. It is built
// once per command via indexSessions so discovery costs one walk per harness
// instead of one per worktree.
type sessionIndex map[string][]sessionRef

// sessionRef carries an owned file, the boundary for empty-dir cleanup, the
// harness that owns it, and the cwd the session was recorded from, so usage
// loads without re-resolving the harness and prices with the session's own
// settings, not the worktree root's.
type sessionRef struct {
	path       string
	group      string
	harness    string
	cwd        string
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
		if err := s.pol.p.abort(); err != nil {
			return err
		}
		if err := s.walk(a); err != nil {
			return err
		}
	}
	return nil
}

// sessionBase resolves the agent's session root and its resolved target. An
// empty base means the agent has no session directory to scan.
func sessionBase(a agent) (base, target string, err error) {
	if a.configDir() == "" {
		return "", "", nil
	}
	base, err = sessionRoot(filepath.Join(a.configDir(), a.sessionSubdir))
	if err != nil {
		return "", "", fmt.Errorf("session root: %w", err)
	}
	if base == "" {
		return "", "", nil
	}
	target, err = filepath.EvalSymlinks(base)
	if err != nil {
		return "", "", fmt.Errorf("resolve sessions %s: %w", base, err)
	}
	return base, target, nil
}

func (s *sessionScan) walk(a agent) error {
	base, target, err := sessionBase(a)
	if err != nil {
		return s.pol.sessionErr(err)
	}
	if base == "" {
		return nil
	}
	s.pol.p.detail(a.name)
	if err := s.pol.p.abort(); err != nil {
		return err
	}
	cands, unverified, walkErrs := collectSessions(a, base, target, s.pol.p)
	if err := s.pol.p.abort(); err != nil {
		return err
	}
	if err := s.reportWalkFindings(walkErrs, unverified); err != nil {
		return err
	}
	return s.resolve(a, cands)
}

// reportWalkFindings applies the session error policy to walk findings in their
// fixed order: structural walk errors abort in a strict run, then unknown-owner
// paths become evidence.
func (s *sessionScan) reportWalkFindings(walkErrs []error, unverified []unverifiedFile) error {
	for _, walkErr := range walkErrs {
		if perr := s.pol.sessionErr(walkErr); perr != nil {
			return perr
		}
	}
	for _, u := range unverified {
		if err := s.unknown(u.path, u.err); err != nil {
			return err
		}
	}
	return nil
}

// sessionCandidate is one regular session file awaiting an owner read. The walk
// collects candidates first so the reads can run in a bounded pool.
type sessionCandidate struct {
	path       string
	group      string
	root       string
	rootTarget string
}

// unverifiedFile is one path whose ownership cannot be read, with the cause.
type unverifiedFile struct {
	path string
	err  error
}

// collectSessions lists regular session files under base without reading them.
// Walk and classification errors are returned, not applied: the caller owns the
// error policy, so a lenient run reports and continues while a strict run
// aborts. Order is WalkDir's, which keeps the index deterministic.
func collectSessions(a agent, base, target string, prog *progress) ([]sessionCandidate, []unverifiedFile, []error) {
	var (
		cands      []sessionCandidate
		unverified []unverifiedFile
		errs       []error
	)
	werr := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if aerr := prog.abort(); aerr != nil {
			return aerr
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("walk sessions %s: %w", p, err))
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			unverified = append(unverified, unverifiedFile{path: p})
			return nil
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".jsonl") {
			return nil
		}
		if !d.Type().IsRegular() {
			unverified = append(unverified, unverifiedFile{path: p})
			return nil
		}
		cands = append(cands, sessionCandidate{path: p, group: sessionGroup(base, p), root: base, rootTarget: target})
		return nil
	})
	// WalkDir returns the walkFn's abort error on interrupt; the caller checks
	// prog.abort() after this and surfaces it, so it is not a walk finding. Any
	// other walk failure is a real finding and joins errs. runProgressProgram
	// translates the context errors to errInterrupted at the top level.
	if werr != nil && !errors.Is(werr, context.Canceled) && !errors.Is(werr, context.DeadlineExceeded) {
		errs = append(errs, fmt.Errorf("walk sessions %s: %w", base, werr))
	}
	return cands, unverified, errs
}

// sessionProgressEvery bounds progress chatter: one label update per chunk of
// owner reads, not one per file. A real machine walks 15k session files.
const sessionProgressEvery = 128

// ownerRead is one pooled owner lookup. cwd is empty when the file is not
// attributable, with err naming the cause for the caller's error policy.
type ownerRead struct {
	candidate sessionCandidate
	cwd       string
	err       error
}

// readOwners looks up owners in a bounded pool: the kernel and disk, not the
// CPU, bound this walk. Reads stay in candidate order for the merge.
func (s *sessionScan) readOwners(a agent, cands []sessionCandidate) []ownerRead {
	var processed atomic.Int64
	return mapParallel(len(cands), scanWorkers, func(i int) ownerRead {
		c := cands[i]
		if err := s.pol.p.abort(); err != nil {
			return ownerRead{candidate: c, err: err}
		}
		cwd, err := a.readSessionOwner(c.path)
		n := processed.Add(1)
		if n%sessionProgressEvery == 0 {
			s.pol.p.detail(fmt.Sprintf("%s · %d files", a.name, n))
		}
		if err != nil || !filepath.IsAbs(cwd) {
			return ownerRead{candidate: c, err: err}
		}
		return ownerRead{candidate: c, cwd: canonical(cwd)}
	})
}

// mergeReads folds read results into the index in candidate order, so the index
// and its warnings never depend on completion order.
func (s *sessionScan) mergeReads(a agent, reads []ownerRead) error {
	for _, r := range reads {
		if err := s.pol.p.abort(); err != nil {
			return err
		}
		if r.cwd == "" {
			if err := s.unknown(r.candidate.path, r.err); err != nil {
				return err
			}
			continue
		}
		c := r.candidate
		s.idx[r.cwd] = append(s.idx[r.cwd], sessionRef{
			path: c.path, group: c.group, harness: a.name, cwd: r.cwd,
			root: c.root, rootTarget: c.rootTarget, owner: a.sessionOwner,
		})
	}
	return nil
}

func (s *sessionScan) resolve(a agent, cands []sessionCandidate) error {
	return s.mergeReads(a, s.readOwners(a, cands))
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

// sessionLastUsed fails closed for fresh removal checks. Display can retain
// known activity, but must disclose every file it could not inspect. The
// warning rides p because discovery workers run under the renderer and a raw
// stderr write would be overwritten by the next frame. Descendant sessions
// count too, so GC never reclaims a worktree whose only recent work ran from a
// subdirectory. sessionsUnder includes nested worktrees' sessions for the
// parent: that over-approximates COST attribution and is conservative for
// reclamation and harmless for display.
func sessionLastUsed(wtPath string, idx sessionIndex, strict bool, p *progress) (time.Time, error) {
	var newest time.Time
	for _, r := range sessionsUnder(wtPath, idx) {
		mtime, err := sessionMtime(r.path)
		if err != nil {
			if strict {
				return newest, err
			}
			warnProgress(p, "%v", err)
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
		done, err := removeSessionDirIfEmpty(r, dir)
		if err != nil || done {
			return err
		}
	}
	return nil
}

// removeSessionDirIfEmpty deletes dir when it holds nothing, reporting done so
// the caller stops climbing. A vanished or repopulated dir ends the climb.
func removeSessionDirIfEmpty(r sessionRef, dir string) (bool, error) {
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
	if errors.Is(err, fs.ErrNotExist) || os.IsNotExist(err) {
		return nil
	}
	return err
}
