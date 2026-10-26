package main

import (
	"bufio"
	"bytes"
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
	line, _, err := sessionRecord(r)
	if err == io.EOF {
		err = nil
	}
	return recordCWD(line), err
}

// Claude can write queue metadata before the first record with a cwd.
func claudeSessionOwner(r *bufio.Reader) (string, error) {
	for n := 0; n < maxSessionOwnerRecords; n++ {
		line, _, err := sessionRecord(r)
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

// sessionRecord returns one record. An oversized record drains without being
// retained and reports oversized=true exactly once, so the parser can count the
// loss and continue at the next record. Owner scans ignore the flag: an
// oversized header proves nothing about ownership.
func sessionRecord(r *bufio.Reader) ([]byte, bool, error) {
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
			return line, oversized, err
		}
	}
}

// recordType reads the top-level "type" from a truncated prefix. Nested types
// cannot classify the record, and a prefix that ends first fails closed.
func recordType(prefix []byte) (string, bool) {
	dec := json.NewDecoder(bytes.NewReader(prefix))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return "", false
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", false
		}
		if key == "type" {
			v, err := dec.Token()
			t, ok := v.(string)
			return t, ok && err == nil
		}
		if dec.Decode(new(json.RawMessage)) != nil {
			return "", false
		}
	}
	return "", false
}

// sessionIndex maps a canonical worktree cwd to its session files. It is built
// once per command via indexSessions so discovery costs one walk per harness
// instead of one per worktree.
type sessionIndex map[string][]sessionRef

