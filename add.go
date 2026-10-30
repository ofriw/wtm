package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// add.go — create a worktree, then seed it with the source's agent harness and
// ChunkHound workspace.

type addOptions struct {
	path    string // explicit directory override; empty derives a sibling path
	start   string
	from    string
	noIndex bool
	temp    bool   // ephemeral: branch as tmp/<name>, recorded for gc to reclaim
	ttl     string // idle window for a temp worktree; implies temp
}

type addResult struct {
	Path      string  `json:"path"`
	Branch    string  `json:"branch"`
	Base      string  `json:"base"`
	Source    string  `json:"source"`
	Indexed   bool    `json:"indexed"`
	Temp      bool    `json:"temp"`
	TTL       string  `json:"ttl,omitempty"`
	ExpiresAt *string `json:"expiresAt,omitempty"`
}

func cmdAdd(g *globals, args []string) error {
	fs := subFlags("add", g)
	var o addOptions
	bindAddFlags(fs, &o)
	pos, err := parseSub(fs, g, args)
	if err != nil {
		return err
	}
	if len(pos) < 1 || len(pos) > 2 {
		return usagef("add: want <branch> [<start-point>]")
	}
	if len(pos) == 2 {
		o.start = pos[1]
	}
	resolveAddOptions(&o)
	return addWorktree(g, pos[0], o)
}

// resolveAddOptions applies derived flag implications after parsing.
func resolveAddOptions(o *addOptions) {
	// A TTL implies temp so the requested window cannot be silently ignored.
	o.temp = o.temp || o.ttl != ""
}

func bindAddFlags(fs *flag.FlagSet, o *addOptions) {
	fs.StringVar(&o.path, "p", "", "worktree directory (default: sibling of the current worktree)")
	fs.StringVar(&o.path, "path", "", "alias of -p")
	fs.StringVar(&o.from, "from", "", "source worktree to seed from (default: the worktree at the start-point)")
	fs.BoolVar(&o.noIndex, "no-index", false, "skip `chunkhound index` after seeding (also skipped when chunkhound is absent)")
	fs.BoolVar(&o.temp, "temp", false, "ephemeral worktree on a tmp/ branch that gc reclaims once idle")
	fs.StringVar(&o.ttl, "ttl", "", "temp idle window (e.g. 30m, 1h, 7d); implies --temp; temp defaults to 1h")
}

// tempTTL resolves the idle window for an add: zero when not temp, the default
// when temp without --ttl, else the parsed --ttl.
func tempTTL(o addOptions) (time.Duration, error) {
	if !o.temp {
		return 0, nil
	}
	if o.ttl == "" {
		return defaultTempTTL, nil
	}
	return parseTTL(o.ttl)
}

func seedWorktree(src, path string, noIndex bool) (bool, error) {
	if err := copyHarness(src, path); err != nil {
		return false, fmt.Errorf("copy agent harness: %w", err)
	}
	if err := copyChunkHound(src, path); err != nil {
		return false, fmt.Errorf("copy ChunkHound workspace: %w", err)
	}
	if err := rewriteRootGuard(path); err != nil {
		return false, err
	}
	if err := patchDatabasePath(src, path); err != nil {
		return false, err
	}
	return indexSeed(path, noIndex)
}

// indexSeed refreshes the seeded db unless indexing is opted out or chunkhound
// is absent. A missing binary is not an error — ChunkHound is optional — but a
// seeded workspace means the project does use it, so the skip is disclosed.
func indexSeed(path string, noIndex bool) (bool, error) {
	if noIndex {
		return false, nil
	}
	switch err := runIndex(path); {
	case errors.Is(err, errNoChunkHound):
		warnIfChunkHoundExpected(path)
		return false, nil
	case err != nil:
		return false, err
	default:
		return true, nil
	}
}

