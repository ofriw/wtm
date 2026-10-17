package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestExplicitLayoutIgnoresColumns(t *testing.T) {
	wts := []worktree{{Path: "/with", Usage: estimatedUsage()}}
	for _, budget := range []int{0, 20, 80} {
		t.Run(fmt.Sprint(budget), func(t *testing.T) {
			t.Setenv("COLUMNS", "240")
			want := captureStatusLayout(t, wts, budget)
			for _, columns := range []string{"5", "20", "abc", "0", ""} {
				t.Setenv("COLUMNS", columns)
				if got := captureStatusLayout(t, wts, budget); got != want {
					t.Fatalf("COLUMNS=%q changed explicit budget %d:\n got: %q\nwant: %q", columns, budget, got, want)
				}
			}
		})
	}
}

func captureStatusLayout(t *testing.T, wts []worktree, budget int) [2]string {
	t.Helper()
	var stderr string
	stdout := captureStdout(t, func() {
		stderr = captureStderr(t, func() {
			cols := statusSchema(false, wts)
			printStatusTable(cols, wts, at(0), 10*time.Hour, colorNever, budget)
			legend := costLegendNeeded(cols, wts, "/with", at(0), 10*time.Hour, budget)
			printCostLegend(legend, colorNever, budget)
			printStatusDetail(wts, "/with", at(0), colorNever, budget)
		})
	})
	return [2]string{stdout, stderr}
}

func TestCLIChildTerminalIsolation(t *testing.T) {
	sandbox(t)
	repo := initRepo(t, "main")
	wantOut, wantErr, wantRC := runWTM(t, repo, nil, "status", "--color=never")
	for _, key := range []string{"COLUMNS", "LINES", "TERM", "COLORTERM", "NO_COLOR", "CLICOLOR", "CLICOLOR_FORCE"} {
		t.Setenv(key, "5")
	}
	out, stderr, rc := runWTM(t, repo, nil, "status", "--color=never")
	if out != wantOut || stderr != wantErr || rc != wantRC || rc != 0 {
		t.Fatalf("ambient terminal state changed CLI output: rc=%d stderr=%q stdout=%q", rc, stderr, out)
	}
	out, stderr, rc = runWTM(t, repo, []string{"COLUMNS=5"}, "status", "--color=never")
	if rc != 0 || out == wantOut || !strings.Contains(stderr, "terminal too narrow") {
		t.Fatalf("explicit terminal override was not applied: rc=%d stderr=%q stdout=%q", rc, stderr, out)
	}
}
