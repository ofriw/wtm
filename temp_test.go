package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// temp_test.go — Tier-1 contracts for temp.go: TTL parsing, the idle-window
// predicate, display labels, and the locked JSON store. Every store test runs
// under a sandboxed HOME so ~/.wtm/temp.json never touches the host.

// tempTestTime is a fixed, monotonic-free instant so JSON round-trips compare
// equal.
var tempTestTime = time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)

// tempNow is the reference clock for removal paths: past the 1h fixture window.
var tempNow = tempTestTime.Add(2 * time.Hour)

// tempActivation is fresh activity: inside tempNow's window, so it flips an idle fixture to ACTIVE.
var tempActivation = tempTestTime.Add(90 * time.Minute)

const fixtureTempIdentity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// mutateTempStore applies fn under the exclusive lock and writes the whole map
// back atomically; fn returning false skips the write, so a no-op reconcile
// never rewrites the file.
func mutateTempStore(fn func(tempStore) bool) error {
	return withLockedTempStore(func(store tempStore, p string) error {
		if !fn(store) {
			return nil
		}
		return writeTempStore(p, store)
	})
}

func storeTempFixture(path string, rec tempRecord) error {
	return mutateTempStore(func(s tempStore) bool { s[path] = rec; return true })
}

// Raw missing-path fixtures test registry cleanup; live Git paths get identity.
func seedTempRecord(t *testing.T, path, branch, ttl string, createdAt time.Time) {
	t.Helper()
	rec := tempRecord{CreatedAt: createdAt, TTL: ttl, Branch: branch}
	var err error
	if _, gitErr := gitPrivateDir(path); gitErr == nil {
		_, err = addTempRecord(canonical(path), rec)
	} else {
		rec.Identity = fixtureTempIdentity
		err = storeTempFixture(canonical(path), rec)
	}
	if err != nil {
		t.Fatalf("seedTempRecord(%q): %v", path, err)
	}
}

func mustReadTempStore(t *testing.T) tempStore {
	t.Helper()
	store, err := readTempStore()
	if err != nil {
		t.Fatalf("readTempStore: %v", err)
	}
	return store
}

// clearTempRecord removes path's record; an absent record is a no-op success.
func clearTempRecord(path string) error {
	return mutateTempStore(func(s tempStore) bool {
		if _, ok := s[path]; !ok {
			return false
		}
		delete(s, path)
		return true
	})
}

// verifyTempWorktree is the test view of the under-lock pre-removal snapshot.
func verifyTempWorktree(w worktree) error {
	if !w.Temp {
		return nil
	}
	return withLockedTempStore(func(store tempStore, _ string) error {
		return verifyTempSnapshot(store, w)
	})
}

func TestParseTTL(t *testing.T) {
	valid := map[string]time.Duration{
		"1h":  time.Hour,
		"30m": 30 * time.Minute,
		"7d":  7 * 24 * time.Hour,
		"24h": 24 * time.Hour,
	}
	for in, want := range valid {
		got, err := parseTTL(in)
		if err != nil || got != want {
			t.Errorf("parseTTL(%q) = (%v,%v), want %v", in, got, err, want)
		}
	}
	invalid := []string{
		"", "0", "-5m", "abc", "1x", "0d",
		(maxTempTTL + time.Hour).String(),                    // duration past the overflow guard
		fmt.Sprintf("%dd", int(maxTempTTL/(24*time.Hour))+1), // days past the same bound
	}
	for _, in := range invalid {
		if got, err := parseTTL(in); err == nil {
			t.Errorf("parseTTL(%q) = %v, want error", in, got)
		}
	}
}