// warnIfChunkHoundExpected warns that indexing was skipped for a worktree that
// carries a ChunkHound workspace, whose copied db is therefore not refreshed.
func warnIfChunkHoundExpected(path string) {
	if hasChunkHoundWorkspace(path) {
		warnf("chunkhound not on PATH; skipped indexing %s; search results for this worktree may be stale", path)
	}
}

// resolveAddSource picks the worktree to seed from. An explicit --from wins.
// Otherwise the source is the worktree checked out at the start-point, so the
// seeded harness — gitignored db/agent/mcp state git cannot carry — matches the
// tree the new worktree is created from. When no worktree sits at the
// start-point, the ref is checked out nowhere and no faithful index exists, so
// the main worktree is the fallback and the mismatch is disclosed: the copied
// index cannot match, so indexing must re-embed instead of being a no-op.
func resolveAddSource(root string, wts []gworktree, base, from, target string) (string, error) {
	if from != "" {
		src, err := canonicalAbs(from)
		if err != nil {
			return "", err
		}
		if !exists(src) {
			return "", fmt.Errorf("--from %s: not found", src)
		}
		return src, nil
	}
	if src, ok := worktreeAt(root, wts, base, target); ok {
		return src, nil
	}
	warnf("no worktree checked out at %s; seeding from %s; the copied ChunkHound index cannot match and indexing will re-embed", base, canonical(root))
	return canonical(root), nil
}

// worktreeAt returns the existing worktree checked out at base. It prefers the
// worktree whose branch is base's branch, then any worktree at the same commit;
// both require a commit match, so a diverged branch of the same name never
// masquerades as the start-point. target (the checkout being created) is excluded.
func worktreeAt(root string, wts []gworktree, base, target string) (string, bool) {
	commit, err := git(root, "rev-parse", "--verify", "--quiet", base+"^{commit}")
	if err != nil {
		return "", false
	}
	branch := baseBranchName(root, base)
	exact, same := "", ""
	for _, w := range wts {
		p := canonical(w.Path)
		if p == target || w.HEAD != commit {
			continue
		}
		if branch != "" && w.Branch == branch {
			exact = p
			break
		}
		if same == "" {
			same = p
		}
	}
	if exact != "" {
		return exact, true
	}
	return same, same != ""
}

// baseBranchName maps a start-point ref to its short branch name: refs/heads/x
// and refs/remotes/<remote>/x both yield x; a tag or raw commit yields "".
func baseBranchName(root, base string) string {
	ref, err := git(root, "rev-parse", "--symbolic-full-name", base)
	if err != nil {
		return ""
	}
	if b, ok := strings.CutPrefix(ref, "refs/heads/"); ok {
		return b
	}
	if rest, ok := strings.CutPrefix(ref, "refs/remotes/"); ok {
		if _, b, ok := strings.Cut(rest, "/"); ok {
			return b
		}
	}
	return ""
}

// locationBase is where sibling worktrees live: the parent of the worktree the
// user is in when it belongs to the target repo, else the parent of the repo's
// main worktree. Never the cwd itself — a worktree created under the cwd nests
// inside another and breaks IDE watchers and test runners.
func locationBase(root string, wts []gworktree) string {
	fallback := filepath.Dir(canonical(root))
	cwd, err := os.Getwd()
	if err != nil {
		return fallback
	}
	top, err := gitTopLevel(cwd)
	if err != nil {
		return fallback
	}
	top = canonical(top)
	for _, w := range wts {
		if canonical(w.Path) == top {
			return filepath.Dir(top)
		}
	}
	return fallback
}

// resolveAddTarget picks the checkout directory: an explicit --path (guarded
// against nesting), or a sibling named <project>-<branch-slug>.
func resolveAddTarget(root string, wts []gworktree, branch, override string) (string, error) {
	if override != "" {
		target, err := canonicalAbs(override)
		if err != nil {
			return "", err
		}
		if parent, nested := nestedIn(target, wts); nested {
			return "", usagef("add: refusing to create a worktree inside %s; worktrees must be siblings, not nested", parent)
		}
		return target, nil
	}
	// The derived path is guarded like --path: locationBase can return a
	// directory inside another worktree when the current worktree is itself
	// nested, and a sibling must never be nested either.
	target := filepath.Join(locationBase(root, wts), deriveDirName(projectName(wts), branch))
	if parent, nested := nestedIn(target, wts); nested {
		return "", usagef("add: refusing to create a worktree inside %s; worktrees must be siblings, not nested", parent)
	}
	return target, nil
}

