//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// TestReadUsageFIFODoesNotBlock pins that the billing pass opens transcripts on
// the guarded nonblocking handle: a file replaced by a FIFO after discovery must
// fail fast, never block the whole report.
func TestReadUsageFIFODoesNotBlock(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "s.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unavailable: %v", err)
	}
	done := make(chan struct{})
	var err error
	go func() {
		_, err = readUsage(claudeAgent, fifo)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("readUsage blocked on a FIFO")
	}
	if err == nil {
		t.Fatal("readUsage succeeded on a FIFO, want an error")
	}
	if !errors.Is(err, errNotRegularFile) {
		t.Fatalf("readUsage err = %v, want errNotRegularFile", err)
	}
	if _, serr := os.Lstat(fifo); serr != nil {
		t.Fatalf("fixture FIFO disappeared: %v", serr)
	}
}
