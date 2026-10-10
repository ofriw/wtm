package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// temp.go — the ephemeral-worktree store: which checkouts reclaim themselves
// and after how long. It is the only file that knows ~/.wtm/temp.json.

// defaultTempTTL is the idle window a temp worktree gets when `add --temp`
// carries no --ttl.
const defaultTempTTL = time.Hour

// Sanity bound on user input; 100 years is well within time.Time's range.
const maxTempTTL = 100 * 365 * 24 * time.Hour

// tempRecord is one tracked ephemeral checkout. The effective deadline is
// max(CreatedAt, LastUsed)+TTL, so the TTL is an idle window, not a fixed age.
type tempRecord struct {
	CreatedAt time.Time `json:"createdAt"`
	TTL       string    `json:"ttl"`
	Branch    string    `json:"branch"`
	Identity  string    `json:"identity"`
}

// tempStore maps a canonical checkout path to its record.
type tempStore map[string]tempRecord

func tempPath() (string, error) {
	dir, err := wtmDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "temp.json"), nil
}

func tempLockPath() (string, error) {
	dir, err := wtmDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "temp.lock"), nil
}

// parseTTL parses a positive idle window: a Go duration (30m, 1h, 24h) or whole
// days (7d), matching the settings file's day syntax.
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, fmt.Errorf("ttl is required")
	}
	if days, ok := strings.CutSuffix(s, "d"); ok {
		return parseTTLDays(s, days)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl %q: %w", s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("ttl %q: must be positive", s)
	}
	if d > maxTempTTL {
		return 0, fmt.Errorf("ttl %q: exceeds the maximum %s", s, maxTempTTL)
	}
	return d, nil
}

