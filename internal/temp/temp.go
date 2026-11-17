package temp

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

	"wtm/internal/fs"
	"wtm/internal/git"
)

// temp.go — the ephemeral-worktree store: which checkouts reclaim themselves
// and after how long. It is the only file that knows ~/.wtm/temp.json. The
// store operates on checkout paths; activity for idle checks is injected as
// data, so this package never reaches into harness discovery.

// defaultTempTTL is the idle window a temp worktree gets when `add --temp`
// carries no --ttl.
const defaultTempTTL = time.Hour

// Sanity bound on user input; 100 years is well within time.Time's range.
const MaxTempTTL = 100 * 365 * 24 * time.Hour

// Record is one tracked ephemeral checkout. The effective deadline is
// max(CreatedAt, LastUsed)+TTL, so the TTL is an idle window, not a fixed age.
type Record struct {
	CreatedAt time.Time `json:"createdAt"`
	TTL       string    `json:"ttl"`
	Branch    string    `json:"branch"`
	Identity  string    `json:"identity"`
}

// Store maps a canonical checkout path to its record.
type Store map[string]Record

// ActivityFn refreshes a checkout's activity at now. It returns the newest
// known activity time, plus any unverified sessions that predate it (stale),
// or an error when activity cannot be refreshed.
type ActivityFn func(path string, now time.Time) (last time.Time, stale []string, err error)

func tempPath() (string, error) { return fs.WtmFilePath("temp.json") }

func TempLockPath() (string, error) { return fs.WtmFilePath("temp.lock") }

// DefaultTempTTL is the idle window a temp worktree gets when `add --temp`
// carries no --ttl.
func DefaultTempTTL() time.Duration { return defaultTempTTL }

// ParseTTL parses a positive idle window: a Go duration (30m, 1h, 24h) or whole
// days (7d), matching the settings file's day syntax.
func ParseTTL(s string) (time.Duration, error) {
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
	if d > MaxTempTTL {
		return 0, fmt.Errorf("ttl %q: exceeds the maximum %s", s, MaxTempTTL)
	}
	return d, nil
}

func parseTTLDays(s, days string) (time.Duration, error) {
	n, err := strconv.Atoi(days)
	if err != nil || n < 1 || n > int(MaxTempTTL/(24*time.Hour)) {
		return 0, fmt.Errorf("ttl %q: want a positive duration (30m, 1h) or whole days (7d)", s)
	}
	return time.Duration(n) * 24 * time.Hour, nil
}

// TempDeadline is the deadline a temp worktree is measured against. Anchoring
// on the newer of creation and last use makes any edit, commit or agent
// session push the deadline forward.
func TempDeadline(createdAt, lastUsed time.Time, ttl time.Duration) time.Time {
	if lastUsed.After(createdAt) {
		createdAt = lastUsed
	}
	return createdAt.Add(ttl)
}

// FormatTempDeadline preserves supported fractional TTLs in all machine-readable
// deadlines.
func FormatTempDeadline(deadline time.Time) string {
	return deadline.UTC().Format(time.RFC3339Nano)
}

// TempExpired reports whether a temp worktree has been idle longer than ttl.
func TempExpired(createdAt, lastUsed time.Time, ttl time.Duration, now time.Time) bool {
	return now.After(TempDeadline(createdAt, lastUsed, ttl))
}

// RemainingWindowLabel renders a temp deadline for display: "expired" once it
// has passed, else a compact duration. It is the single source of truth for the
// status TEMP cell and the gc picker's window column. The strict After matches
// TempExpired (the candidate predicate), so STATUS and TEMP never disagree.
func RemainingWindowLabel(expiresAt, now time.Time) string {
	if now.After(expiresAt) {
		return "expired"
	}
	return compactWindow(expiresAt.Sub(now))
}

