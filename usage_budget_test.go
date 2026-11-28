package main

import (
	"strings"
	"testing"
)

// TestUsageBudgetAllow pins the budget reservation semantics: reservations
// succeed only while they fit the remaining bytes, never drive it negative, and
// a nil budget disables the cap.
func TestUsageBudgetAllow(t *testing.T) {
	b := newUsageBudget(10)
	if !b.allow(10) {
		t.Fatal("allow(10) = false on a fresh 10-byte budget, want true")
	}
	if b.allow(1) {
		t.Fatal("allow(1) = true after spending the whole budget, want false")
	}
	b2 := newUsageBudget(10)
	if !b2.allow(6) {
		t.Fatal("allow(6) = false")
	}
	if !b2.allow(4) {
		t.Fatal("allow(4) = false after allow(6)")
	}
	if b2.allow(1) {
		t.Fatal("allow(1) = true after spending the whole budget, want false")
	}
	// A nil budget is the disabled cap: every file is allowed.
	if !budgetAllows(nil, "whatever") {
		t.Fatal("nil budget refused a file")
	}
}

// TestBuildUsageOverBudgetSkipped pins the scale guard: once the run budget is
// spent, sessions are skipped whole and the totals taint partial instead of
// reading as complete; a nil budget leaves the same report clean.
func TestBuildUsageOverBudgetSkipped(t *testing.T) {
	dir, idx := claudeWorktreeDir(t)
	parts := partitionSessions([]worktree{{Path: dir}}, idx)
	u, _, err := buildUsageSessionsReport(dir, parts[canonical(dir)], testRates(t), loadSubscriptions(true), nowFunc(), newUsageBudget(4))
	if err == nil || !strings.Contains(err.Error(), "byte budget") {
		t.Fatalf("err = %v, want the budget skip cause", err)
	}
	if !u.partial {
		t.Error("usage partial = false, want true (a skipped session must taint)")
	}
	u2, _, err2 := buildUsageSessionsReport(dir, parts[canonical(dir)], testRates(t), loadSubscriptions(true), nowFunc(), nil)
	if err2 != nil {
		t.Fatalf("nil budget err = %v", err2)
	}
	if u2.partial {
		t.Error("usage partial = true with nil budget, want false")
	}
}

// TestUsageBudgetEnvSeams pins the env parsing: an explicit 0 disables the cap
// and WTM_STATUS_SKIP_USAGE=1 opts out of loading.
func TestUsageBudgetEnvSeams(t *testing.T) {
	t.Setenv(usageByteBudgetEnv, "0")
	if b := usageBudgetFromEnv(nil); b != nil {
		t.Fatal("WTM_USAGE_BYTE_BUDGET=0 produced a budget, want nil")
	}
	t.Setenv(usageByteBudgetEnv, "abc")
	if b := usageBudgetFromEnv(nil); b == nil {
		t.Fatal("malformed budget env disabled the cap, want the default")
	}
	t.Setenv(statusSkipUsageEnv, "1")
	if !statusSkipUsage() {
		t.Fatal("WTM_STATUS_SKIP_USAGE=1 did not skip usage")
	}
	t.Setenv(statusSkipUsageEnv, "0")
	if statusSkipUsage() {
		t.Fatal("WTM_STATUS_SKIP_USAGE=0 skipped usage")
	}
}
