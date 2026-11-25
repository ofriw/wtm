//go:build !windows

package main

import (
	"context"
	"io"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

func TestProgressInterruptRunIsBounded(t *testing.T) {
	keep := watchInterrupts() // keeps self-delivery harmless after the renderer stops
	defer signal.Stop(keep)
	release, returned := make(chan struct{}), make(chan error, 1)
	defer close(release)
	go func() {
		returned <- runProgressProgram(io.Discard, progressOptions{interruptGrace: 50 * time.Millisecond}, func(*progress) error {
			<-release
			return nil
		})
	}()
	if err := interruptUntilKill(t, returned, "interrupted run"); err != errInterrupted {
		t.Fatalf("interrupted run = %v, want errInterrupted", err)
	}
}

// interruptUntilKill self-delivers SIGTERM until the run reports, driving the
// renderer's interrupt path without a PTY.
func interruptUntilKill(t *testing.T, res <-chan error, what string) error {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); ; {
		select {
		case err := <-res:
			return err
		case <-time.After(5 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatalf("%s did not return within the grace bound", what)
			}
			_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		}
	}
}

func TestChildCommandDiesOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	childCtx.Store(&ctx)
	defer childCtx.Store(nil)
	cmd := childCommand("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	cancel()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancelled child exited cleanly")
		}
	case <-time.After(2 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("child survived cancellation")
	}
}
