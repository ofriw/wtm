package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seedUnknownClaudeSession(t *testing.T, repo, model string) {
	t.Helper()
	path := mkClaudeSession(t, repo, "unknown", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf("{\"type\":\"user\",\"cwd\":%q}\n", repo)
	body += fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T00:00:01Z","requestId":"r2","message":{"id":"m2","model":%q,"usage":{"input_tokens":1,"service_tier":"unpriced"}}}`+"\n", model)
	writeFile(t, path, body, 0o644)
}

func estimateBinaryRepo(t *testing.T, model string) string {
	t.Helper()
	repo, _ := subscriptionBinaryRepo(t)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "metered-test")
	seedSubscriptionSession(t, repo, "main", `{"input_tokens":100000,"cache_creation_input_tokens":100000}`)
	seedUnknownClaudeSession(t, repo, model)
	return repo
}

func assertUnknownEstimateTotal(t *testing.T, object map[string]any) {
	t.Helper()
	if v, present := object["cost"]; !present || v != nil {
		t.Errorf("unknown cost = %v (present %v), want null", v, present)
	}
	for _, key := range []string{"costEstimated", "listCostEstimated"} {
		if _, present := object[key]; present {
			t.Errorf("unknown total must omit %s: %#v", key, object)
		}
	}
}

func assertEstimateAggregateJSON(t *testing.T, repo string, sameModel bool) {
	t.Helper()
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	h := subscriptionObject(t, subscriptionObject(t, rows[0]["usage"])["claude"])
	assertUnknownEstimateTotal(t, rows[0])
	assertUnknownEstimateTotal(t, h)
	m := subscriptionObject(t, subscriptionObject(t, h["models"])["claude-sonnet-4-6"])
	if sameModel {
		assertUnknownEstimateTotal(t, m)
	} else {
		assertUnknownEstimateTotal(t, subscriptionObject(t, subscriptionObject(t, h["models"])["unpriced-model"]))
		if m["cost"] != 0.675 || m["costEstimated"] != true {
			t.Errorf("estimated model must keep its numeric caveat: %#v", m)
		}
	}
}

// assertEstimateLegend pins the "~ estimated:" line against the visible "~"
// marks. When a mark is expected (want), its amount, the cache-write reason and
// the lower-bound caveat must all render, so the legend can never drift from
// the estimate it explains.
func assertEstimateLegend(t *testing.T, repo string, want bool) {
	t.Helper()
	out, stderr, rc := runWTM(t, repo, nil, "status", "--color=never")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if got := strings.Contains(out, "~ estimated:"); got != want {
		t.Errorf("legend present = %v, want %v:\n%s", got, want, out)
	}
	if want && (!strings.Contains(out, "~$") || !strings.Contains(out, "5-minute rate") || !strings.Contains(out, "lower bound")) {
		t.Errorf("visible estimate needs its amount and explanation:\n%s", out)
	}
}

func TestEstimateBinaryUnknownTotals(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4-6", "unpriced-model"} {
		t.Run(model, func(t *testing.T) {
			repo := estimateBinaryRepo(t, model)
			assertEstimateAggregateJSON(t, repo, model == "claude-sonnet-4-6")
			// The claude-sonnet-4-6 variant keeps an unknown aggregate total, so its
			// only estimate hides in per-tier costs and leaves no "~" cell. The
			// unpriced-model variant knows its claude total ($0.675, estimated), so
			// that total renders the legend.
			assertEstimateLegend(t, repo, model != "claude-sonnet-4-6")
		})
	}
}

func TestEstimateBinaryUnknownListOmitsFlags(t *testing.T) {
	repo := estimateBinaryRepo(t, "claude-sonnet-4-6")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "")
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	assertSubscriptionPrices(t, rows[0], map[string]any{"cost": float64(0), "listCost": nil})
	// Per-tier costs are no longer rendered and the aggregate list stays unknown,
	// so the 5-minute write estimate leaves no "~" cell for a legend to explain.
	assertEstimateLegend(t, repo, false)
}

func TestEstimateBinaryHiddenModelsNeedNoLegend(t *testing.T) {
	repo := estimateBinaryRepo(t, "unpriced-model")
	other := filepath.Join(t.TempDir(), "other")
	gitWorktreeAdd(t, repo, other, "other", "main")
	// Only unknown totals appear for the non-current worktree; its model rows
	// are not rendered, so their estimate must not cause a legend.
	assertEstimateLegend(t, other, false)
}
