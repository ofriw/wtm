package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func pinRates(t *testing.T) {
	t.Helper()
	path, err := filepath.Abs("test/fixtures/rates.json")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WTM_RATES", path)
}

func subscriptionBinaryRepo(t *testing.T) (string, sandboxEnv) {
	t.Helper()
	env := sandbox(t)
	pinRates(t)
	// sandbox already clears the production rule list; this keeps the binary
	// test explicit if the sandbox changes.
	for _, key := range claudeMeteredKeys() {
		t.Setenv(key, "")
	}
	writeClaudeAccount(t, "stripe_subscription", "2025-01-01T00:00:00Z")
	repo := nativeRepo(t)
	seedSubscriptionSession(t, repo, "main", `{"input_tokens":100000}`)
	return repo, env
}

func seedSubscriptionSession(t *testing.T, repo, name, usage string) {
	t.Helper()
	path := mkClaudeSession(t, repo, name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf("{\"type\":\"queue-operation\",\"cwd\":null}\n{\"type\":\"user\",\"cwd\":%q}\n", repo)
	body += `{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":` + usage + "}}\n"
	writeFile(t, path, body, 0o644)
}

func subscriptionStatus(t *testing.T, repo string) []map[string]any {
	t.Helper()
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, out)
	}
	return rows
}

func subscriptionObject(t *testing.T, value any) map[string]any {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("expected JSON object, got %#v", value)
	}
	return object
}

func assertSubscriptionPrices(t *testing.T, row, want map[string]any) {
	t.Helper()
	usage := subscriptionObject(t, row["usage"])
	harness := subscriptionObject(t, usage["claude"])
	models := subscriptionObject(t, harness["models"])
	model := subscriptionObject(t, models["claude-sonnet-4-6"])
	for level, object := range map[string]map[string]any{"worktree": row, "harness": harness, "model": model} {
		got := map[string]any{}
		for _, key := range []string{"cost", "listCost", "costEstimated", "listCostEstimated"} {
			if value, present := object[key]; present {
				got[key] = value
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s prices = %#v, want %#v", level, got, want)
		}
	}
}

func assertSingleSubscription(t *testing.T, repo string, covered bool) {
	t.Helper()
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	assertSubscriptionPrices(t, rows[0], subscriptionPrices(covered))
}

func subscriptionPrices(covered bool) map[string]any {
	// The fixture charges $3/million input tokens: 100000 tokens cost $0.30.
	if covered {
		return map[string]any{"cost": float64(0), "listCost": 0.3}
	}
	return map[string]any{"cost": 0.3}
}

func subscriptionSettingsPath(repo string, env sandboxEnv, scope, file string) string {
	if scope == "user" {
		return filepath.Join(env.ClaudeDir, file)
	}
	return filepath.Join(repo, ".claude", file)
}

func seedSubscriptionSettings(t *testing.T, path, state string) {
	t.Helper()
	switch state {
	case "missing":
		return
	case "malformed":
		writeFile(t, path, `{"env":`, 0o600)
	case "directory":
		// A directory produces a read error even for privileged CI users.
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown settings state %q", state)
	}
}

func applySubscriptionOverride(t *testing.T, repo string, env sandboxEnv, key, scope, file string) {
	t.Helper()
	if scope == "process" {
		t.Setenv(key, "1")
		return
	}
	body := fmt.Sprintf(`{"env":{%q:"1"}}`, key)
	writeFile(t, subscriptionSettingsPath(repo, env, scope, file), body, 0o600)
}

func TestSubscriptionBinaryOverrides(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_FOUNDRY"} {
		for _, scope := range []string{"process", "user", "project"} {
			for _, file := range []string{"settings.json", "settings.local.json"} {
				if scope == "process" && file == "settings.local.json" {
					continue
				}
				t.Run(key+"/"+scope+"/"+file, func(t *testing.T) {
					repo, env := subscriptionBinaryRepo(t)
					applySubscriptionOverride(t, repo, env, key, scope, file)
					assertSingleSubscription(t, repo, false)
				})
			}
		}
	}
}

func TestSubscriptionBinarySettingsFailures(t *testing.T) {
	for _, scope := range []string{"user", "project"} {
		for _, file := range []string{"settings.json", "settings.local.json"} {
			for _, state := range []string{"missing", "malformed", "directory"} {
				t.Run(scope+"/"+file+"/"+state, func(t *testing.T) {
					repo, env := subscriptionBinaryRepo(t)
					seedSubscriptionSettings(t, subscriptionSettingsPath(repo, env, scope, file), state)
					assertSingleSubscription(t, repo, state == "missing")
				})
			}
		}
	}
}

func subscriptionRealPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return resolved
}

func assertIsolatedSubscription(t *testing.T, repo, overridden string) {
	t.Helper()
	repo, overridden = subscriptionRealPath(t, repo), subscriptionRealPath(t, overridden)
	rows := subscriptionStatus(t, repo)
	if len(rows) != 2 {
		t.Fatalf("got %d worktrees, want 2", len(rows))
	}
	seen := map[string]bool{}
	for _, row := range rows {
		path, ok := row["path"].(string)
		if !ok || (path != repo && path != overridden) || seen[path] {
			t.Fatalf("unexpected worktree path: %#v", row["path"])
		}
		seen[path] = true
		assertSubscriptionPrices(t, row, subscriptionPrices(path != overridden))
	}
}

func TestSubscriptionBinaryWorktreeIsolation(t *testing.T) {
	for _, file := range []string{"settings.json", "settings.local.json"} {
		for _, override := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_FOUNDRY", "malformed", "directory"} {
			t.Run(file+"/"+override, func(t *testing.T) {
				repo, env := subscriptionBinaryRepo(t)
				other := filepath.Join(t.TempDir(), "other")
				gitWorktreeAdd(t, repo, other, "other", "main")
				seedSubscriptionSession(t, other, "other", `{"input_tokens":100000}`)
				path := subscriptionSettingsPath(other, env, "project", file)
				if override == "malformed" || override == "directory" {
					seedSubscriptionSettings(t, path, override)
				} else {
					applySubscriptionOverride(t, other, env, override, "project", file)
				}
				assertIsolatedSubscription(t, repo, other)
			})
		}
	}
}

func TestSubscriptionBinaryCoveredCacheWriteEstimate(t *testing.T) {
	repo, _ := subscriptionBinaryRepo(t)
	seedSubscriptionSession(t, repo, "main", `{"input_tokens":100000,"cache_creation_input_tokens":100000}`)
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	// Missing cache TTL uses the fixture's 5-minute rate: $0.30 + $0.375.
	assertSubscriptionPrices(t, rows[0], map[string]any{
		"cost": float64(0), "listCost": 0.675, "listCostEstimated": true,
	})
}