// compactWindow renders d at the coarsest whole unit that fits: 3d, 2h, 1h30m, 45m.
func compactWindow(d time.Duration) string {
	switch {
	// Negative durations should not reach here (RemainingWindowLabel guards
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

// CompactWindow renders d at the coarsest whole unit that fits.
func CompactWindow(d time.Duration) string { return compactWindow(d) }

// ReadTempStoreUnlocked reads the registry via a shared handle that lets a
// concurrent mutation atomically replace the file (open_windows.go). It is
// called by both unlocked readers and mutations holding the registry lock.
// Missing files are empty stores; malformed files remain explicit errors.
func ReadTempStoreUnlocked() (Store, error) {
	p, err := tempPath()
	if err != nil {
		return nil, err
	}
	data, err := fs.ReadFileShared(p)
	if os.IsNotExist(err) {
		return Store{}, nil
	}
	if err != nil {
		return nil, err
	}
	return DecodeTempStore(data, p)
}

// Mutation must not turn an unreadable record into proven absence.
func DecodeTempStore(data []byte, p string) (Store, error) {
	var store Store
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

func validateTempRecord(rec Record) error {
	if _, err := ParseTTL(rec.TTL); err != nil {
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

func WithLockedTempStore(fn func(Store, string) error) error {
	p, err := tempPath()
	if err != nil {
		return err
	}
	lock, err := TempLockPath()
	if err != nil {
		return err
	}
	return fs.WithFileLock(lock, func() error {
		store, err := ReadTempStoreUnlocked()
		if err != nil {
			return err
		}
		return fn(store, p)
	})
}

func WriteTempStore(path string, store Store) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return err
	}
	return fs.WriteFileAtomic(path, append(data, '\n'), 0o600)
}

// ValidTempIdentity reports whether identity has the temp-record fingerprint
// shape; exported for tests pinning the identity contract.
func ValidTempIdentity(identity string) bool {
	return validTempIdentity(identity)
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

func TempIdentityPath(path string) (string, error) {
	dir, err := git.GitPrivateDir(path)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "wtm-temp-identity"), nil
}

func installTempIdentity(path string, rec Record) error {
	branch, err := git.GitCurrentBranch(path)
	if err != nil {
		return err
	}
	if branch != rec.Branch {
		return fmt.Errorf("temp branch changed: want %s, got %s", rec.Branch, branch)
	}
	p, err := TempIdentityPath(path)
	if err != nil {
		return err
	}
	return fs.WriteFileAtomic(p, []byte(rec.Identity), 0o600)
}

var ErrTempMetadataChanged = errors.New("temp metadata changed")

// VerifyTempIdentity checks the checkout's stored identity token against the
// record, failing when it is missing, replaced or its branch changed.
func VerifyTempIdentity(path string, rec Record) error {
	if err := validateTempRecord(rec); err != nil {
		return err
	}
	p, err := TempIdentityPath(path)
	if err != nil {
		return err
	}
	data, err := fs.ReadFileShared(p)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: missing identity for %s", ErrTempMetadataChanged, path)
		}
		return err
	}
	if string(data) != rec.Identity {
		return fmt.Errorf("%w: identity for %s", ErrTempMetadataChanged, path)
	}
	return verifyTempBranch(path, rec.Branch)
}

func verifyTempBranch(path, expected string) error {
	branch, err := git.GitCurrentBranch(path)
	if err != nil {
		return err
	}
	if branch != expected {
		return fmt.Errorf("%w: branch for %s: want %s, got %s", ErrTempMetadataChanged, path, expected, branch)
	}
	return nil
}

func VerifyTempSnapshot(store Store, path, branch, identity string) error {
	rec, ok := store[path]
	if !ok {
		return fmt.Errorf("%w: %s", ErrTempPromoted, path)
	}
	if rec.Identity != identity || rec.Branch != branch {
		return fmt.Errorf("%w: identity or branch changed for %s", ErrTempMetadataChanged, path)
	}
	return VerifyTempIdentity(path, rec)
}

var (
	ErrTempActive   = errors.New("temp worktree is now ACTIVE")
	ErrTempPromoted = errors.New("temp record was promoted or removed")
)

// Idle GC refreshes activity under the lock; explicit removals bypass idleness.
// Unverified sessions block only when newer than the candidate's known newest
// activity — only such a file could be its hidden recent use. Older ones warn.
func VerifyTempIdle(store Store, path string, now time.Time, fresh ActivityFn) (stale []string, err error) {
	// The record exists: verifyTempRemoval checked the snapshot before this runs.
	rec := store[path]
	ttl, err := ParseTTL(rec.TTL)
	if err != nil {
		return nil, err
	}
	last, stale, err := fresh(path, now)
	if err != nil {
		return nil, fmt.Errorf("refresh worktree activity: %w", err)
	}
	if !TempExpired(rec.CreatedAt, last, ttl, now) {
		return nil, ErrTempActive
	}
	return stale, nil
}

// Registry permission cannot change between verification and checkout removal.
// Removed stays true when the checkout is gone but registry persistence fails.
// RemoveTempWorktreeIf holds the exclusive registry lock for the entire
// idle verification + checkout removal sequence.  Verification refreshes
// external state (Git activity, agent sessions) and removal runs a blocking
// git command; callers should not expect the lock to be short-lived.
func RemoveTempWorktreeIf(path, branch, identity string, idleOnly bool, now time.Time, fresh ActivityFn) (removed bool, stale []string, err error) {
	err = WithLockedTempStore(func(store Store, registry string) error {
		stale, err = verifyTempRemoval(store, path, branch, identity, idleOnly, now, fresh)
		if err != nil {
			return err
		}
		if err := removeTempCheckout(path); err != nil {
			return err
		}
		removed = true
		return persistTempRemoval(store, registry, path)
	})
	return removed, stale, err
}

func verifyTempRemoval(store Store, path, branch, identity string, idleOnly bool, now time.Time, fresh ActivityFn) ([]string, error) {
	// branch/identity come from the observed checkout (the inventory record);
	// the stored record alone cannot prove the checkout is the same one.
	if err := VerifyTempSnapshot(store, path, branch, identity); err != nil {
		return nil, err
	}
	if idleOnly {
		return VerifyTempIdle(store, path, now, fresh)
	}
	return nil, nil
}

func persistTempRemoval(store Store, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return fmt.Errorf("clear temp record for %s: %w", path, err)
	}
	return nil
}

