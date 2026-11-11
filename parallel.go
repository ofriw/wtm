package main

import (
	"runtime"
	"sync"
)

// parallel.go — the one bounded worker pool. Discovery is I/O bound (session
// head reads, per-worktree config reads), so the pool size is fixed by
// measurement rather than by NumCPU alone: 4 workers cut the owner-read walk
// from 4.5s to 0.5s, and 8/16 workers add nothing because the disk is the
// limit. Keeping the pool here keeps every concurrent stage at one tuning point.

// scanWorkers is the pool size for read-only discovery stages.
var scanWorkers = min(8, max(4, runtime.NumCPU()))

// mapParallel runs fn for every index in [0,n) with a bounded pool and returns
// the results in index order. The contract is deliberate: results are keyed by
// index so callers keep deterministic output, and workers must not write files
// or stderr — they return what they learned and the caller emits it in order.
func mapParallel[T any](n, workers int, fn func(i int) T) []T {
	out := make([]T, n)
	if n == 0 {
		return out
	}
	idx := make(chan int)
	var wg sync.WaitGroup
	workers = min(max(workers, 1), n)
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for i := range idx {
				out[i] = fn(i)
			}
		}()
	}
	for i := range n {
		idx <- i
	}
	close(idx)
	wg.Wait()
	return out
}
