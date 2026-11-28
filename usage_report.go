package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// usage_report.go — the usage report loader and attribution. Session reads,
// loss accounting, deepest-owner partition, and the fold live here, so the
// harness registry file owns discovery and purge only.

// parsedSession pairs one readable session's events with its fold inputs, so
// the report path can parse every session before it folds any.
type parsedSession struct {
	harness  string
	events   []usageEvent
	activity []activityEvent
	price    func(usageEvent) pricedEvent
	proof    coveredProof // per-event subscription proof for the dedupe election
}

// usageLoss accumulates what a parse walk lost, so folding can taint the total
// after every readable session has been seen.
type usageLoss struct {
	lost        map[string]bool // whole-session failures: cost blanks to unknown
	tailLost    map[string]bool // record-level tail loss: completed cost survives, partial
	errs        []error         // every distinct session-read cause, disclosed
	dropped     int
	unsupported map[string]int
}

func newUsageLoss() usageLoss {
	return usageLoss{lost: map[string]bool{}, tailLost: map[string]bool{}, unsupported: map[string]int{}}
}

func (l *usageLoss) fail(harness string, err error) {
	l.lost[harness] = true
	l.addCause(err)
}

// addCause records one failure cause for disclosure. Later causes are never
// hidden behind the first; identical text collapses so a repeat cannot bury a
// distinct cause.
func (l *usageLoss) addCause(err error) {
	if err == nil {
		return
	}
	text := err.Error()
	for _, e := range l.errs {
		if e.Error() == text {
			return
		}
	}
	l.errs = append(l.errs, err)
}

func (l *usageLoss) recordDropped(harness string, n int, unsupported map[string]int) {
	if n == 0 {
		return
	}
	// The readable records still fold; the dropped ones only taint the
	// total, so a partial sum never reads as exact.
	l.lost[harness] = true
	l.dropped += n
	for t, c := range unsupported {
		l.unsupported[t] += c
	}
}

// recordTailLoss keeps the completed records of a session whose final record was
// cut (io.ErrUnexpectedEOF). The tail itself counts as at least one lost record;
// the harness taints partial but keeps its measured cost, unlike a whole-session
// failure that blanks everything unknown.
func (l *usageLoss) recordTailLoss(harness string, n int, unsupported map[string]int, err error) {
	l.tailLost[harness] = true
	l.addCause(err)

	if n < 1 {
		n = 1 // the cut tail itself is at least one lost record
	}
	l.dropped += n
	for t, c := range unsupported {
		l.unsupported[t] += c
	}
}

func buildUsageSessionsReport(wtPath string, refs []sessionRef, cat catalog, subs subscriptions, now time.Time, budget *usageBudget) (worktreeUsage, []error, error) {
	proofs := newProofCache(subs, wtPath)
	sessions, loss := parseWorktreeSessions(refs, cat, proofs, budget)
	var u worktreeUsage
	foldParsedSessions(&u, sessions, now, proofs.sink())
	markPartial(&u, loss.lost)
	markTailPartial(&u, loss.tailLost)
	return u, proofs.warnings, usageLossErr(loss.errs, loss.dropped, loss.unsupported)
}

// proofCache resolves the proof per session cwd: a metered settings file in a
// session's own directory, or above it, voids that session's proof instead of
// hiding behind the worktree root's clean verdict. A ref built without a cwd
// falls back to the worktree root, so tests and callers that never index a cwd
// keep the previous pricing.
//
// WHY memoized: one status run holds many sessions per cwd and each proof walks
// the disk. The cache stores the resolved subscription per cwd; the worktree
// root resolves through the same map, so a descendant cwd seeds the root
// exactly once. Warnings leave through the worker sink (dedupeWarn), never via
// a stored per-cwd slice, and the parent pass collapses overlap across
// worktrees (see emitUsageResults).
type proofCache struct {
	base     subscriptions
	wtPath   string
	byCwd    map[string]subscriptions
	warnings []error
}

func newProofCache(base subscriptions, wtPath string) *proofCache {
	return &proofCache{base: base, wtPath: wtPath, byCwd: map[string]subscriptions{}}
}

func (c *proofCache) forRef(ref sessionRef) subscriptions {
	return c.forCwd(ref.cwd)
}