// rollbackAdd undoes a failed seed: pristine checkouts are removed, but any
// seeded content (even gitignored, e.g. a copied MCP config) is user-inspectable
// state, so the partial worktree survives and the caller is told to inspect it.
func rollbackAdd(root, path, branch string, seedErr error) error {
	// --ignored keeps the check honest: a copied MCP config is ignored, yet is
	// exactly the seeded state a user must be able to inspect.
	changes, checkErr := git(path, "status", "--porcelain", "--untracked-files=all", "--ignored")
	if checkErr != nil || changes != "" {
		return fmt.Errorf("%w; partial worktree kept at %s (branch %s); inspect it before removing", seedErr, path, branch)
	}
	if cleanup := worktreeRemove(root, path); cleanup != nil {
		return fmt.Errorf("%w; partial worktree kept at %s (branch %s): %v", seedErr, path, branch, cleanup)
	}
	if _, cleanup := git(root, "branch", "-d", branch); cleanup != nil {
		return fmt.Errorf("%w; worktree removed but branch %s remains: %v", seedErr, branch, cleanup)
	}
	return seedErr
}

func reportAddResult(g *globals, res addResult, ttl time.Duration) error {
	if g.json {
		return printJSON(res)
	}
	origin := "local"
	if strings.HasPrefix(res.Base, "refs/remotes/") {
		origin = "remote"
	}
	fmt.Printf("worktree: %s\nbranch:   %s\nbase:     %s (%s)\nsource:   %s\nindexed:  %v\n",
		res.Path, res.Branch, res.Base, origin, res.Source, res.Indexed)
	if res.Temp {
		fmt.Printf("temp:     true (expires in %s)\n", compactWindow(ttl))
	}
	return nil
}

type addParams struct {
	root, src, path, branch, base string
	attach                        bool // branch exists and will be attached
	temp                          bool
	ttl                           time.Duration
}

func prepareAdd(g *globals, branchArg string, o addOptions) (addParams, error) {
	root, err := resolveRoot(g)
	if err != nil {
		return addParams{}, err
	}
	ttl, err := tempTTL(o)
	if err != nil {
		return addParams{}, usagef("add: %v", err)
	}
	branch, err := resolveAddBranch(root, branchArg, o.temp)
	if err != nil {
		return addParams{}, err
	}
	warnBranchConvention(branch.name)
	return resolveAddParams(addParams{
		root:   root,
		branch: branch.name,
		attach: branch.exists,
		temp:   o.temp,
		ttl:    ttl,
	}, o)
}

func resolveAddParams(p addParams, o addOptions) (addParams, error) {
	wts, err := worktrees(p.root)
	if err != nil {
		return addParams{}, err
	}
	if p.attach {
		// One branch cannot have two checkouts; name the blocker and a repair.
		for _, w := range wts {
			if w.Branch == p.branch {
				return addParams{}, occupiedErr(p.branch, w)
			}
		}
	}
	if p.path, err = resolveAddTarget(p.root, wts, p.branch, o.path); err != nil {
		return addParams{}, err
	}
	if err = resolveAddBase(&p, o.start); err != nil {
		return addParams{}, err
	}
	if p.src, err = resolveAddSource(p.root, wts, p.base, o.from, p.path); err != nil {
		return addParams{}, err
	}
	return p, nil
}

// resolveAddBase picks the base ref: an attachment's own branch ref (guarded
// against a contradictory start-point), else the requested or default
// start-point.
func resolveAddBase(p *addParams, start string) error {
	if !p.attach {
		p.base = start
		if p.base == "" {
			p.base = baseRefFor(p.root)
		}
		return nil
	}
	p.base = "refs/heads/" + p.branch
	if start == "" {
		return nil
	}
	return verifyAttachStart(p.root, p.branch, start)
}

