package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"wtm/internal/core"
	"wtm/internal/fs"
	"wtm/internal/git"
	"wtm/internal/temp"
	. "wtm/internal/testutil"
)

// temp_test.go — Tier-1 contracts for temp.go: TTL parsing, the idle-window
// predicate, display labels, and the locked JSON store. Every store test runs
// under a sandboxed HOME so ~/.wtm/temp.json never touches the host.

// tempActivation is fresh activity: inside TempNow's window, so it flips an idle fixture to ACTIVE.
var tempActivation = TempTestTime.Add(90 * time.Minute)

const fixtureTempIdentity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// mutateTempStore applies fn to the registry under the exclusive registry lock
// and persists the result atomically: every mutation is a locked
// read-modify-write.
func mutateTempStore(fn func(temp.Store)) error {
	return temp.WithLockedTempStore(func(s temp.Store) error {
		fn(s)
		return nil
	})
}

func storeTempFixture(path string, rec temp.Record) error {
	return mutateTempStore(func(s temp.Store) { s[path] = rec })
}

// Raw missing-path fixtures test registry cleanup; live Git paths get identity.
func seedTempRecord(t *testing.T, path, branch, ttl string, createdAt time.Time) {
	t.Helper()
	rec := temp.Record{CreatedAt: createdAt, TTL: ttl, Branch: branch}
	var err error
	if _, gitErr := git.PrivateDir(path); gitErr == nil {
		_, err = temp.AddTempRecord(CanonicalPath(path), rec)
	} else {
		rec.Identity = fixtureTempIdentity
		err = storeTempFixture(CanonicalPath(path), rec)
	}
	if err != nil {
		t.Fatalf("seedTempRecord(%q): %v", path, err)
	}
}

func mustReadTempStore(t *testing.T) temp.Store {
	t.Helper()
	store, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		t.Fatalf("readTempStore: %v", err)
	}
	return store
}

// clearTempRecord removes path's record; an absent record is a no-op success.
func clearTempRecord(path string) error {
	return mutateTempStore(func(s temp.Store) { delete(s, path) })
}

// verifyTempWorktree is the test view of the pre-removal snapshot.
func verifyTempWorktree(w core.Worktree) error {
	if !w.Temp {
		return nil
	}
	store, err := temp.ReadTempStoreUnlocked()
	if err != nil {
		return err
	}
	return temp.VerifyTempSnapshot(store, temp.CheckoutIdentity{Path: w.Path, Branch: w.Branch, Identity: w.TempIdentity})
}

func TestParseTTL(t *testing.T) {
	valid := map[string]time.Duration{
		"1h":  time.Hour,
		"30m": 30 * time.Minute,
		"7d":  7 * 24 * time.Hour,
		"24h": 24 * time.Hour,
	}
	for in, want := range valid {
		got, err := temp.ParseTTL(in)
		if err != nil || got != want {
			t.Errorf("temp.ParseTTL(%q) = (%v,%v), want %v", in, got, err, want)
		}
	}
	invalid := []string{
		"", "0", "-5m", "abc", "1x", "0d",
		(temp.MaxTempTTL + time.Hour).String(),                    // duration past the overflow guard
		fmt.Sprintf("%dd", int(temp.MaxTempTTL/(24*time.Hour))+1), // days past the same bound
	}
	for _, in := range invalid {
		if got, err := temp.ParseTTL(in); err == nil {
			t.Errorf("temp.ParseTTL(%q) = %v, want error", in, got)
		}
	}
}

