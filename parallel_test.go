package main

import (
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

// parallel_test.go — the pool contract and the determinism it must preserve:
// index content and order may not depend on how many workers ran.

// TestMapParallelKeepsIndexOrderAndBound pins the two pool promises: results
// come back in input order, and no more than the requested workers run at once.
func TestMapParallelKeepsIndexOrderAndBound(t *testing.T) {
	const n, workers = 64, 4
	var active, peak atomic.Int64
	out := mapParallel(n, workers, func(i int) int {
		now := active.Add(1)
		for {
			seen := peak.Load()
			if now <= seen || peak.CompareAndSwap(seen, now) {
				break
			}
		}
		time.Sleep(time.Millisecond)
		active.Add(-1)
		return i * 3
	})
	for i, got := range out {
		if want := i * 3; got != want {
			t.Fatalf("out[%d] = %d, want %d", i, got, want)
		}
	}
	if got := peak.Load(); got > workers {
		t.Errorf("peak concurrency = %d, want <= %d", got, workers)
	}
}

func TestMapParallelEmpty(t *testing.T) {
	called := false
	if out := mapParallel(0, 4, func(int) int { called = true; return 0 }); len(out) != 0 {
		t.Errorf("out = %v, want empty", out)
	}
	if called {
		t.Error("fn ran for an empty input")
	}
}

// indexPaths projects the index to its comparable content: owner cwd → ordered
// session paths. sessionRef holds a parse function, which DeepEqual cannot
// compare, and the order is exactly what "deterministic" means here.
func indexPaths(idx sessionIndex) map[string][]string {
	out := make(map[string][]string, len(idx))
	for cwd, refs := range idx {
		paths := make([]string, len(refs))
		for i, r := range refs {
			paths[i] = r.path
		}
		out[cwd] = paths
	}
	return out
}

// TestIndexSessionsIndependentOfWorkers pins that pooling the owner reads
// changes only the wall clock: the same sessions land under the same cwds in
// the same order, so LAST USED and the dedupe fold cannot drift with load.
// Boundary worker counts are covered too: > n and <= 0 both clamp.
func TestIndexSessionsIndependentOfWorkers(t *testing.T) {
	sandbox(t)
	wtA, wtB := t.TempDir(), t.TempDir()
	base := time.Now().Add(-time.Hour)
	for i := range 24 {
		mkSession(t, wtA, fmt.Sprintf("a%02d", i), base.Add(time.Duration(i)*time.Minute))
		mkClaudeSession(t, wtB, fmt.Sprintf("b%02d", i), base.Add(time.Duration(i)*time.Minute))
	}
	// Capture the product pool size before pinning the serial run: reading it
	// from the mutated global is what made both runs serial.
	defaultWorkers := scanWorkers
	serial := indexWithWorkers(t, 1)
	for _, workers := range []int{defaultWorkers, 4096, 0, -1} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			pooled := indexWithWorkers(t, workers)
			if !reflect.DeepEqual(indexPaths(serial), indexPaths(pooled)) {
				t.Errorf("pooled index differs from serial\nserial: %v\npooled: %v",
					indexPaths(serial), indexPaths(pooled))
			}
			for _, path := range []string{wtA, wtB} {
				if got, want := worktreeLastUsed(path, pooled, nil), worktreeLastUsed(path, serial, nil); !got.Equal(want) {
					t.Errorf("LAST USED for %s = %s, want %s (workers=%d)", path, got, want, workers)
				}
			}
		})
	}
}

// indexWithWorkers indexes the sandbox with a pinned pool size. The global is
// restored on return, so a later read can never observe a pinned value.
func indexWithWorkers(t *testing.T, workers int) sessionIndex {
	t.Helper()
	old := scanWorkers
	scanWorkers = workers
	defer func() { scanWorkers = old }()
	idx, err := indexSessions(agents(), sessionErrPolicy{failClosed: true})
	if err != nil {
		t.Fatalf("indexSessions(workers=%d): %v", workers, err)
	}
	return idx
}

// TestMapParallelClampsWorkerCount pins the pool boundaries: a worker count
// above n or at/below zero still runs every index exactly once, in order.
func TestMapParallelClampsWorkerCount(t *testing.T) {
	for _, workers := range []int{100, 0, -1} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			var runs atomic.Int64
			out := mapParallel(3, workers, func(i int) int {
				runs.Add(1)
				return i * 10
			})
			if want := []int{0, 10, 20}; !reflect.DeepEqual(out, want) {
				t.Errorf("out = %v, want %v", out, want)
			}
			if got := runs.Load(); got != 3 {
				t.Errorf("fn ran %d times, want 3", got)
			}
		})
	}
}