func parseTTLDays(s, days string) (time.Duration, error) {
	n, err := strconv.Atoi(days)
	if err != nil || n < 1 || n > int(maxTempTTL/(24*time.Hour)) {
		return 0, fmt.Errorf("ttl %q: want a positive duration (30m, 1h) or whole days (7d)", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// tempExpired reports whether a temp worktree has been idle longer than ttl.
// Anchoring on the newer of creation and last use makes any edit, commit or
// agent session push the deadline forward.
func tempDeadline(createdAt, lastUsed time.Time, ttl time.Duration) time.Time {
	if lastUsed.After(createdAt) {
		createdAt = lastUsed
	}
	return createdAt.Add(ttl)
}

// Preserve supported fractional TTLs in all machine-readable deadlines.
func formatTempDeadline(deadline time.Time) string {
	return deadline.UTC().Format(time.RFC3339Nano)
}

func tempExpired(createdAt, lastUsed time.Time, ttl time.Duration, now time.Time) bool {
	return now.After(tempDeadline(createdAt, lastUsed, ttl))
}

// remainingWindowLabel renders a temp deadline for display: "expired" once it
// has passed, else a compact duration. It is the single source of truth for the
// status TEMP cell and the gc picker's window column. The strict After matches
// tempExpired (the candidate predicate), so STATUS and TEMP never disagree.
func remainingWindowLabel(expiresAt, now time.Time) string {
	if now.After(expiresAt) {
		return "expired"
	}
	return compactWindow(expiresAt.Sub(now))
}

// compactWindow renders d at the coarsest whole unit that fits: 3d, 2h, 1h30m, 45m.
// Negative durations clamp to "0s"; callers should prevent this, but the guard
// keeps display correct if a future caller passes negative input.
func compactWindow(d time.Duration) string {
	switch {
	// Negative durations should not reach here (remainingWindowLabel guards
	// the precondition), but surfacing a negative label is more honest than
	// clamping to 0s and masking a caller bug.
	case d >= 24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	case d >= time.Hour:
		h := int(d.Hours())
		if m := int(d.Minutes()) % 60; m > 0 {
			return fmt.Sprintf("%dh%dm", h, m)
		}
		return fmt.Sprintf("%dh", h)
	case d >= time.Minute:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// readTempStoreUnlocked is used by mutations already holding the registry lock.
// OS-specific readers decide whether replacement requires locking. Missing files
// are empty stores; malformed files remain explicit errors.
func readTempStoreUnlocked() (tempStore, error) {
	p, err := tempPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return tempStore{}, nil
	}
	if err != nil {
		return nil, err
	}
	return decodeTempStore(data, p)
}

// Mutation must not turn an unreadable record into proven absence.
func decodeTempStore(data []byte, p string) (tempStore, error) {
	var store tempStore
	if err := json.Unmarshal(data, &store); err != nil {
		return nil, fmt.Errorf("%s: %w", p, err)
	}
	if store == nil {
		return nil, fmt.Errorf("%s: want a registry object", p)
	}
	for path, rec := range store {
		if err := validateTempRecord(rec); err != nil {
			return nil, fmt.Errorf("%s: record for %s: %w", p, path, err)
		}
	}
	return store, nil
}

func validateTempRecord(rec tempRecord) error {
	if _, err := parseTTL(rec.TTL); err != nil {
		return err
	}
	if rec.CreatedAt.IsZero() || rec.Branch == "" {
		return fmt.Errorf("creation time and branch are required")
	}
	if !validTempIdentity(rec.Identity) {
		return fmt.Errorf("invalid temp identity")
	}
	return nil
}

func withLockedTempStore(fn func(tempStore, string) error) error {
	p, err := tempPath()
	if err != nil {
		return err
	}
	lock, err := tempLockPath()
	if err != nil {
		return err
	}
	return withFileLock(lock, func() error {
		store, err := readTempStoreUnlocked()
		if err != nil {
			return err
		}
		return fn(store, p)
	})
}

func writeTempStore(path string, store tempStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'), 0o600)
}

func validTempIdentity(identity string) bool {
	data, err := hex.DecodeString(identity)
	return err == nil && len(data) == 32 && hex.EncodeToString(data) == identity
}

func newTempIdentity() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func tempIdentityPath(path string) (string, error) {
	dir, err := gitPrivateDir(path)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wtm-temp-identity"), nil
}

func installTempIdentity(path string, rec tempRecord) error {
	branch, err := gitCurrentBranch(path)
	if err != nil {
		return err
	}
	if branch != rec.Branch {
		return fmt.Errorf("temp branch changed: want %s, got %s", rec.Branch, branch)
	}
	p, err := tempIdentityPath(path)
	if err != nil {
		return err
	}
	return writeFileAtomic(p, []byte(rec.Identity), 0o600)
}

var errTempMetadataChanged = errors.New("temp metadata changed")

func verifyTempIdentity(path string, rec tempRecord) error {
	if err := validateTempRecord(rec); err != nil {
		return err
	}
	p, err := tempIdentityPath(path)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: missing identity for %s", errTempMetadataChanged, path)
		}
		return err
	}
	if string(data) != rec.Identity {
		return fmt.Errorf("%w: identity for %s", errTempMetadataChanged, path)
	}
	return verifyTempBranch(path, rec.Branch)
}

func verifyTempBranch(path, expected string) error {
	branch, err := gitCurrentBranch(path)
	if err != nil {
		return err
	}
	if branch != expected {
		return fmt.Errorf("%w: branch for %s: want %s, got %s", errTempMetadataChanged, path, expected, branch)
	}
	return nil
}

func verifyTempSnapshot(store tempStore, w worktree) error {
	rec, ok := store[w.Path]
	if !ok {
		return fmt.Errorf("%w: %s", errTempPromoted, w.Path)
	}
	if rec.Identity != w.TempIdentity || rec.Branch != w.Branch {
		return fmt.Errorf("%w: identity or branch changed for %s", errTempMetadataChanged, w.Path)
	}
	return verifyTempIdentity(w.Path, rec)
}

var (
	errTempActive     = errors.New("temp worktree is now ACTIVE")
	errTempUnverified = errors.New("unverified agent session is newer than known activity")
	errTempPromoted   = errors.New("temp record was promoted or removed")
)

// Idle GC refreshes activity under the lock; explicit removals bypass idleness.
// Unverified sessions block only when newer than the candidate's known newest
// activity — only such a file could be its hidden recent use. Older ones warn.
func verifyTempIdle(store tempStore, w worktree, now time.Time) (stale []string, err error) {
	// The record exists: verifyTempRemoval checked the snapshot before this runs.
	rec := store[w.Path]
	ttl, err := parseTTL(rec.TTL)
	if err != nil {
		return nil, err
	}
	last, unverified, err := freshWorktreeActivity(w.Path, now)
	if err != nil {
		return nil, fmt.Errorf("refresh worktree activity: %w", err)
	}
	if !tempExpired(rec.CreatedAt, last, ttl, now) {
		return nil, errTempActive
	}
	return splitUnverifiedSessions(unverified, last)
}

// splitUnverifiedSessions separates unverified sessions: one newer than known
// activity could be the candidate's hidden recent use and blocks removal;
// older ones cannot change the verdict and are returned for the caller to
// disclose. temp.go owns the store, not the terminal, so no warnings here.
func splitUnverifiedSessions(unverified []unverifiedSession, knownNewest time.Time) ([]string, error) {
	var stale []string
	for _, u := range unverified {
		if u.blocksRemoval(knownNewest) {
			return nil, fmt.Errorf("%w: %s", errTempUnverified, u.Path)
		}
		stale = append(stale, u.Path)
	}
	return stale, nil
}

// Registry permission cannot change between verification and checkout removal.
// Removed stays true when the checkout is gone but registry persistence fails.
// removeTempWorktreeIf holds the exclusive registry lock for the entire
// idle verification + checkout removal sequence.  Verification refreshes
// external state (Git activity, agent sessions) and removal runs a blocking
// git command; callers should not expect the lock to be short-lived.
func removeTempWorktreeIf(w worktree, idleOnly bool, now time.Time) (removed bool, stale []string, err error) {
	err = withLockedTempStore(func(store tempStore, registry string) error {
		stale, err = verifyTempRemoval(store, w, idleOnly, now)
		if err != nil {
			return err
		}
		if err := removeTempCheckout(w.Path); err != nil {
			return err
		}
		removed = true
		return persistTempRemoval(store, registry, w.Path)
	})
	return removed, stale, err
}

func verifyTempRemoval(store tempStore, w worktree, idleOnly bool, now time.Time) ([]string, error) {
	if err := verifyTempSnapshot(store, w); err != nil {
		return nil, err
	}
	if idleOnly {
		return verifyTempIdle(store, w, now)
	}
	return nil, nil
}

func persistTempRemoval(store tempStore, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return fmt.Errorf("clear temp record for %s: %w", path, err)
	}
	return nil
}

func deleteStoredTempRecord(store tempStore, registry, path string) error {
	delete(store, path)
	return writeTempStore(registry, store)
}

func removeTempCheckout(path string) error {
	root, err := repoRoot(path)
	if err != nil {
		return err
	}
	return worktreeRemove(root, path)
}

func prepareTempRecord(rec tempRecord) (tempRecord, error) {
	if rec.Identity == "" {
		identity, err := newTempIdentity()
		if err != nil {
			return rec, err
		}
		rec.Identity = identity
	}
	return rec, validateTempRecord(rec)
}

func addTempRecord(path string, rec tempRecord) (tempRecord, error) {
	rec, err := prepareTempRecord(rec)
	if err != nil {
		return tempRecord{}, err
	}
	err = withLockedTempStore(func(store tempStore, p string) error {
		if err := installTempIdentity(path, rec); err != nil {
			return err
		}
		store[canonical(path)] = rec
		return writeTempStore(p, store)
	})
	if err != nil {
		return tempRecord{}, err
	}
	return rec, nil
}

// Permanent removal also holds the registry lock: a stale record grants no
// temp permission, but a newly registered checkout must never be removed.
func removePermanentWorktree(root, path string) (bool, error) {
	removed := false
	err := withLockedTempStore(func(store tempStore, registry string) error {
		if err := clearStaleTempTarget(store, registry, canonical(path)); err != nil {
			return err
		}
		err := worktreeRemoveRecovering(root, path)
		removed = err == nil
		return err
	})
	return removed, err
}

func clearStaleTempTarget(store tempStore, registry, path string) error {
	rec, ok := store[path]
	if !ok {
		return nil
	}
	err := inspectTempMetadata(path, rec)
	if err == nil {
		return fmt.Errorf("temp record appeared for %s since discovery; re-run gc", path)
	}
	if !staleTempMetadata(err) {
		return err
	}
	return deleteStoredTempRecord(store, registry, path)
}

func staleTempMetadata(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, errTempMetadataChanged)
}