func (c *proofCache) forCwd(cwd string) subscriptions {
	if cwd == "" {
		cwd = c.wtPath
	}
	if subs, ok := c.byCwd[cwd]; ok {
		return subs
	}
	subs, warnings := c.base.forWorktree(cwd)
	// Resolve the worktree root through the same map first: a descendant cwd
	// sees the root's verdict even when no session ever touched the root, and
	// the root seeds exactly once.
	if cwd != c.wtPath {
		root := c.forCwd(c.wtPath)
		subs.claude = subs.claude && root.claude
	}
	// Only settings refusals fall back to list price, so only they carry the
	// caveat. Record warnings keep their own cause verbatim.
	for _, w := range warnings {
		c.warnings = append(c.warnings, fmt.Errorf("%w; claude costs use list price", w))
	}
	c.byCwd[cwd] = subs
	return subs
}

// sink returns the worker's dedupe sink: repeated identical warnings collapse
// to one line per worker, so a large corrupt transcript stays memory-bound and
// the parent pass (emitUsageResults) only handles overlap across worktrees.
func (c *proofCache) sink() warnSink {
	return dedupeWarn(func(err error) { c.warnings = append(c.warnings, err) })
}

// sessionsUnder returns the sessions whose cwd is the worktree or a descendant
// of it, including nested worktrees' sessions. LAST USED deliberately
// over-approximates COST attribution: reclamation must be conservative. Exact
// refs keep their index order; descendant refs follow in cwd order so dedupe
// stays deterministic.
func sessionsUnder(wtPath string, idx sessionIndex) []sessionRef {
	root := canonical(wtPath)
	refs := slices.Clone(idx[root])
	var cwds []string
	for cwd := range idx {
		if cwd != root && pathInside(cwd, root) {
			cwds = append(cwds, cwd)
		}
	}
	slices.Sort(cwds)
	for _, cwd := range cwds {
		refs = append(refs, idx[cwd]...)
	}
	return refs
}

// pathInside reports whether p is strictly below root, using a separator
// boundary so /repo-feature is not inside /repo.
func pathInside(p, root string) bool {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || rel == ".." {
		return false
	}
	return !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// worktreePartitions assigns every session cwd to its deepest owning worktree,
// computed once per run instead of re-scanning for every harness check and
// worktree fold.
type worktreePartitions map[string][]sessionRef

// partitionSessions maps each indexed cwd to the deepest worktree that owns
// it. Exact cwd==root refs keep index order; descendant refs follow in cwd
// order, so a partition matches the nested-checkout attribution for cost and
// report paths.
func partitionSessions(wts []worktree, idx sessionIndex) worktreePartitions {
	roots := make([]string, len(wts))
	for i, w := range wts {
		roots[i] = canonical(w.Path)
	}
	parts := make(worktreePartitions, len(roots))
	rootSet := make(map[string]bool, len(roots))
	for _, root := range roots {
		parts[root] = slices.Clone(idx[root])
		rootSet[root] = true
	}
	desc := make(map[string][]string, len(roots))
	for cwd := range idx {
		if rootSet[cwd] {
			continue
		}
		owner := ""
		for _, root := range roots {
			if pathInside(cwd, root) && len(root) > len(owner) {
				owner = root
			}
		}
		if owner != "" {
			desc[owner] = append(desc[owner], cwd)
		}
	}
	for _, root := range roots {
		cwds := desc[root]
		slices.Sort(cwds)
		for _, cwd := range cwds {
			parts[root] = append(parts[root], idx[cwd]...)
		}
	}
	return parts
}

// parseWorktreeSessions reads every owned session, remembering unreadable files
// and dropped records so the fold can taint the total.
func parseWorktreeSessions(refs []sessionRef, cat catalog, proofs *proofCache, budget *usageBudget) ([]parsedSession, usageLoss) {
	loss := newUsageLoss()
	var sessions []parsedSession
	for _, ref := range refs {
		if s, ok := parseWorktreeSession(ref, cat, proofs, &loss, budget); ok {
			sessions = append(sessions, s)
		}
	}
	return sessions, loss
}

func parseWorktreeSession(ref sessionRef, cat catalog, proofs *proofCache, loss *usageLoss, budget *usageBudget) (parsedSession, bool) {
	a, ok := agentByName(ref.harness)
	if !ok || a.parseUsage == nil {
		return parsedSession{}, false
	}
	parsed, ok := readSession(a, ref, loss, budget)
	if !ok {
		return parsedSession{}, false
	}
	warn := proofs.sink()
	// Sanitize before dedupe: a corrupt count must never win a slot or feed the fold.
	sanitizeEvents(parsed.events, warn)
	subs := sessionSubscriptions(ref, proofs)
	price := usagePricer(a, cat, subs, warn)
	return parsedSession{harness: ref.harness, events: parsed.events, activity: parsed.activity, price: price, proof: sessionProof(ref, subs)}, true
}

// readSession reads one session file and records any loss: a whole-session open
// failure drops the session (an unreadable file sorts anywhere, so the loss is
// tainted after the fold joins every session); a cut final record is a
// record-level loss that keeps the completed records and taints only the tail.
// Dropped records are the parse's own disclosure count.
func readSession(a agent, ref sessionRef, loss *usageLoss, budget *usageBudget) (sessionParse, bool) {
	if !budgetAllows(budget, ref.path) {
		loss.fail(ref.harness, errBudgetSkipped)
		return sessionParse{}, false
	}
	parsed, err := readUsage(a, ref.path)
	if err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			loss.recordTailLoss(ref.harness, parsed.dropped, parsed.unsupported, err)
			return parsed, true
		}
		loss.fail(ref.harness, err)
		return sessionParse{}, false
	}
	loss.recordDropped(ref.harness, parsed.dropped, parsed.unsupported)
	return parsed, true
}

