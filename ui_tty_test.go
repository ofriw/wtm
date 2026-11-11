//go:build !windows

package main

import (
	"bufio"
	"os"
	"strings"
	"testing"
	"time"
)

// Real, fixed-size terminals exercise fallback without host-terminal state.
func TestWidthBudgetTTY(t *testing.T) {
	for _, tc := range []struct {
		name, columns string
		want          int
	}{
		{"positive override", "80", 80},
		{"invalid fallback", "abc", 37},
		{"zero fallback", "0", 37},
		{"negative fallback", "-5", 37},
		{"unset fallback", "", 37},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertTTYBudget(t, tc.columns, 37, tc.want)
		})
	}
}

func assertTTYBudget(t *testing.T, columns string, width, want int) {
	t.Helper()
	t.Setenv("COLUMNS", columns)
	if columns == "" {
		if err := os.Unsetenv("COLUMNS"); err != nil {
			t.Fatal(err)
		}
	}
	_, slave := openTTY(t, width)
	withStdout(t, slave, func() {
		if got := widthBudget(); got != want {
			t.Fatalf("widthBudget() = %d, want %d", got, want)
		}
	})
}

func TestWidthBudgetZeroSizeTTY(t *testing.T) {
	assertTTYBudget(t, "", 0, 0)
}

func TestExplicitRenderBudgetSurvivesCapture(t *testing.T) {
	t.Setenv("COLUMNS", "")
	for _, budget := range []int{0, 20, 80} {
		render := func() {
			rows := [][]string{{"path", "/a/long/checkout/path"}}
			printTable([]string{"LABEL", "VALUE"}, rows, colorNever, nil, nil, budget)
			printKeyValues(rows, 1, colorNever, budget)
		}
		want := captureStdout(t, render)
		if got := captureTTYStdout(t, 5, render); got != want {
			t.Fatalf("budget %d changed between pipe and TTY:\n got: %q\nwant: %q", budget, got, want)
		}
	}
}

func captureTTYStdout(t *testing.T, width int, render func()) string {
	t.Helper()
	master, slave := openTTY(t, width)
	// Read before closing the slave: some PTYs discard queued output on close.
	done := readTTYCapture(master)
	withStdout(t, slave, render)
	if _, err := slave.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		return strings.ReplaceAll(result.text, "\r\n", "\n")
	case <-time.After(5 * time.Second):
		t.Fatal("PTY capture timed out")
		return ""
	}
}

type ttyCapture struct {
	text string
	err  error
}

func readTTYCapture(master *os.File) <-chan ttyCapture {
	done := make(chan ttyCapture, 1)
	go func() {
		out, err := bufio.NewReader(master).ReadString('\x00')
		done <- ttyCapture{strings.TrimSuffix(out, "\x00"), err}
	}()
	return done
}

func TestExplicitBudgetSurvivesCapture(t *testing.T) {
	t.Setenv("COLUMNS", "")
	_, slave := openTTY(t, 5)
	wts := []worktree{{Path: "/with", Usage: estimatedUsage()}}
	cols := statusSchema(false, wts)
	withStdout(t, slave, func() {
		before := costLegendNeeded(cols, wts, "/elsewhere", at(0), 10*time.Hour, 0)
		captureStdout(t, func() {
			after := costLegendNeeded(cols, wts, "/elsewhere", at(0), 10*time.Hour, 0)
			if before != after || !after.tableReasons.any() {
				t.Fatalf("capture changed explicit unbounded layout: before=%+v after=%+v", before, after)
			}
		})
	})
}