func TestTempExpired(t *testing.T) {
	base := tempTestTime
	ttl := time.Hour
	cases := []struct {
		name               string
		created, last, now time.Time
		want               bool
	}{
		{"exactly at deadline is not expired", base, time.Time{}, base.Add(ttl), false},
		{"one nanosecond past is expired", base, time.Time{}, base.Add(ttl + time.Nanosecond), true},
		{"just before deadline is not expired", base, time.Time{}, base.Add(ttl - time.Nanosecond), false},
		{"newer lastUsed extends the deadline", base, base.Add(30 * time.Minute), base.Add(ttl + time.Minute), false},
		{"older lastUsed is ignored", base, base.Add(-time.Hour), base.Add(ttl + time.Nanosecond), true},
		{"zero base with a real now is long expired", time.Time{}, time.Time{}, base, true},
		{"all-zero with zero ttl is not expired", time.Time{}, time.Time{}, time.Time{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tempExpired(tc.created, tc.last, ttl, tc.now); got != tc.want {
				t.Fatalf("tempExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemainingWindowLabel(t *testing.T) {
	now := tempTestTime
	cases := []struct {
		name    string
		expires time.Time
		want    string
	}{
		{"now equals deadline is not yet expired", now, "0s"},
		{"past deadline reads expired", now.Add(-time.Minute), "expired"},
		{"two hours", now.Add(2 * time.Hour), "2h"},
		{"forty-five minutes", now.Add(45 * time.Minute), "45m"},
		{"three days", now.Add(3 * 24 * time.Hour), "3d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := remainingWindowLabel(tc.expires, now); got != tc.want {
				t.Fatalf("remainingWindowLabel = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCompactWindow(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{3 * 24 * time.Hour, "3d"},
		{2 * time.Hour, "2h"},
		{90 * time.Minute, "1h30m"},
		{45 * time.Minute, "45m"},
		{30 * time.Second, "30s"},
	}
	for _, tc := range cases {
		if got := compactWindow(tc.d); got != tc.want {
			t.Errorf("compactWindow(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func fixtureRecord(branch string) tempRecord {
	return tempRecord{CreatedAt: tempTestTime, TTL: "1h", Branch: branch, Identity: fixtureTempIdentity}
}

func storeFixtureRecords(t *testing.T) tempStore {
	t.Helper()
	sandbox(t)
	base := t.TempDir()
	want := tempStore{filepath.Join(base, "a"): fixtureRecord("tmp/a"), filepath.Join(base, "b"): fixtureRecord("tmp/b")}
	for path, rec := range want {
		if err := storeTempFixture(path, rec); err != nil {
			t.Fatal(err)
		}
	}
	return want
}

func assertTempStore(t *testing.T, want tempStore) {
	t.Helper()
	got := mustReadTempStore(t)
	if len(got) != len(want) {
		t.Fatalf("store size %d, want %d", len(got), len(want))
	}
	for path, rec := range want {
		if got[path] != rec {
			t.Fatalf("record %s: %+v, want %+v", path, got[path], rec)
		}
	}
}

func TestTempStoreRoundTrip(t *testing.T) {
	want := storeFixtureRecords(t)
	assertTempStore(t, want)
	if err := clearTempRecord(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatal(err)
	}
	for path := range want {
		if err := clearTempRecord(path); err != nil {
			t.Fatal(err)
		}
		delete(want, path)
		break
	}
	assertTempStore(t, want)
}

// TestTempStoreConcurrentAdds proves the exclusive lock serializes the
// read-modify-write: N goroutines adding distinct keys must all survive.
func TestTempStoreConcurrentAdds(t *testing.T) {
	sandbox(t)
	dir := t.TempDir()
	const n = 12
	errs := concurrentFixtureAdds(dir, n)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent addTempRecord: %v", err)
		}
	}
	if store := mustReadTempStore(t); len(store) != n {
		t.Fatalf("store has %d records, want %d (lost updates): %+v", len(store), n, store)
	}
}

func concurrentFixtureAdds(dir string, n int) <-chan error {
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("wt-%d", i)
			errs <- storeTempFixture(filepath.Join(dir, name), fixtureRecord("tmp/"+name))
		}(i)
	}
	wg.Wait()
	close(errs)
	return errs
}

// TestDiscoveryLenientOnCorruptStore pins the read-only contract: a corrupt
// registry loses temp metadata but never breaks discovery, so status still runs.
func TestDiscoveryLenientOnCorruptStore(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	gitTestCommit(t, repo)
	p, err := tempPath()
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, p, "{not json", 0o600)
	var wts []worktree
	var derr error
	warnings := captureStderr(t, func() { wts, derr = discover(repo, nil) })
	if derr != nil {
		t.Fatalf("discover on corrupt store: %v", derr)
	}
	if len(wts) != 1 || wts[0].Temp {
		t.Fatalf("wts = %+v, want one permanent worktree", wts)
	}
	if !strings.Contains(warnings, "temp store") {
		t.Fatalf("corrupt store warning missing: %q", warnings)
	}
}

func TestReconcileTempStore(t *testing.T) {
	sandbox(t)
	base := t.TempDir()
	keep, gone := initRepo(t, "tmp/keep"), filepath.Join(base, "gone")
	seedTempRecord(t, keep, "tmp/keep", "1h", tempTestTime)
	seedTempRecord(t, gone, "tmp/gone", "1h", tempTestTime)
	if err := reconcileTempStore(); err != nil {
		t.Fatalf("reconcileTempStore: %v", err)
	}
	store := mustReadTempStore(t)
	if _, ok := store[canonical(gone)]; ok {
		t.Fatalf("missing checkout survived reconcile: %+v", store)
	}
	if _, ok := store[canonical(keep)]; !ok {
		t.Fatalf("existing checkout dropped by reconcile: %+v", store)
	}
}

// A store key that is not canonical can never match a live checkout, so
// reconcile drops it: the checkout becomes permanent, never removed.
func TestReconcileDropsNonCanonicalKeys(t *testing.T) {
	sandbox(t)
	keep := initRepo(t, "tmp/keep")
	seedTempRecord(t, keep, "tmp/keep", "1h", tempTestTime)
	stale := canonical(keep) + string(os.PathSeparator) + "."
	if err := storeTempFixture(stale, fixtureRecord("tmp/keep")); err != nil {
		t.Fatal(err)
	}
	if err := reconcileTempStore(); err != nil {
		t.Fatal(err)
	}
	store := mustReadTempStore(t)
	if _, ok := store[stale]; ok {
		t.Fatalf("non-canonical key survived reconcile: %+v", store)
	}
	if _, ok := store[canonical(keep)]; !ok {
		t.Fatalf("canonical record dropped by reconcile: %+v", store)
	}
}

// Promoting a checkout proven missing clears its stale record and succeeds:
// there is nothing left for the identity to protect.
func TestPromoteMissingCheckoutClearsStaleRecord(t *testing.T) {
	_, path, w := identityCheckout(t)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if promoted, err := promoteTempRecord(w.Path); err != nil || !promoted {
		t.Fatalf("promote missing = %v, %v; want (true, nil)", promoted, err)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("missing checkout retained registry permission")
	}
	if promoted, err := promoteTempRecord(w.Path); err != nil || promoted {
		t.Fatalf("retry promote missing = %v, %v; want (false, nil)", promoted, err)
	}
}

// Malformed records must not become proven absence during mutation.
func TestDecodeTempStoreRejectsInvalidEntry(t *testing.T) {
	sandbox(t)
	good := canonical(filepath.Join(t.TempDir(), "good"))
	bad := canonical(filepath.Join(t.TempDir(), "bad"))
	data, err := json.Marshal(tempStore{
		good: {CreatedAt: tempTestTime, TTL: "1h", Branch: "tmp/good", Identity: fixtureTempIdentity},
		bad:  {CreatedAt: tempTestTime, TTL: "bogus", Branch: "tmp/bad", Identity: fixtureTempIdentity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTempStore(data, "/store/temp.json"); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("invalid record must fail strict decode: %v", err)
	}
}

func TestDecodeTempStoreMalformedJSON(t *testing.T) {
	sandbox(t)
	if _, err := decodeTempStore([]byte("{not json"), "/store/temp.json"); err == nil {
		t.Fatal("malformed top-level JSON must be an error")
	}
}

// tempJSONShape decodes printStatusJSON's temp fields; the struct tags are the
// script contract, so the test asserts on the raw keys too.
type tempJSONShape struct {
	Temp      bool    `json:"temp"`
	ExpiresAt *string `json:"expiresAt"`
}

func TestPrintStatusJSONTempFields(t *testing.T) {
	now := tempTestTime
	ttl := 10 * time.Hour
	created := tempTestTime.Add(123456789 * time.Nanosecond)
	wts := []worktree{
		{Path: "/perm", Branch: "main"},
		{Path: "/temp", Branch: "tmp/x", Temp: true, TempCreated: created, TempTTL: 2*time.Hour + 1*time.Nanosecond},
	}
	out := captureStdout(t, func() {
		if err := printStatusJSON(wts, now, ttl); err != nil {
			t.Fatalf("printStatusJSON: %v", err)
		}
	})
	var rows []tempJSONShape
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("status JSON invalid: %v\n%s", err, out)
	}
	if rows[0].Temp || rows[0].ExpiresAt != nil {
		t.Fatalf("permanent row = %+v, want temp false and expiresAt null", rows[0])
	}
	if !rows[1].Temp || rows[1].ExpiresAt == nil {
		t.Fatalf("temp row = %+v, want temp true and expiresAt set", rows[1])
	}
	want := "2024-01-02T05:04:05.12345679Z"
	if *rows[1].ExpiresAt != want {
		t.Fatalf("expiresAt = %q, want %q", *rows[1].ExpiresAt, want)
	}
	if !strings.Contains(out, `"temp": true`) || !strings.Contains(out, `"expiresAt": null`) {
		t.Fatalf("JSON keys missing from output:\n%s", out)
	}
}

// Reading with no registry must stay side-effect free: `wtm status` on a
// machine that never used --temp must not create ~/.wtm or its lock file.
func TestReadTempStoreWithoutRegistryIsSideEffectFree(t *testing.T) {
	env := sandbox(t)
	store, err := readTempStore()
	if err != nil {
		t.Fatalf("readTempStore: %v", err)
	}
	if len(store) != 0 {
		t.Fatalf("store = %v, want empty", store)
	}
	dir := filepath.Join(env.Home, ".wtm")
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("read created %s: %v", dir, err)
	}
}

// status and promote are read-only against the registry: they must not create
// ~/.wtm, take the registry lock, or drop records. Only gc reconciles.
func TestReadCommandsLeaveNoRegistryState(t *testing.T) {
	for _, cmd := range []string{"status", "promote"} {
		t.Run(cmd, func(t *testing.T) {
			env := sandbox(t)
			repo := nativeRepo(t)
			captureStdout(t, func() {
				if err := dispatch(cmd, &globals{root: repo, json: true}, nil); err != nil {
					t.Logf("%s error: %v", cmd, err)
				}
			})
			if _, err := os.Stat(filepath.Join(env.Home, ".wtm")); !os.IsNotExist(err) {
				t.Fatalf("%s created ~/.wtm: %v", cmd, err)
			}
		})
	}
}