// sessionSubscriptions resolves the proof for one session's harness: Claude
// refines the base proof per worktree repo; Pi uses the base grant.
func sessionSubscriptions(ref sessionRef, proofs *proofCache) subscriptions {
	if ref.harness == claudeAgent.name {
		return proofs.forRef(ref)
	}
	return proofs.base
}

// sessionProof is one session's per-event coverage proof. It consults the
// event's own provider and timestamp, so transcript placement cannot decide
// whether a subscription proves a charge.
func sessionProof(ref sessionRef, subs subscriptions) coveredProof {
	return func(e usageEvent) bool {
		return subs.coverage(ref.harness, e.provider, e.at) != coverageNone
	}
}

// sanitizeEvents rejects corrupt token and web-search counts before the fold
// reads them, and discloses corrupt text figures (a non-string Pi child model, a
// non-string Claude service_tier) once per cause. A sentinel model becomes the
// established "unknown" fallback label instead of a garbage name — before any
// warning names it, so a corrupt-count warning never prints the raw sentinel;
// a sentinel tier stays unknown (ratesFor has no match) but is now named
// instead of silent. A corrupt web-search count warns here even when no catalog
// can resolve the event, so a missing catalog never suppresses the disclosure;
// the pricing path warns with identical text, which the per-run dedupe
// collapses to one line. A rejected self-priced Pi cost warns here once per
// cause without mutating the event, so a bad price in a forked transcript is
// disclosed before the duplicate election picks the winning copy unchanged; the
// winner itself still warns at pricing with the same text, which the per-run
// dedupe collapses to one line.
func sanitizeEvents(events []usageEvent, warn warnSink) {
	modelWarned := false
	tierWarned := false
	costWarned := map[string]bool{}
	for i := range events {
		e := &events[i]
		modelWarned = sanitizeEventModel(e, modelWarned, warn)
		sanitizeEventTokens(e, warn)
		tierWarned = sanitizeEventTier(e, tierWarned, warn)
		sanitizeEventCost(e, costWarned, warn)
	}
}

// sanitizeEventModel replaces a corrupt model name with the established
// "unknown" fallback label, warning once per run before any warning names it.
func sanitizeEventModel(e *usageEvent, modelWarned bool, warn warnSink) bool {
	if e.model != unreadableText {
		return modelWarned
	}
	if !modelWarned {
		doWarnSink(warn, fmt.Errorf("model figure unreadable; reported as unknown"))
		modelWarned = true
	}
	e.model = unknownValue
	return modelWarned
}

// sanitizeEventTokens rejects corrupt token counts and discloses a corrupt
// web-search count even when no catalog can price the event.
func sanitizeEventTokens(e *usageEvent, warn warnSink) {
	e.tokens = sanitizeTokens(e.tokens, e.model, warn)
	if cause := invalidCount(e.webSearch); cause != "" {
		doWarnSink(warn, countCauseError(e.model, "webSearch", cause))
	}
}

// sanitizeEventTier names a corrupt service_tier once per run; it stays
// unknown because ratesFor has no match for it.
func sanitizeEventTier(e *usageEvent, tierWarned bool, warn warnSink) bool {
	if e.tier == unreadableText && !tierWarned {
		doWarnSink(warn, fmt.Errorf("service_tier figure unreadable; tier stays unknown"))
		tierWarned = true
	}
	return tierWarned
}

