package core

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/ui"
)

// naming.go — the single source of truth for branch and worktree directory
// names. Branches keep their semantic, taxonomy-prefixed identity (feature/x);
// directories are project-prefixed slugs placed next to the current worktree,
// so a worktree is never nested inside another and never inherits the branch's
// slashes.

// MaxBranchLen follows the 80-char convention that keeps branch names within
// Windows path limits and readable in CI.
const MaxBranchLen = 80

// TempBranchPrefix marks an ephemeral branch that gc may reclaim once idle.
const TempBranchPrefix = "tmp/"

// branchPrefixes is the taxonomy the warn-only convention check recognizes.
var branchPrefixes = []string{
	TempBranchPrefix,
	"feature/", "fix/", "hotfix/", "chore/", "refactor/",
	"docs/", "test/", "perf/", "ci/", "release/",
}

func cleanSegments(segs []string, raw string) ([]string, error) {
	for i, s := range segs {
		s = trimDots(trimHyphens(s))
		if s == "" || strings.HasSuffix(s, ".lock") {
			return nil, fmt.Errorf("branch %q is not a valid git ref", raw)
		}
		segs[i] = s
	}
	return segs, nil
}

// NormalizeBranch canonicalizes user input to a valid, conventional git branch:
// lowercase, hyphen-separated segments, slashes preserved as taxonomy
// separators. Invalid ref characters are rejected rather than silently dropped.
// Lowercasing collides names that differ only by case (Feature/X vs feature/x
// resolve to the same branch), so the normalized form is the branch identity.
func NormalizeBranch(raw string) (string, error) {
	b := strings.ToLower(strings.TrimSpace(raw))
	if b == "" {
		return "", errors.New("branch name is required")
	}
	b = strings.NewReplacer("_", "-", " ", "-").Replace(b)
	if bad, ok := badBranchRune(b); !ok {
		return "", fmt.Errorf("branch %q contains unsupported character %q", raw, bad)
	}
	// @{ opens revision syntax (HEAD@{yesterday}); git rejects it in refnames.
	if strings.Contains(b, "@{") {
		return "", fmt.Errorf("branch %q is not a valid git ref: @{ is revision syntax", raw)
	}
	// '@' alone is not a branch name to git (it resolves the HEAD shorthand).
	if b == "@" {
		return "", fmt.Errorf("branch %q is not a valid git ref: @ has no name", raw)
	}
	segs, err := cleanSegments(strings.Split(b, "/"), raw)
	if err != nil {
		return "", err
	}
	b = strings.Join(segs, "/")
	if len(b) > MaxBranchLen {
		return "", fmt.Errorf("branch %q exceeds %d characters", b, MaxBranchLen)
	}
	return b, nil
}

// badBranchRune returns the first character outside the allowed git-ref
// alphabet (a-z, 0-9, '-', '.', '/', '@'), if any. '@' alone is legal
// (feature/@scope/auth); the '@{' revision sequence is rejected separately in
// NormalizeBranch.
func badBranchRune(b string) (rune, bool) {
	for _, r := range b {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '.', r == '/', r == '@':
		default:
			return r, false
		}
	}
	return 0, true
}

// trimHyphens collapses runs of '-' and trims the ends; a leading run cannot
// survive because git refs may not start a path component with '-'.
func trimHyphens(s string) string { return trimRuns(s, '-') }

// trimDots collapses runs of '.' and trims the ends, so "a..b" cannot produce
// the invalid ".." sequence.
func trimDots(s string) string { return trimRuns(s, '.') }

func trimRuns(s string, sep rune) string {
	var out strings.Builder
	prev := false
	for _, r := range s {
		if r == sep {
			if prev || out.Len() == 0 {
				continue
			}
			prev = true
		} else {
			prev = false
		}
		out.WriteRune(r)
	}
	return strings.TrimRight(out.String(), string(sep))
}

// TempBranch prefixes a requested name with tmp/ unless it is already
// prefixed, so `add --temp` is idempotent on an already-temp name. The prefix
// is applied before the length check, since it is part of the branch identity.
func TempBranch(raw string) (string, error) {
	b, err := NormalizeBranch(raw)
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(b, TempBranchPrefix) {
		b = TempBranchPrefix + b
	}
	if len(b) > MaxBranchLen {
		return "", fmt.Errorf("branch %q exceeds %d characters", b, MaxBranchLen)
	}
	return b, nil
}

// Slugify flattens a branch into a filesystem-safe directory component: every
// separator becomes '-', so feature/login -> feature-login.
func Slugify(branch string) string {
	var out strings.Builder
	prev := false
	for _, r := range strings.ToLower(branch) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			out.WriteRune(r)
			prev = false
			continue
		}
		if !prev && out.Len() > 0 {
			out.WriteByte('-')
			prev = true
		}
	}
	return strings.Trim(out.String(), "-")
}

// ProjectName is the stable prefix shared by every sibling worktree: the main
// worktree's directory name. Deriving it from the main worktree (not the
// current one) keeps names stable when a worktree is created from a linked one.
func ProjectName(wts []git.GWorktree) string {
	if len(wts) == 0 {
		return "worktree"
	}
	base := filepath.Base(wts[0].Path)
	if base == "" || base == "." || base == string(filepath.Separator) {
		return "worktree"
	}
	return base
}

