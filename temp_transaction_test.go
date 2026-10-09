package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTempWorktree(t *testing.T) {
	_, _, w := identityCheckout(t)
	assertTempRemoval(t, w, true)
}

func TestRemoveTempWorktreeRejectsChangedRecord(t *testing.T) {
	_, path, w := identityCheckout(t)
	seedTempRecord(t, path, w.Branch, "1h", tempNow)
	want := mustReadTempStore(t)
	if removed, _, err := removeTempWorktreeIf(w, false, tempNow); removed || err == nil {
		t.Fatalf("stale removal = %v, %v", removed, err)
	}
	assertTempStore(t, want)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestRemoveTempWorktreeFailurePreservesRecord(t *testing.T) {
	repo, path, w := identityCheckout(t)
	mustGit(t, repo, "worktree", "lock", path)
	want := mustReadTempStore(t)
	if removed, _, err := removeTempWorktreeIf(w, false, tempNow); removed || err == nil {
		t.Fatalf("locked removal = %v, %v", removed, err)
	}
	assertTempStore(t, want)
	if err := verifyTempWorktree(w); err != nil {
		t.Fatalf("failed removal changed identity: %v", err)
	}
}

func TestRemoveTempWorktreeRegistryWriteFailure(t *testing.T) {
	_, path, w := identityCheckout(t)
	restrictRegistryWrites(t)
	removed, _, err := removeTempWorktreeIf(w, false, tempNow)
	if !removed || err == nil {
		t.Fatalf("write-failed removal = %v, %v", removed, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("checkout remains: %v", err)
	}
	if rec := mustReadTempStore(t)[w.Path]; rec.Identity != w.TempIdentity {
		t.Fatal("failed registry write changed record")
	}
}

type tempRemovalOutcome struct {
	removed bool
	err     error
}

// The start gate permits concurrent transactions but cannot force OS lock
// contention. Deterministic order tests below cover both serial outcomes.
func runTempContenders(t *testing.T, fns ...func()) {
	t.Helper()
	start := make(chan struct{})
	for _, fn := range fns {
		go func(fn func()) {
			<-start
			fn()
		}(fn)
	}
	close(start)
}

func TestTempPromotionRemovalOrders(t *testing.T) {
	for _, first := range []string{"promotion", "removal"} {
		t.Run(first+"-first", func(t *testing.T) {
			_, path, w := identityCheckout(t)
			if first == "promotion" {
				assertTempPromotion(t, path, true)
				assertTempRemoval(t, w, false)
				assertPermanentAfterReconcile(t, path, w.Branch)
				if _, err := repoRoot(path); err != nil {
					t.Fatalf("promoted checkout was removed: %v", err)
				}
				return
			}
			assertTempRemoval(t, w, true)
			assertTempPromotion(t, path, false)
			assertRemovedTempCheckout(t, path)
		})
	}
}

func TestTempReplacementRegistrationRemovalOrders(t *testing.T) {
	for _, first := range []string{"registration", "removal"} {
		t.Run(first+"-first", func(t *testing.T) {
			_, path, w := identityCheckout(t)
			if first == "registration" {
				assertTempRegistration(t, w, true)
				assertTempRemoval(t, w, false)
				assertRegisteredCheckout(t, path, w)
				return
			}
			assertTempRemoval(t, w, true)
			assertTempRegistration(t, w, false)
			assertRemovedTempCheckout(t, path)
		})
	}
}

func assertTempPromotion(t *testing.T, path string, want bool) {
	t.Helper()
	if promoted, err := promoteTempRecord(path); promoted != want || err != nil {
		t.Fatalf("promotion = %v, %v; want %v, nil", promoted, err, want)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("promotion retained temp permission")
	}
}

func assertTempRemoval(t *testing.T, w worktree, want bool) {
	t.Helper()
	before := mustReadTempStore(t)
	removed, _, err := removeTempWorktreeIf(w, false, tempNow)
	if removed != want || (want && err != nil) {
		t.Fatalf("removal = %v, %v; want %v", removed, err, want)
	}
	if want {
		assertRemovedTempCheckout(t, w.Path)
		return
	}
	if err == nil {
		t.Fatalf("stale snapshot was not refused: %v", err)
	}
	assertTempStore(t, before)
	if _, err := os.Stat(w.Path); err != nil {
		t.Fatalf("refused removal changed checkout: %v", err)
	}
}

func assertTempRegistration(t *testing.T, w worktree, want bool) {
	t.Helper()
	before := mustReadTempStore(t)
	_, err := addTempRecord(w.Path, tempRecord{CreatedAt: tempNow, TTL: "1h", Branch: w.Branch})
	if (err == nil) != want {
		t.Fatalf("registration error = %v; want success %v", err, want)
	}
	if !want {
		assertTempStore(t, before)
		return
	}
	assertRegisteredCheckout(t, w.Path, w)
}

func assertRemovedTempCheckout(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("removed checkout remains: %v", err)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("removed checkout retained registry permission")
	}
}

func TestTempRemovalConcurrentPromotion(t *testing.T) {
	_, path, w := identityCheckout(t)
	removal := make(chan tempRemovalOutcome, 1)
	promotion := make(chan tempRemovalOutcome, 1)
	runTempContenders(t,
		func() { ok, _, err := removeTempWorktreeIf(w, false, tempNow); removal <- tempRemovalOutcome{ok, err} },
		func() { ok, err := promoteTempRecord(w.Path); promotion <- tempRemovalOutcome{ok, err} },
	)
	r, p := <-removal, <-promotion
	if p.err != nil || r.removed == p.removed || (r.removed && r.err != nil) || (p.removed && r.err == nil) {
		t.Fatalf("removal %+v, promotion %+v: want removal or promotion to win", r, p)
	}
	if exists(path) != p.removed {
		t.Fatalf("checkout existence differs from promotion outcome: %+v", p)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("completed transactions retained temp permission")
	}
	if promoted, err := promoteTempRecord(path); promoted || err != nil {
		t.Fatalf("idempotent promotion = %v, %v", promoted, err)
	}
}

func TestTempRemovalConcurrentRegistration(t *testing.T) {
	_, path, w := identityCheckout(t)
	removal := make(chan tempRemovalOutcome, 1)
	registration := make(chan error, 1)
	runTempContenders(t,
		func() { ok, _, err := removeTempWorktreeIf(w, false, tempNow); removal <- tempRemovalOutcome{ok, err} },
		func() {
			_, err := addTempRecord(path, tempRecord{CreatedAt: tempNow, TTL: "1h", Branch: w.Branch})
			registration <- err
		},
	)
	r, err := <-removal, <-registration
	if r.removed == (err == nil) {
		t.Fatalf("removal %+v, registration %v: want one successful transaction", r, err)
	}
	if err == nil {
		assertRegisteredCheckout(t, path, w)
	}
}

func assertRegisteredCheckout(t *testing.T, path string, old worktree) {
	t.Helper()
	rec := mustReadTempStore(t)[old.Path]
	if rec.Identity == old.TempIdentity {
		t.Fatal("replacement registration reused identity")
	}
	if err := verifyTempIdentity(path, rec); err != nil {
		t.Fatalf("registered checkout was removed: %v", err)
	}
}

func TestReconcilePreservesUnrelatedRepositoryGitFailure(t *testing.T) {
	_, _, _ = identityCheckout(t)
	other := initRepo(t, "unrelated")
	seedTempRecord(t, other, "unrelated", "1h", tempTestTime)
	want := mustReadTempStore(t)
	if err := os.WriteFile(filepath.Join(other, ".git", "config"), []byte("[invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	gone := canonical(filepath.Join(t.TempDir(), "gone"))
	seedTempRecord(t, gone, "tmp/gone", "1h", tempTestTime)
	assertErrorNamesPath(t, "unrelated Git failure was not reported", reconcileTempStore(), other)
	assertTempStore(t, want)
}

func TestReconcilePreservesFilesystemFailure(t *testing.T) {
	_, path, _ := identityCheckout(t)
	token, err := tempIdentityPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(token); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(token, 0700); err != nil {
		t.Fatal(err)
	}
	want := mustReadTempStore(t)
	assertErrorNamesPath(t, "identity read failure was not reported", reconcileTempStore(), path)
	assertTempStore(t, want)
}

func removeCheckoutMetadata(t *testing.T, path, target string) {
	t.Helper()
	p := filepath.Join(path, ".git")
	if target == "identity" {
		var err error
		p, err = tempIdentityPath(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileMissingMetadata(t *testing.T) {
	for _, target := range []string{"identity", "git"} {
		t.Run(target, func(t *testing.T) {
			_, path, _ := identityCheckout(t)
			removeCheckoutMetadata(t, path, target)
			if err := reconcileTempStore(); err != nil {
				t.Fatal(err)
			}
			if len(mustReadTempStore(t)) != 0 {
				t.Fatal("proven missing metadata survived")
			}
		})
	}
}

// startContender runs fn in a goroutine and returns after it started,
// mirroring the child processes' "ready" handshake. Readiness means fn started,
// not that it holds no lock: the holder guarantees contention until release.
func startContender(fn func()) {
	ready := make(chan struct{})
	go func() {
		close(ready)
		fn()
	}()
	<-ready
}

// TestReconcileDuringAdd verifies reconcileTempStore waits when another
// process holds the registry lock.
func TestReconcileDuringAdd(t *testing.T) {
	sandboxTempProcesses(t)
	base := t.TempDir()
	keep := filepath.Join(base, "keep")
	mustProcessError(t, storeTempFixture(keep, fixtureRecord("tmp/keep")))
	holder := startTempProcess(t, "hold-lock", base)
	holder.expect(t, "locked")
	reconciled := make(chan error, 1)
	startContender(func() { reconciled <- reconcileTempStore() })
	assertTempProcessContention(t, base)
	releaseTempProcess(t, holder)
	if err := <-reconciled; err != nil {
		t.Fatalf("reconcile during add: %v", err)
	}
	if _, ok := mustReadTempStore(t)[keep]; ok {
		t.Fatal("reconcile retained a stale record while lock was held")
	}
}

// TestReconcileDuringRemove verifies removeTempWorktreeIf waits when another
// process holds the registry lock.
func TestReconcileDuringRemove(t *testing.T) {
	sandboxTempProcesses(t)
	_, _, w := identityCheckout(t)
	holder := startTempProcess(t, "hold-lock", w.Path)
	holder.expect(t, "locked")
	removeDone := make(chan tempRemovalOutcome, 1)
	startContender(func() {
		removed, _, err := removeTempWorktreeIf(w, false, tempNow)
		removeDone <- tempRemovalOutcome{removed, err}
	})
	assertTempProcessContention(t, w.Path)
	releaseTempProcess(t, holder)
	r := <-removeDone
	if !r.removed || r.err != nil {
		t.Fatalf("remove during reconcile: %+v", r)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("concurrent reconcile/remove retained temp record")
	}
}

// TestIdleCheckContention verifies gcCleanup with an idle temp waits when
// another process holds the registry lock.
func TestIdleCheckContention(t *testing.T) {
	sandboxTempProcesses(t)
	repo, w, _ := idleTempCheckout(t)
	idx := cleanupSessionIndex(t, repo)
	holder := startTempProcess(t, "hold-lock", repo)
	holder.expect(t, "locked")
	gcDone := make(chan gcResult, 1)
	startContender(func() {
		var res gcResult
		var err error
		err = withProgress(&globals{json: true}, true, func(p *progress) error {
			res, err = gcCleanup(repo, []worktree{w}, remotePlan{keep: true}, false, idx, gcMode{deleteTemp: true, idleOnly: true}, tempNow, p)
			return err
		})
		if err != nil {
			gcDone <- gcResult{}
			return
		}
		gcDone <- res
	})
	assertTempProcessContention(t, repo)
	releaseTempProcess(t, holder)
	res := <-gcDone
	if len(res.Removed) != 1 || res.Removed[0] != w.Path {
		t.Fatalf("idle gc under contention = %+v", res)
	}
	if len(mustReadTempStore(t)) != 0 {
		t.Fatal("idle gc did not clear the temp record")
	}
}