// sanitizeEventCost warns once per cause about a corrupt self-priced cost
// without mutating the event, so the duplicate election keeps the winning copy.
func sanitizeEventCost(e *usageEvent, costWarned map[string]bool, warn warnSink) {
	if e.cost == nil {
		return
	}
	if cause := invalidCost(*e.cost); cause != "" && !costWarned[cause] {
		costWarned[cause] = true
		doWarnSink(warn, fmt.Errorf("cost.total: %s", cause))
	}
}

func foldParsedSessions(u *worktreeUsage, sessions []parsedSession, now time.Time, warn warnSink) {
	lists := make([][]usageEvent, len(sessions))
	proofs := make([]coveredProof, len(sessions))
	for i, s := range sessions {
		lists[i] = s.events
		proofs[i] = s.proof
	}
	// Spans fold the original events; only the billed winners carry cost. This
	// keeps activity time independent of the session order and of what dedupe
	// removes from a shared transcript.
	best := bestScores(lists, proofs)
	// Dedupe mutates best as it awards slots; read the disputed verdict first.
	disputed := disputedKeys(best)
	billed := dedupeMasks(lists, best)
	for i, s := range sessions {
		price := s.price
		if len(disputed) > 0 {
			price = meterDisputed(price, disputed, warn)
		}
		u.addBilledSession(s.harness, s.events, s.activity, billed[i], price, now)
	}
}

// meterDisputed overrides a subscription-substituted $0 with the list price for
// billed copies of a disputed key (one with a metered twin): the metered copy
// proves the charge, so coverage cannot erase it. The override fires one warning
// per run disclosing the conflict instead of deleting the figure.
func meterDisputed(price func(usageEvent) pricedEvent, disputed map[string]bool, warn warnSink) func(usageEvent) pricedEvent {
	warned := false
	return func(e usageEvent) pricedEvent {
		p := price(e)
		if p.total.subscription && disputed[e.key] {
			p.total.billed = p.total.list
			p.total.subscription = false
			for i := range p.tiers {
				if p.tiers[i].subscription {
					p.tiers[i].billed = p.tiers[i].list
					p.tiers[i].subscription = false
				}
			}
			if !warned {
				doWarnSink(warn, fmt.Errorf("same turn exists under a subscription and outside it; billed at list price"))
				warned = true
			}
		}
		return p
	}
}

// usageLossErr reports the session loss that makes a total partial: an
// unreadable session, unreadable records, or both. One worktree-level warning
// keeps a read-only run alive while still disclosing the loss, including the
// record types a readable unknown-type-with-usage record named.
func usageLossErr(causes []error, dropped int, unsupported map[string]int) error {
	switch {
	case len(causes) > 0 && dropped > 0:
		return fmt.Errorf("%w; %s", errors.Join(causes...), droppedRecordsMsg(dropped, unsupported))
	case len(causes) > 0:
		return errors.Join(causes...)
	case dropped > 0:
		return fmt.Errorf("%s", droppedRecordsMsg(dropped, unsupported))
	default:
		return nil
	}
}