// Promotion reads and removes under one lock; discovery cannot prove absence.
// A checkout proven missing (path or .git gone) clears its stale record and
// succeeds: there is nothing left to protect. Changed branch/identity means
// already permanent. Git failures alone never prove stale permission.
func promoteTempRecord(path string) (bool, error) {
	promoted := false
	err := withLockedTempStore(func(store tempStore, p string) error {
		key := canonical(path)
		rec, ok := store[key]
		if !ok {
			if provenMissingCheckout(path) {
				return nil
			}
			return removeTempIdentity(path)
		}
		var err error
		promoted, err = promoteStoredTempRecord(store, p, key, rec)
		return err
	})
	return promoted, err
}

// Decision tree: missing checkout → promoted=true; stale metadata →
// promoted=false; valid → promoted=true; other errors fail closed.
func promoteStoredTempRecord(store tempStore, registry, path string, rec tempRecord) (bool, error) {
	if err := inspectTempMetadata(path, rec); err != nil {
		if provenMissingCheckout(path) {
			return true, promoteMissingCheckout(store, registry, path)
		}
		if staleTempMetadata(err) {
			return false, persistPromotion(store, registry, path)
		}
		return false, err
	}
	return true, persistPromotion(store, registry, path)
}

// promoteMissingCheckout drops the stale record, then best-effort clears the
// identity token: the checkout is gone, so its private Git admin dir likely
// vanished too. A leftover token must not fail promotion.
func promoteMissingCheckout(store tempStore, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return err
	}
	_ = removeTempIdentity(path)
	return nil
}