// DeriveDirName builds the sibling directory name from the project prefix and
// the branch slug: repo + feature/login -> repo-feature-login.
func DeriveDirName(project, branch string) string {
	return project + "-" + Slugify(branch)
}

// NestedIn reports the existing worktree that contains target (or equals it).
// worktrees must be siblings: a nested checkout makes IDE watchers, linters and
// test runners double-scan, and breaks discovery assumptions.
func NestedIn(target string, wts []git.GWorktree) (string, bool) {
	for _, w := range wts {
		p := fs.Canonical(w.Path)
		if target == p || strings.HasPrefix(target, p+string(filepath.Separator)) {
			return p, true
		}
	}
	return "", false
}

// BranchChoice is the resolved identity of the requested branch before any
// write: exists means attach instead of create.
type BranchChoice struct {
	Name   string
	Exists bool
}

// BranchLookupMode selects the candidate spellings a command accepts. add
// adopts before normalization so git-valid stored names stay untouched; address
// mode adds the bare-name tmp/ alias shared by delete and promote.
type BranchLookupMode int

const (
	BranchForAdd BranchLookupMode = iota
	BranchForTempAdd
	BranchForAddress
)

// ResolveLocalBranch resolves raw to a stored branch or to the caller's create
// name. Each candidate tries Git's stored spelling, then one unique case-fold.
// The first existing candidate wins. A miss returns the mode's fallback name so
// add can create it and address commands keep their not-found errors.
func ResolveLocalBranch(refs map[string]bool, raw string, mode BranchLookupMode) (BranchChoice, error) {
	switch mode {
	case BranchForTempAdd:
		return resolveTempBranch(refs, raw)
	case BranchForAddress:
		return resolveAddressBranch(refs, raw)
	default:
		return resolveCreateBranch(refs, raw)
	}
}

func resolveTempBranch(refs map[string]bool, raw string) (BranchChoice, error) {
	name, err := TempBranch(raw)
	if err != nil {
		return BranchChoice{}, err
	}
	return MatchLocalBranch(refs, name)
}

func resolveAddressBranch(refs map[string]bool, raw string) (BranchChoice, error) {
	choice, err := firstStoredBranch(refs, addressCandidates(raw))
	if err != nil || choice.Exists {
		return choice, err
	}
	return BranchChoice{Name: strings.TrimSpace(raw)}, nil
}

func resolveCreateBranch(refs map[string]bool, raw string) (BranchChoice, error) {
	exact := strings.TrimSpace(raw)
	if choice, err := MatchLocalBranch(refs, exact); err != nil || choice.Exists {
		return choice, err
	}
	normalized, err := NormalizeBranch(raw)
	if err != nil {
		return BranchChoice{}, err
	}
	return MatchLocalBranch(refs, normalized)
}

func firstStoredBranch(refs map[string]bool, candidates []string) (BranchChoice, error) {
	for _, name := range candidates {
		choice, err := MatchLocalBranch(refs, name)
		if err != nil || choice.Exists {
			return choice, err
		}
	}
	return BranchChoice{}, nil
}

// addressCandidates keeps delete's precedence: raw, normalized, then the tmp/
// alias. Invalid normalized candidates are skipped because the raw spelling may
// still be a valid stored ref. Option-like raw names never reach git as options.
func addressCandidates(raw string) []string {
	var out []string
	add := func(name string) {
		if name == "" || name[0] == '-' || slices.Contains(out, name) {
			return
		}
		out = append(out, name)
	}
	add(strings.TrimSpace(raw))
	if name, err := NormalizeBranch(raw); err == nil {
		add(name)
	}
	if name, err := TempBranch(raw); err == nil {
		add(name)
	}
	return out
}

// StoredBranchName maps name to Git's stored spelling when it exists, else
// returns name unchanged. Used where two names must compare by identity, such
// as the default-branch refusal on case-insensitive filesystems.
func StoredBranchName(refs map[string]bool, name string) string {
	choice, err := MatchLocalBranch(refs, name)
	if err == nil && choice.Exists {
		return choice.Name
	}
	return name
}

// MatchLocalBranch finds the stored branch for name: exact, else a unique case
// fold. A miss returns name with exists=false, ready for the create path.
func MatchLocalBranch(refs map[string]bool, name string) (BranchChoice, error) {
	if refs[name] {
		return BranchChoice{Name: name, Exists: true}, nil
	}
	found := BranchChoice{Name: name}
	for stored := range refs {
		if !strings.EqualFold(stored, name) {
			continue
		}
		if found.Exists {
			return BranchChoice{}, fmt.Errorf("branch %q %w", name, ErrAmbiguousBranch)
		}
		found = BranchChoice{Name: stored, Exists: true}
	}
	return found, nil
}

// WarnBranchConvention discloses a branch without a recognized type prefix.
// A warning, not a rejection: conventions drive CI routing and search, but the
// user may skip them. Matching folds case, so a stored Feature/X still counts.
func WarnBranchConvention(branch string) {
	for _, p := range branchPrefixes {
		if strings.HasPrefix(strings.ToLower(branch), p) {
			return
		}
	}
	ui.Warnf("branch %q has no type prefix (e.g. feature/…); continuing", branch)
}