func TestTempExpired(t *testing.T) {
	base := TempTestTime
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
			if got := temp.TempExpired(tc.created, tc.last, ttl, tc.now); got != tc.want {
				t.Fatalf("temp.TempExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRemainingWindowLabel(t *testing.T) {
	now := TempTestTime
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
			if got := temp.RemainingWindowLabel(tc.expires, now); got != tc.want {
				t.Fatalf("temp.RemainingWindowLabel = %q, want %q", got, tc.want)
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
		if got := temp.CompactWindow(tc.d); got != tc.want {
			t.Errorf("temp.CompactWindow(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

func fixtureRecord(branch string) temp.Record {
	return temp.Record{CreatedAt: TempTestTime, TTL: "1h", Branch: branch, Identity: fixtureTempIdentity}
}

func assertTempStore(t *testing.T, want temp.Store) {
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

// TestDiscoveryLenientOnCorruptStore pins the read-only contract: a corrupt
// registry loses temp metadata but never breaks discovery, so status still runs.
func TestDiscoveryLenientOnCorruptStore(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	p, err := fs.WtmFilePath("temp.json")
	if err != nil {
		t.Fatal(err)
	}
	WriteFile(t, p, "{not json", 0o600)
	var ws core.Workspace
	var derr error
	warnings := CaptureStderr(t, func() { ws, derr = core.LoadWorkspace(core.Options{Root: repo}, false, defaultHooks()) })
	wts := ws.Wts
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
	Sandbox(t)
	base := t.TempDir()
	keep, gone := InitRepo(t, "tmp/keep"), filepath.Join(base, "gone")
	seedTempRecord(t, keep, "tmp/keep", "1h", TempTestTime)
	seedTempRecord(t, gone, "tmp/gone", "1h", TempTestTime)
	if err := temp.ReconcileTempStore(); err != nil {
		t.Fatalf("temp.ReconcileTempStore: %v", err)
	}
	store := mustReadTempStore(t)
	if _, ok := store[CanonicalPath(gone)]; ok {
		t.Fatalf("missing checkout survived reconcile: %+v", store)
	}
	if _, ok := store[CanonicalPath(keep)]; !ok {
		t.Fatalf("existing checkout dropped by reconcile: %+v", store)
	}
}

// A store key that is not Canonical can never match a live checkout, so
// reconcile drops it: the checkout becomes permanent, never removed.
func TestReconcileDropsNonCanonicalKeys(t *testing.T) {
	Sandbox(t)
	keep := InitRepo(t, "tmp/keep")
	seedTempRecord(t, keep, "tmp/keep", "1h", TempTestTime)
	stale := CanonicalPath(keep) + string(os.PathSeparator) + "."
	if err := storeTempFixture(stale, fixtureRecord("tmp/keep")); err != nil {
		t.Fatal(err)
	}
	if err := temp.ReconcileTempStore(); err != nil {
		t.Fatal(err)
	}
	store := mustReadTempStore(t)
	if _, ok := store[stale]; ok {
		t.Fatalf("non-canonical key survived reconcile: %+v", store)
	}
	if _, ok := store[CanonicalPath(keep)]; !ok {
		t.Fatalf("Canonical record dropped by reconcile: %+v", store)
	}
}

// Promoting a checkout proven missing clears its stale record and succeeds:
// there is nothing left for the identity to protect.
func TestPromoteMissingCheckoutClearsStaleRecord(t *testing.T) {
	_, path, w := identityCheckout(t)
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if promoted, err := temp.PromoteTempRecord(w.Path); err != nil || !promoted {
		t.Fatalf("promote missing = %v, %v; want (true, nil)", promoted, err)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("missing checkout retained registry permission")
	}
	if promoted, err := temp.PromoteTempRecord(w.Path); err != nil || promoted {
		t.Fatalf("retry promote missing = %v, %v; want (false, nil)", promoted, err)
	}
}

// tempJSONShape decodes core.PrintStatusJSON's temp fields; the struct tags are the
// script contract, so the test asserts on the raw keys too.
type tempJSONShape struct {
	Temp      bool    `json:"temp"`
	ExpiresAt *string `json:"expiresAt"`
}

func TestPrintStatusJSONTempFields(t *testing.T) {
	now := TempTestTime
	ttl := 10 * time.Hour
	created := TempTestTime.Add(123456789 * time.Nanosecond)
	wts := []core.Worktree{
		{Path: "/perm", Branch: "main"},
		{Path: "/temp", Branch: "tmp/x", Temp: true, TempCreated: created, TempTTL: 2*time.Hour + 1*time.Nanosecond},
	}
	out := CaptureStdout(t, func() {
		if err := core.PrintStatusJSON(wts, now, ttl); err != nil {
			t.Fatalf("core.PrintStatusJSON: %v", err)
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

// status and promote are read-only against the registry: they must not create
// ~/.wtm, take the registry lock, or drop records. Only gc reconciles.
func TestReadCommandsLeaveNoRegistryState(t *testing.T) {
	for _, cmd := range []string{"status", "promote"} {
		t.Run(cmd, func(t *testing.T) {
			env := Sandbox(t)
			repo := NativeRepo(t)
			CaptureStdout(t, func() {
				if err := dispatch(cmd, &globals{Root: repo, JSON: true}, nil); err != nil {
					t.Logf("%s error: %v", cmd, err)
				}
			})
			if _, err := os.Stat(filepath.Join(env.Home, ".wtm")); !os.IsNotExist(err) {
				t.Fatalf("%s created ~/.wtm: %v", cmd, err)
			}
		})
	}
}
