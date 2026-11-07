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

func TestRunProgressProgramInterruptIsBounded(t *testing.T) {
	interruptGrace = 50 * time.Millisecond
	defer func(old time.Duration) { interruptGrace = old }(interruptGrace)
	keep := watchInterrupts() // keeps self-delivery harmless after the renderer stops
	defer signal.Stop(keep)
	release, returned := make(chan struct{}), make(chan error, 1)
	defer close(release)
	go func() {
		returned <- runProgressProgram(io.Discard, progressOptions{}, func(*progress) error {
			<-release
			return nil
		})
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		select {
		case err := <-returned:
			if err != errInterrupted {
				t.Fatalf("interrupted run = %v, want errInterrupted", err)
			}
			return
		case <-time.After(5 * time.Millisecond):
			if time.Now().After(deadline) {
				t.Fatal("interrupted run did not return within the grace bound")
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