func persistPromotion(store tempStore, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return err
	}
	return removeTempIdentity(path)
}

func removeTempIdentity(path string) error {
	p, err := tempIdentityPath(path)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// The global store must check identity, not a repository-local path list.
// Reused paths and changed branches lose temp rules, never their checkout.
func reconcileTempStore() error {
	return withLockedTempStore(func(store tempStore, registry string) error {
		changed, err := reconcileTempRecords(store)
		if changed {
			err = errors.Join(err, writeTempStore(registry, store))
		}
		return err
	})
}

func reconcileTempRecords(store tempStore) (bool, error) {
	changed := false
	var failures []error
	for path, rec := range store {
		// Safe direction: a non-canonical key can never match a live checkout,
		// so dropping it only turns temp into permanent, never removes one.
		if path != canonical(path) {
			delete(store, path)
			changed = true
			continue
		}
		err := inspectTempMetadata(path, rec)
		if staleTempMetadata(err) {
			delete(store, path)
			changed = true
		} else if err != nil {
			failures = append(failures, fmt.Errorf("verify temp checkout %s: %w", path, err))
		}
	}
	return changed, errors.Join(failures...)
}

func provenMissingCheckout(path string) bool {
	return os.IsNotExist(statCheckoutPresence(path))
}

func statCheckoutPresence(path string) error {
	for _, p := range []string{path, filepath.Join(path, ".git")} {
		if _, err := os.Stat(p); err != nil {
			return err
		}
	}
	return nil
}

func inspectTempMetadata(path string, rec tempRecord) error {
	// Absence is evidence; Git command failures alone are not.
	if err := statCheckoutPresence(path); err != nil {
		return err
	}
	return verifyTempIdentity(path, rec)
}
