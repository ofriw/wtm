package temp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"wtm/internal/fs"
	. "wtm/internal/testutil"
)

// store_test.go — raw store behaviors: decode strictness, locked round-trips,
// lock-serialized concurrent writes, and side-effect-free reads. Multi-process
// lock contention stays covered by the cli integration suite.

const fixtureStoreIdentity = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func fixtureStoreRecord(branch string) Record {
	return Record{CreatedAt: TempTestTime, TTL: "1h", Branch: branch, Identity: fixtureStoreIdentity}
}

// seedStore writes recs through the exclusive lock and returns the stored map.
func seedStore(t *testing.T, recs map[string]Record) Store {
	t.Helper()
	if err := withLockedTempStore(func(store Store, _ string) error {
		for k, v := range recs {
			store[fs.Canonical(k)] = v
		}
		return WriteTempStoreUnlocked(store)
	}); err != nil {
		t.Fatal(err)
	}
	store, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestDecodeTempStoreRejectsInvalidEntry(t *testing.T) {
	good := fs.Canonical(filepath.Join(t.TempDir(), "good"))
	bad := fs.Canonical(filepath.Join(t.TempDir(), "bad"))
	data, err := json.Marshal(Store{
		good: fixtureStoreRecord("tmp/good"),
		bad:  {CreatedAt: TempTestTime, TTL: "bogus", Branch: "tmp/bad", Identity: fixtureStoreIdentity},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeTempStore(data, "/store/temp.json"); err == nil || !strings.Contains(err.Error(), bad) {
		t.Fatalf("invalid record must fail strict decode: %v", err)
	}
}

func TestDecodeTempStoreMalformedJSON(t *testing.T) {
	if _, err := decodeTempStore([]byte("{not json"), "/store/temp.json"); err == nil {
		t.Fatal("malformed top-level JSON must be an error")
	}
}

func TestTempStoreRoundTrip(t *testing.T) {
	Sandbox(t)
	base := t.TempDir()
	want := map[string]Record{
		filepath.Join(base, "a"): fixtureStoreRecord("tmp/a"),
		filepath.Join(base, "b"): fixtureStoreRecord("tmp/b"),
	}
	store := seedStore(t, want)
	for path, rec := range want {
		key := fs.Canonical(path)
		if store[key] != rec {
			t.Fatalf("record %s: %+v, want %+v", key, store[key], rec)
		}
	}
	// Mutation under the lock drops one record and persists it.
	drop := fs.Canonical(filepath.Join(base, "a"))
	if err := withLockedTempStore(func(s Store, _ string) error {
		delete(s, drop)
		return WriteTempStoreUnlocked(s)
	}); err != nil {
		t.Fatal(err)
	}
	store, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := store[drop]; ok {
		t.Fatal("dropped record still present")
	}
	if len(store) != 1 {
		t.Fatalf("store has %d records, want 1", len(store))
	}
}

func TestTempStoreConcurrentAdds(t *testing.T) {
	Sandbox(t)
	base := t.TempDir()
	const n = 12
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name := fmt.Sprintf("wt-%d", i)
			errs <- withLockedTempStore(func(store Store, _ string) error {
				store[fs.Canonical(filepath.Join(base, name))] = fixtureStoreRecord("tmp/" + name)
				return WriteTempStoreUnlocked(store)
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent store write: %v", err)
		}
	}
	store, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(store) != n {
		t.Fatalf("store has %d records, want %d (lost updates)", len(store), n)
	}
}

func TestReadTempStoreWithoutRegistryIsSideEffectFree(t *testing.T) {
	env := Sandbox(t)
	store, err := ReadTempStoreUnlocked()
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

// TestVerifyTempIdleNilProbeFailClosed pins the fail-closed nil-probe rule:
// a missing probe is an explicit error, never a panic.
func TestVerifyTempIdleNilProbeFailClosed(t *testing.T) {
	_, err := VerifyTempIdle(Store{}, "/any/path", TempTestTime, nil)
	if err == nil || !strings.Contains(err.Error(), "activity probe is required") {
		t.Fatalf("nil probe = %v; want explicit \"activity probe is required\" error", err)
	}
}

// TestRemoveTempWorktreeIfNilProbeLeavesRecord pins the same rule through the
// full removal path: a nil probe fails before any removal, so the checkout
// survives and the registry record stays unchanged.
func TestRemoveTempWorktreeIfNilProbeLeavesRecord(t *testing.T) {
	Sandbox(t)
	repo := InitRepo(t, "main")
	GitTestCommit(t, repo)
	path := filepath.Join(filepath.Dir(repo), "temp")
	GitWorktreeAdd(t, repo, path, "tmp/test", "HEAD")
	rec, err := AddTempRecord(path, Record{CreatedAt: TempTestTime, TTL: "1h", Branch: "tmp/test"})
	if err != nil {
		t.Fatal(err)
	}
	key := fs.Canonical(path)
	before, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	removed, _, err := RemoveTempWorktreeIf(key, rec.Branch, rec.Identity, true, TempTestTime, nil)
	if removed || err == nil {
		t.Fatalf("nil probe removal = %v, %v; want (false, error)", removed, err)
	}
	if !strings.Contains(err.Error(), "activity probe is required") {
		t.Fatalf("nil probe error = %v; want explicit \"activity probe is required\"", err)
	}
	after, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) || after[key] != before[key] {
		t.Fatalf("registry changed: before %+v, after %+v", before, after)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("checkout removed: %v", err)
	}
}

// TestTempStoreReaderSnapshotsWhileWriterHoldsLock pins the shared-reader
// contract: a reader concurrent with an exclusive-lock holder must not wait on
// the lock and must always observe a complete, valid store — exactly one of the
// committed states, never a torn mix. The writer keeps the lock and rewrites the
// whole store atomically until the reader has finished, so a reader that tried
// to take the lock would deadlock and trip the timeout guard here.
func TestTempStoreReaderSnapshotsWhileWriterHoldsLock(t *testing.T) {
	Sandbox(t)
	base := t.TempDir()
	storeA := Store{
		fs.Canonical(filepath.Join(base, "a")): fixtureStoreRecord("tmp/a"),
	}
	storeB := Store{
		fs.Canonical(filepath.Join(base, "a")): fixtureStoreRecord("tmp/a"),
		fs.Canonical(filepath.Join(base, "b")): fixtureStoreRecord("tmp/b"),
	}
	lock, err := LockPath()
	if err != nil {
		t.Fatal(err)
	}
	readerDone := make(chan struct{})
	writerInside := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		// Single lock acquisition for the whole writer lifetime: nesting
		// WithFileLock would re-lock the same inode on a second descriptor,
		// which blocks (flock) even within one process.
		err := fs.WithFileLock(lock, func() error {
			for i := 0; ; i++ {
				s := storeA
				if i%2 == 1 {
					s = storeB
				}
				if err := WriteTempStoreUnlocked(s); err != nil {
					return err
				}
				if i == 0 {
					close(writerInside)
				}
				select {
				case <-readerDone:
					return nil
				default:
				}
				time.Sleep(100 * time.Microsecond)
			}
		})
		if err != nil {
			t.Errorf("writer: %v", err)
		}
	}()

	<-writerInside
	snapshotsOk := make(chan struct{})
	go func() {
		defer close(snapshotsOk)
		for range 25 {
			store, err := ReadTempStoreUnlocked()
			if err != nil {
				t.Errorf("reader snapshot: %v", err)
				return
			}
			if !reflect.DeepEqual(store, storeA) && !reflect.DeepEqual(store, storeB) {
				t.Errorf("reader saw a torn/incomplete snapshot: %#v", store)
				return
			}
		}
	}()
	select {
	case <-snapshotsOk:
	case <-time.After(15 * time.Second):
		t.Fatal("reader blocked on the exclusive registry lock")
	}
	close(readerDone)
	wg.Wait()

	final, err := ReadTempStoreUnlocked()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, storeA) && !reflect.DeepEqual(final, storeB) {
		t.Fatalf("final store = %#v, want one committed state", final)
	}
}

func TestValidTempIdentity(t *testing.T) {
	valid := fixtureStoreIdentity
	if !validTempIdentity(valid) {
		t.Fatalf("%q must be a valid identity fingerprint", valid)
	}
	for _, bad := range []string{"", "abc", valid[:62], strings.Repeat("g", 64)} {
		if validTempIdentity(bad) {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}