// verifyAttachStart rejects a start-point that disagrees with the existing
// branch tip: attachment never rewinds. An unresolvable ref is a usage error;
// a valid ref that conflicts with repository state is an operational error.
func verifyAttachStart(root, branch, start string) error {
	tip, err := git(root, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return fmt.Errorf("add: cannot resolve branch %q: %w", branch, err)
	}
	commit, err := git(root, "rev-parse", "--verify", "--quiet", start+"^{commit}")
	if err != nil {
		return usagef("add: start-point %q is not a commit", start)
	}
	if commit != tip {
		return fmt.Errorf("add: start-point %q (%s) does not match existing branch %q (%s); refusing to attach",
			start, commit, branch, tip)
	}
	return nil
}

// occupiedErr names the blocking worktree and a repair the tool itself allows.
func occupiedErr(branch string, w gworktree) error {
	if w.Prunable || !exists(w.Path) {
		return fmt.Errorf("add: branch %q is already checked out at %s (missing; run git worktree prune)", branch, w.Path)
	}
	if w.Main {
		return fmt.Errorf("add: branch %q is already checked out at the main worktree %s; switch it to another branch first", branch, w.Path)
	}
	return fmt.Errorf("add: branch %q is already checked out at %s; remove that worktree first", branch, w.Path)
}

// resolveAddBranch adopts an existing local branch with git's stored spelling,
// so normalization never duplicates Feature/X as feature/x.
func resolveAddBranch(root, raw string, temp bool) (branchChoice, error) {
	refs, err := localBranches(root)
	if err != nil {
		return branchChoice{}, err
	}
	branch, err := requestedBranch(refs, raw, temp)
	if err != nil {
		return branchChoice{}, usagef("add: %v", err)
	}
	return branch, nil
}

// addWorktree creates or attaches the branch's sibling checkout, then
// replicates the source's harness. Idempotent by construction: an existing
// path, or a branch that is checked out elsewhere, fails before anything is
// written; an existing free branch is attached, never recreated.
func addWorktree(g *globals, branchArg string, o addOptions) error {
	p, err := prepareAdd(g, branchArg, o)
	if err != nil {
		return err
	}
	if err := worktreeAdd(p.root, p.path, p.branch, p.base, p.attach); err != nil {
		return err
	}
	indexed, err := seedWorktree(p.src, p.path, o.noIndex)
	if err != nil {
		return rollbackAdd(p.root, p.path, p.branch, err)
	}
	res := addResult{Path: p.path, Branch: p.branch, Base: p.base, Source: p.src, Indexed: indexed}
	trackAddTemp(p, &res)
	return reportAddResult(g, res, p.ttl)
}

// Record only after seeding: failed seeds or records leave durable checkouts.
// expiresAt equals the effective deadline only because seeding just finished,
// so lastUsed cannot exceed createdAt; status recomputes via tempDeadline,
// which stays the single source of truth for the deadline.
func trackAddTemp(p addParams, res *addResult) {
	if !p.temp {
		return
	}
	rec, err := recordTemp(p.path, p.branch, p.ttl)
	if err != nil {
		warnf("%s is not tracked as temp and will persist: %v", p.path, err)
		return
	}
	expires := formatTempDeadline(rec.CreatedAt.Add(p.ttl))
	res.Temp, res.TTL, res.ExpiresAt = true, p.ttl.String(), &expires
}

// recordTemp writes the store record for a freshly seeded temp worktree, keyed
// by canonical path so lookups from git, Pi and the filesystem compare equal.
func recordTemp(path, branch string, ttl time.Duration) (tempRecord, error) {
	return addTempRecord(path, tempRecord{CreatedAt: time.Now(), TTL: ttl.String(), Branch: branch})
}