// sessionRef carries an owned file, the boundary for empty-dir cleanup, and
// the harness that owns it, so usage loads without re-resolving the harness.
type sessionRef struct {
	path       string
	group      string
	harness    string
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
	target, err := filepath.EvalSymlinks(base)
	if err != nil {
		return s.pol.sessionErr(fmt.Errorf("resolve sessions %s: %w", base, err))
	}
	s.pol.p.detail(a.name)
	cands, unverified, walkErrs := collectSessions(a, base, target)
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
	return s.resolve(a, cands)
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
func collectSessions(a agent, base, target string) ([]sessionCandidate, []unverifiedFile, []error) {
	var (
		cands      []sessionCandidate
		unverified []unverifiedFile
		errs       []error
	)
	_ = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
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

// resolve reads owners in a bounded pool: the kernel and the disk, not the CPU,
// bound this walk. Reads merge in candidate order, so the index and its
// warnings never depend on completion order.
func (s *sessionScan) resolve(a agent, cands []sessionCandidate) error {
	var processed atomic.Int64
	reads := mapParallel(len(cands), scanWorkers, func(i int) ownerRead {
		c := cands[i]
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
	for _, r := range reads {
		if r.cwd == "" {
			if err := s.unknown(r.candidate.path, r.err); err != nil {
				return err
			}
			continue
		}
		c := r.candidate
		s.idx[r.cwd] = append(s.idx[r.cwd], sessionRef{
			path: c.path, group: c.group, harness: a.name, root: c.root, rootTarget: c.rootTarget, owner: a.sessionOwner,
		})
	}
	return nil
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

// lastUsed is the newest mtime among the worktree's session files (looked up
// in idx); zero when the worktree has never hosted a session.
func lastUsed(wtPath string, idx sessionIndex, p *progress) time.Time {
	newest, _ := sessionLastUsed(wtPath, idx, false, p)
	return newest
}

// sessionLastUsed fails closed for fresh removal checks. Display can retain
// known activity, but must disclose every file it could not inspect. The
// warning rides p because discovery workers run under the renderer and a raw
// stderr write would be overwritten by the next frame.
func sessionLastUsed(wtPath string, idx sessionIndex, strict bool, p *progress) (time.Time, error) {
	var newest time.Time
	for _, r := range idx[canonical(wtPath)] {
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

// parsedSession pairs one readable session's events with its fold inputs, so
// buildUsage can parse every session before it folds any.
type parsedSession struct {
	harness  string
	events   []usageEvent
	activity []activityEvent
	price    func(usageEvent) pricedEvent
}

// usageLoss accumulates what a parse walk lost, so folding can taint the total
// after every readable session has been seen.
type usageLoss struct {
	lost     map[string]bool
	firstErr error
	dropped  int
}

func newUsageLoss() usageLoss { return usageLoss{lost: map[string]bool{}} }

func (l *usageLoss) fail(harness string, err error) {
	l.lost[harness] = true
	if l.firstErr == nil {
		l.firstErr = err
	}
}

func (l *usageLoss) recordDropped(harness string, n int) {
	if n == 0 {
		return
	}
	// The readable records still fold; the dropped ones only taint the
	// total, so a partial sum never reads as exact.
	l.lost[harness] = true
	l.dropped += n
}

// buildUsage parses a worktree's owned sessions once and aggregates cost/time.
// cat is nil when no catalog is available: a catalog-rated harness then reports
// unknown cost instead of a wrong number. subs is the run's credential proof; an
// empty proof leaves every event at its list price, and a worktree-level metered
// override drops the Claude proof for this worktree alone.
func buildUsage(wtPath string, idx sessionIndex, cat catalog, subs subscriptions) (worktreeUsage, error) {
	return buildUsageSessions(wtPath, idx[canonical(wtPath)], cat, subs)
}

func buildUsageSessions(wtPath string, refs []sessionRef, cat catalog, subs subscriptions) (worktreeUsage, error) {
	subs = subs.forWorktree(wtPath)
	sessions, loss := parseWorktreeSessions(refs, cat, subs)
	var u worktreeUsage
	foldParsedSessions(&u, sessions)
	markPartial(&u, loss.lost)
	return u, usageLossErr(loss.firstErr, loss.dropped)
}

// parseWorktreeSessions reads every owned session, remembering unreadable files
// and dropped records so the fold can taint the total.
func parseWorktreeSessions(refs []sessionRef, cat catalog, subs subscriptions) ([]parsedSession, usageLoss) {
	loss := newUsageLoss()
	var sessions []parsedSession
	for _, ref := range refs {
		if s, ok := parseWorktreeSession(ref, cat, subs, &loss); ok {
			sessions = append(sessions, s)
		}
	}
	return sessions, loss
}

func parseWorktreeSession(ref sessionRef, cat catalog, subs subscriptions, loss *usageLoss) (parsedSession, bool) {
	a, ok := agentByName(ref.harness)
	if !ok || a.parseUsage == nil {
		return parsedSession{}, false
	}
	parsed, err := readUsage(a, ref.path)
	if err != nil {
		// A bad file sorts anywhere, so record the loss now and taint the
		// total after folding, when even a good session listed later in the
		// walk has folded.
		loss.fail(ref.harness, err)
		return parsedSession{}, false
	}
	loss.recordDropped(ref.harness, parsed.dropped)
	return parsedSession{harness: ref.harness, events: parsed.events, activity: parsed.activity, price: usagePricer(a, cat, subs)}, true
}

func foldParsedSessions(u *worktreeUsage, sessions []parsedSession) {
	lists := make([][]usageEvent, len(sessions))
	for i, s := range sessions {
		lists[i] = s.events
	}
	// Spans fold the original events; only the billed winners carry cost. This
	// keeps activity time independent of the session order and of what dedupe
	// removes from a shared transcript.
	billed := dedupeKeyedEvents(lists)
	for i, s := range sessions {
		u.addBilledSession(s.harness, s.events, s.activity, billed[i], s.price)
	}
}

// usageLossErr reports the session loss that makes a total partial: an
// unreadable session, unreadable records, or both. One worktree-level warning
// keeps a read-only run alive while still disclosing the loss.
func usageLossErr(first error, dropped int) error {
	switch {
	case first != nil && dropped > 0:
		return fmt.Errorf("%w; %d session record(s) dropped (bad JSON, timestamp, or oversized)", first, dropped)
	case first != nil:
		return first
	case dropped > 0:
		return fmt.Errorf("%d session record(s) dropped (bad JSON, timestamp, or oversized); totals are partial", dropped)
	default:
		return nil
	}
}

// markPartial taints the total and each harness that kept some spans but also
// lost a session or records, so a partial sum never looks exact. A harness with
// no readable spans keeps no entry: a zero-span unknown would read as 0s active
// instead of none, so the worktree-level flag carries that loss instead. Every
// model in an affected harness is marked because the lost records may have
// belonged to any of them; an unaffected harness stays exact.
func markPartial(u *worktreeUsage, lost map[string]bool) {
	if len(lost) == 0 {
		return
	}
	u.partial = true
	for name := range lost {
		h, ok := u.harnesses[name]
		if !ok {
			continue
		}
		h.partial = true
		h.cost.addUnknown()
		for _, m := range h.models {
			m.partial = true
			m.cost.addUnknown()
		}
	}
}

func readUsage(a agent, path string) (sessionParse, error) {
	f, err := os.Open(path)
	if err != nil {
		return sessionParse{}, err
	}
	defer func() { _ = f.Close() }()
	return a.parseUsage(bufio.NewReader(f))
}

// usagePricer resolves one event to its price pair and per-tier breakdown. The
// list side is the pay-as-you-go price today's code reports; a proven
// subscription replaces the billed side with the $0 marginal cost while the
// list side stays visible. A Claude proof that still permits overage yields an
// estimated zero only when the event priced something (a known nonzero list,
// real tokens, or a self-priced cost); a zero-token event keeps the exact zero,
// so an empty turn never reads estimated. The substitution reaches every tier,
// so a covered event never shows a bare $0.00 beside a metered list per tier.
// API-key and otherwise metered usage still bills, so a proven $0 never claims
// the whole invoice.
func usagePricer(a agent, cat catalog, subs subscriptions) func(usageEvent) pricedEvent {
	return func(e usageEvent) pricedEvent {
		p := listPrice(a, cat, e)
		switch subs.coverage(a.name, e.provider, e.at) {
		case coverageAllowance:
			coverTiers(&p, knownZero)
		case coverageOveragePossible:
			if (p.total.list.known() && p.total.list.amount != 0) || e.tokens.total() > 0 || e.webSearch > 0 || e.cost != nil {
				coverTiers(&p, estimatedZero)
			} else {
				coverTiers(&p, knownZero)
			}
		}
		return p
	}
}

// coverTiers replaces the billed side of the total and every priced tier with
// the coverage zero, keeping the list side intact. The invariant: every cost
// figure carries the billed/list pair, so a proven subscription discounts each
// tier exactly as it discounts the total. A tier the event never priced stays
// absent rather than becoming a spurious $0.00.
func coverTiers(p *pricedEvent, zero func() money) {
	p.total.billed = zero()
	p.total.subscription = true
	for i := range p.tiers {
		if !p.tiers[i].list.any {
			continue
		}
		p.tiers[i].billed = zero()
		p.tiers[i].subscription = true
	}
}

// listPrice resolves one event's pay-as-you-go price and per-tier breakdown: a
// harness-supplied cost wins, a catalog-rated token record is priced from the
// catalog, and everything else (including a catalog-rated record with no
// catalog) is unknown.
func listPrice(a agent, cat catalog, e usageEvent) pricedEvent {
	switch {
	case e.cost != nil:
		return piPrice(e)
	case a.catalogRated && cat != nil:
		return cat.price(e)
	default:
		return unpriced()
	}
}

// piPrice prices a self-reported Pi event: the record carries the total and,
// in modern sessions, the per-class split. An older record with only the total
// leaves every tier absent rather than inventing a breakdown.
func piPrice(e usageEvent) pricedEvent {
	var p pricedEvent
	p.total.list.addKnown(*e.cost)
	if c := e.tierCost; c != nil {
		addKnownClass(&p.tiers[tierInput].list, c.input)
		addKnownClass(&p.tiers[tierOutput].list, c.output)
		addKnownClass(&p.tiers[tierCacheRead].list, c.cacheRead)
		addKnownClass(&p.tiers[tierCacheWrite].list, c.cacheWrite)
	}
	for i := range p.tiers {
		p.tiers[i].billed = p.tiers[i].list
	}
	p.total.billed = p.total.list
	return p
}

// addKnownClass records a harness-reported class price. A nil field leaves the
// tier untouched, which reads as absent, never as a real zero.
func addKnownClass(m *money, v *float64) {
	if v != nil {
		m.addKnown(*v)
	}
}

// agentByName resolves a session owner to its descriptor.
func agentByName(name string) (agent, bool) {
	for _, a := range agents() {
		if a.name == name {
			return a, true
		}
	}
	return agent{}, false
}

// hasHarness reports whether any of the given worktrees owns a session from
// the named harness. Status uses it to load the catalog only when it can be
// needed, so a Pi-only repo never touches the network.
func (idx sessionIndex) hasHarness(wts []worktree, name string) bool {
	for _, w := range wts {
		for _, r := range idx[canonical(w.Path)] {
			if r.harness == name {
				return true
			}
		}
	}
	return false
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