func deleteStoredTempRecord(store Store, registry, path string) error {
	delete(store, path)
	return WriteTempStore(registry, store)
}

func removeTempCheckout(path string) error {
	root, err := git.RepoRoot(path)
	if err != nil {
		return err
	}
	return git.WorktreeRemove(root, path)
}

func prepareTempRecord(rec Record) (Record, error) {
	if rec.Identity == "" {
		identity, err := newTempIdentity()
		if err != nil {
			return rec, err
		}
		rec.Identity = identity
	}
	return rec, validateTempRecord(rec)
}

// AddTempRecord writes the store record for a freshly seeded temp worktree,
// keyed by canonical path so lookups from git, Pi and the filesystem compare
// equal.
func AddTempRecord(path string, rec Record) (Record, error) {
	rec, err := prepareTempRecord(rec)
	if err != nil {
		return Record{}, err
	}
	err = WithLockedTempStore(func(store Store, p string) error {
		if err := installTempIdentity(path, rec); err != nil {
			return err
		}
		store[fs.Canonical(path)] = rec
		return WriteTempStore(p, store)
	})
	if err != nil {
		return Record{}, err
	}
	return rec, nil
}

// Permanent removal also holds the registry lock: a stale record grants no
// temp permission, but a newly registered checkout must never be removed.
func RemovePermanentWorktree(root, path string) (bool, error) {
	removed := false
	err := WithLockedTempStore(func(store Store, registry string) error {
		if err := clearStaleTempTarget(store, registry, fs.Canonical(path)); err != nil {
			return err
		}
		err := git.WorktreeRemoveRecovering(root, path)
		removed = err == nil
		return err
	})
	return removed, err
}

func clearStaleTempTarget(store Store, registry, path string) error {
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
	return os.IsNotExist(err) || errors.Is(err, ErrTempMetadataChanged)
}

// Promotion reads and removes under one lock; discovery cannot prove absence.
// A checkout proven missing (path or .git gone) clears its stale record and
// succeeds: there is nothing left to protect. Changed branch/identity means
// already permanent. Git failures alone never prove stale permission.
func PromoteTempRecord(path string) (bool, error) {
	promoted := false
	err := WithLockedTempStore(func(store Store, p string) error {
		key := fs.Canonical(path)
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
func promoteStoredTempRecord(store Store, registry, path string, rec Record) (bool, error) {
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
func promoteMissingCheckout(store Store, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return err
	}
	_ = removeTempIdentity(path)
	return nil
}

func persistPromotion(store Store, registry, path string) error {
	if err := deleteStoredTempRecord(store, registry, path); err != nil {
		return err
	}
	return removeTempIdentity(path)
}

func removeTempIdentity(path string) error {
	p, err := TempIdentityPath(path)
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
func ReconcileTempStore() error {
	return WithLockedTempStore(func(store Store, registry string) error {
		changed, err := reconcileTempRecords(store)
		if changed {
			err = errors.Join(err, WriteTempStore(registry, store))
		}
		return err
	})
}

func reconcileTempRecords(store Store) (bool, error) {
	changed := false
	var failures []error
	for path, rec := range store {
		// Safe direction: a non-canonical key can never match a live checkout,
		// so dropping it only turns temp into permanent, never removes one.
		if path != fs.Canonical(path) {
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

func inspectTempMetadata(path string, rec Record) error {
	// Absence is evidence; Git command failures alone are not.
	if err := statCheckoutPresence(path); err != nil {
		return err
	}
	return VerifyTempIdentity(path, rec)
}