// droppedRecordsMsg is the shared dropped-record disclosure: the count, the
// usual causes, any unsupported record types named by the parsers, and the
// partial tail.
func droppedRecordsMsg(dropped int, unsupported map[string]int) string {
	msg := fmt.Sprintf("%d session record(s) dropped (bad JSON, timestamp, or oversized)", dropped)
	if len(unsupported) > 0 {
		types := slices.Sorted(maps.Keys(unsupported))
		if types = slices.DeleteFunc(types, func(t string) bool { return t == "" }); len(types) > 0 {
			msg += "; unsupported record type(s): " + strings.Join(types, ", ")
		}
	}
	return msg + "; totals are partial"
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

// markTailPartial taints a harness partial after record-level tail loss without
// blanking its measured cost: the completed records survived, so only the lost
// tail makes the total inexact.
func markTailPartial(u *worktreeUsage, tailLost map[string]bool) {
	if len(tailLost) == 0 {
		return
	}
	u.partial = true
	for name := range tailLost {
		h, ok := u.harnesses[name]
		if !ok {
			continue
		}
		h.partial = true
		for _, m := range h.models {
			m.partial = true
		}
	}
}

func readUsage(a agent, path string) (sessionParse, error) {
	f, err := openRegularFile(path)
	if err != nil {
		return sessionParse{}, err
	}
	defer func() { _ = f.Close() }()
	return a.parseUsage(bufio.NewReader(f))
}

// usageBudget caps the session bytes one run reads, so a pathological transcript
// cannot balloon a status run. A nil budget disables the cap entirely. Once the
// cumulative cap is spent, further sessions are skipped whole and reported as
// lost, so totals stay partial instead of reading as complete.
type usageBudget struct {
	remaining atomic.Int64
}

func newUsageBudget(limit int64) *usageBudget {
	b := &usageBudget{}
	b.remaining.Store(limit)
	return b
}

// allow reserves n bytes if they fit the remaining budget, reporting whether the
// reservation succeeded. It never makes remaining negative: a session larger
// than what is left is refused whole instead of partially read. It is atomic so
// parallel worktree folds share one per-run cap safely.
func (b *usageBudget) allow(n int64) bool {
	for {
		rem := b.remaining.Load()
		if rem < n {
			return false
		}
		if b.remaining.CompareAndSwap(rem, rem-n) {
			return true
		}
	}
}

const (
	defaultUsageByteBudget int64 = 1 << 30
	usageByteBudgetEnv           = "WTM_USAGE_BYTE_BUDGET"
	statusSkipUsageEnv           = "WTM_STATUS_SKIP_USAGE"
)

// errBudgetSkipped is the one over-budget cause. It carries no path so
// identical skips collapse to a single disclosed cause per run.
var errBudgetSkipped = errors.New("session exceeds the usage byte budget; skipped")

// usageBudgetFromEnv builds the run budget from WTM_USAGE_BYTE_BUDGET, falling
// back to the default when unset. A malformed value warns through the progress
// handle so the disclosure survives the renderer; an explicit 0 disables the
// cap (nil budget).
func usageBudgetFromEnv(p *progress) *usageBudget {
	raw := os.Getenv(usageByteBudgetEnv)
	if raw == "" {
		return newUsageBudget(defaultUsageByteBudget)
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		warnProgress(p, "%s %q is not a non-negative integer; using default %d", usageByteBudgetEnv, raw, defaultUsageByteBudget)
		return newUsageBudget(defaultUsageByteBudget)
	}
	if n == 0 {
		return nil
	}
	return newUsageBudget(n)
}

// statusSkipUsage reports whether WTM_STATUS_SKIP_USAGE=1 opts out of usage
// loading entirely, so status skips the catalog, subscriptions and session
// parses.
func statusSkipUsage() bool {
	return os.Getenv(statusSkipUsageEnv) == "1"
}

// budgetAllows reports whether the session file fits the remaining budget. When
// the stat fails the read proceeds so readUsage reports the real open error.
func budgetAllows(b *usageBudget, path string) bool {
	if b == nil {
		return true
	}
	info, err := os.Stat(path)
	if err != nil {
		return true
	}
	return b.allow(info.Size())
}

// runSessionDriver is the shared session-parse loop: it reads records, folds
// each into the caller's consume step, and maps the outcome. A clean EOF
// finishes; a cut final record (io.ErrUnexpectedEOF) keeps the completed
// records and finishes with the tail loss; any other read failure drops the
// whole session so a partial result never reads as complete.
func runSessionDriver(r *bufio.Reader, consume func(line []byte, oversized bool, prefix []byte), finish func() sessionParse) (sessionParse, error) {
	for {
		line, oversized, prefix, rerr := sessionRecord(r)
		err := sessionRecordErr(line, oversized, rerr)
		consume(line, oversized, prefix)
		if err == io.EOF {
			return finish(), nil
		}
		if err != nil {
			if errors.Is(err, io.ErrUnexpectedEOF) {
				// A killed writer cut the final record mid-line. The completed
				// records survive; the tail is a record-level loss the caller
				// taints, instead of dropping the whole session.
				return finish(), err
			}
			// One error policy: any other read failure drops the session and
			// taints the total.
			return sessionParse{}, err
		}
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

// hasHarness reports whether any part owns a session from the named harness.
// Status uses it to load the catalog only when it can be needed, so a Pi-only
// repo never touches the network.
func (p worktreePartitions) hasHarness(name string) bool {
	for _, refs := range p {
		for _, r := range refs {
			if r.harness == name {
				return true
			}
		}
	}
	return false
}
