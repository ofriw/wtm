package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
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
	seedClaudeModelSession(t, repo, name, "claude-sonnet-4-6", usage)
}

// seedClaudeModelSession seeds one Claude assistant turn for a chosen model, so
// a test can pin a catalog lookup (exact name, alias) at the CLI boundary.
func seedClaudeModelSession(t *testing.T, repo, name, model, usage string) {
	t.Helper()
	path := mkClaudeSession(t, repo, name, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	body := fmt.Sprintf("{\"type\":\"queue-operation\",\"cwd\":null}\n{\"type\":\"user\",\"cwd\":%q}\n", repo)
	body += fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":%q,"message":{"id":%q,"model":%q,"usage":`, "r-"+name, "m-"+name, model) + usage + "}}\n"
	writeFile(t, path, body, 0o644)
}

func subscriptionStatus(t *testing.T, repo string) []map[string]any {
	t.Helper()
	out, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	return statusRows(t, out)
}

// statusRows decodes `wtm status --json` stdout, the machine output contract.
func statusRows(t *testing.T, stdout string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(stdout), &rows); err != nil {
		t.Fatalf("invalid status JSON: %v\n%s", err, stdout)
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

// TestStatusDanglingPrimaryAccountVoidsFallbackCoverage pins  at the CLI
// boundary: a primary `.config.json` that is a dangling symlink voids the
// fallback proof, so a Claude event bills at its metered list price and stderr
// discloses the unreadable account instead of silently claiming coverage.
func TestStatusDanglingPrimaryAccountVoidsFallbackCoverage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	repo, env := subscriptionBinaryRepo(t)
	link := filepath.Join(env.ClaudeDir, ".config.json")
	if err := os.Symlink(filepath.Join(env.ClaudeDir, "missing-target"), link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if !strings.Contains(stderr, ".config.json") {
		t.Errorf("stderr must disclose the unreadable primary account:\n%s", stderr)
	}
	row := statusRows(t, stdout)[0]
	if got := row["cost"]; got != 0.3 {
		t.Errorf("cost = %v, want metered 0.3: the dangling primary must void fallback coverage", got)
	}
	// listCost is emitted only when a proven subscription makes billed and list
	// differ; metered output therefore omits it. Its absence is the fail-closed
	// signal: the dangling primary yields an exact billed zero.
	if _, present := row["listCost"]; present {
		t.Errorf("listCost present for a metered run; want omitted (no substitution)")
	}
}

// TestMixedPiDirDoesNotVoidClaudeCoverage pins  at the CLI boundary: a Pi
// session recorded from a sibling subdirectory whose own `.claude/settings.json`
// is malformed must not taint the Claude proof. The root Claude session stays
// covered (cost 0, listCost visible) and no list-price warning fires.
func TestMixedPiDirDoesNotVoidClaudeCoverage(t *testing.T) {
	repo, env := subscriptionBinaryRepo(t)
	sub := filepath.Join(repo, "pisub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, mkSession(t, sub, "pi", time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		piUsageBody(sub, `{"input":1,"cost":{"total":0.5}}`), 0o644)
	// Malformed Claude settings in the Pi-only directory must not affect proof.
	writeFile(t, subscriptionSettingsPath(sub, env, "project", "settings.json"), "{ not json", 0o600)
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	if strings.Contains(stderr, "claude costs use list price") {
		t.Errorf("the Pi-only directory must not void Claude coverage:\n%s", stderr)
	}
	row := statusRows(t, stdout)[0]
	claude := subscriptionObject(t, subscriptionObject(t, row["usage"])["claude"])
	if got := claude["cost"]; got != float64(0) {
		t.Errorf("claude cost = %v, want covered 0", got)
	}
	if got := claude["listCost"]; got != 0.3 {
		t.Errorf("claude listCost = %v, want 0.3", got)
	}
}

// TestStatusMeteredDuplicateNotSubstitutedByCoveredCopy pins  at the CLI
// boundary: when identical copies of a billed turn exist in a metered and a
// covered session, the metered copy must win the slot so the bill stays at list
// price. Transcript placement must not decide whether a subscription proves the
// charge.
func TestStatusMeteredDuplicateNotSubstitutedByCoveredCopy(t *testing.T) {
	repo, env := subscriptionBinaryRepo(t)
	sub := filepath.Join(repo, "metered-sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	// Metered settings for the subdirectory void the Claude proof there.
	writeFile(t, subscriptionSettingsPath(sub, env, "project", "settings.json"),
		`{"env":{"ANTHROPIC_AUTH_TOKEN":"1"}}`, 0o600)
	mt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	usage := `{"input_tokens":100000}`
	// dup writes one session transcript whose assistant record carries the same
	// request/message ids, model, timestamp, and tokens everywhere, so the two
	// copies collide on the same billed-turn key while living in different
	// session files and cwds.
	dup := func(cwd string) string {
		b := fmt.Sprintf("{\"type\":\"queue-operation\",\"cwd\":null}\n{\"type\":\"user\",\"cwd\":%q}\n", cwd)
		return b + fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r-dup","message":{"id":"m-dup","model":"claude-sonnet-4-6","usage":%s}}`, usage) + "\n"
	}
	// Baseline: only the metered sub copy exists (plus the covered main session
	// subscriptionBinaryRepo seeded), so the claude harness bills at list for
	// this turn.
	writeFile(t, mkClaudeSession(t, sub, "sess", mt), dup(sub), 0o644)
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	row := statusRows(t, stdout)[0]
	claude := subscriptionObject(t, subscriptionObject(t, row["usage"])["claude"])
	if got := claude["cost"]; got != 0.3 {
		t.Fatalf("baseline claude cost = %v, want metered 0.3: fix the fixture first", got)
	}
	// Add an exact copy of the same turn to the covered root session. The
	// matchedered copy must win the slot, so the bill stays at list price.
	writeFile(t, mkClaudeSession(t, repo, "sess2", mt), dup(repo), 0o644)
	stdout, stderr, rc = runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	row = statusRows(t, stdout)[0]
	claude = subscriptionObject(t, subscriptionObject(t, row["usage"])["claude"])
	if got := claude["cost"]; got != 0.3 {
		t.Errorf("claude cost = %v, want metered 0.3: a covered copy must not substitute coverage for a metered duplicate; stderr=%s", got, stderr)
	}
}

// TestStatusLargerCoveredDuplicateCannotEraseMeteredCharge pins  at the CLI
// boundary: when the covered copy of a billed turn reports more tokens than the
// metered copy, the larger score must not erase the metered evidence. The
// metered copy still wins the slot, so the bill stays at list price.
func TestStatusLargerCoveredDuplicateCannotEraseMeteredCharge(t *testing.T) {
	repo, env := subscriptionBinaryRepo(t)
	sub := filepath.Join(repo, "metered-sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, subscriptionSettingsPath(sub, env, "project", "settings.json"),
		`{"env":{"ANTHROPIC_AUTH_TOKEN":"1"}}`, 0o600)
	mt := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dup := func(cwd, tokens string) string {
		b := fmt.Sprintf("{\"type\":\"queue-operation\",\"cwd\":null}\n{\"type\":\"user\",\"cwd\":%q}\n", cwd)
		return b + fmt.Sprintf(`{"type":"assistant","timestamp":"2026-01-01T00:00:00Z","requestId":"r-dup","message":{"id":"m-dup","model":"claude-sonnet-4-6","usage":{"input_tokens":%s}}}`+"\n", tokens)
	}
	// The covered root copy reports more tokens (100001) than the metered sub
	// copy (100000), so the larger score belongs to the covered side. The
	// metered charge must still surface.
	writeFile(t, mkClaudeSession(t, repo, "sess-dup", mt), dup(repo, "100001"), 0o644)
	writeFile(t, mkClaudeSession(t, sub, "sess-dup", mt), dup(sub, "100000"), 0o644)
	stdout, stderr, rc := runWTM(t, repo, nil, "status", "--json")
	if rc != 0 {
		t.Fatalf("status exit %d: %s", rc, stderr)
	}
	row := statusRows(t, stdout)[0]
	claude := subscriptionObject(t, subscriptionObject(t, row["usage"])["claude"])
	if got := claude["cost"]; got != 0.3 {
		t.Errorf("claude cost = %v, want metered 0.3: a larger covered copy must not erase the metered charge; stderr=%s", got, stderr)
	}
}

// TestPiDualModeMeteredTwinWinsBothOrders pins  at the fold boundary: the
// same billed turn recorded under an OAuth-proven provider (anthropic) and an
// API-key provider (openai) must elect the metered copy regardless of which
// session file comes first, so transcript placement never decides whether a
// subscription proves the charge.
func TestPiDualModeMeteredTwinWinsBothOrders(t *testing.T) {
	sandbox(t)
	writeAuthJSON(t, `{"anthropic":{"type":"oauth"},"openai":{"type":"api_key"}}`)
	turn := func(provider string) string {
		return fmt.Sprintf(`{"type":"message","id":"t1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":%q,"model":"m","usage":{"cost":{"total":1}}}}`+"\n", provider)
	}
	for name, sessions := range map[string][]string{
		"covered-first": {turn("anthropic"), turn("openai")},
		"metered-first": {turn("openai"), turn("anthropic")},
	} {
		t.Run(name, func(t *testing.T) {
			u := piUsageWorktree(t, sessions...)
			if got := u.cost().cell(); got != "$1.00" {
				t.Fatalf("cost = %q, want the metered list price $1.00 (the covered anthropic copy never substitutes)", got)
			}
			if j := worktreeJSON(t, &u); j["cost"] != 1.0 {
				t.Errorf("json cost = %v, want 1", j["cost"])
			} else if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("metered winner must omit listCost (no substitution)")
			}
		})
	}
}

// TestPiCoveredPricedTwinBillsAtListBothOrders pins the measurement/coverage
// decoupling at the CLI boundary: when the covered copy of a billed turn carries
// the price and the metered twin is unpriced, the bill must preserve the known
// list price (never a null cost), in either file order.
func TestPiCoveredPricedTwinBillsAtListBothOrders(t *testing.T) {
	sandbox(t)
	writeAuthJSON(t, `{"anthropic":{"type":"oauth"},"openai":{"type":"api_key"}}`)
	pricedCovered := fmt.Sprintf(`{"type":"message","id":"t1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":"anthropic","model":"m","usage":{"cost":{"total":1}}}}` + "\n")
	unpricedMetered := fmt.Sprintf(`{"type":"message","id":"t1","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":"openai","model":"m","usage":{}}}` + "\n")
	for name, sessions := range map[string][]string{
		"covered-first": {pricedCovered, unpricedMetered},
		"metered-first": {unpricedMetered, pricedCovered},
	} {
		t.Run(name, func(t *testing.T) {
			u := piUsageWorktree(t, sessions...)
			j := worktreeJSON(t, &u)
			if j["cost"] != 1.0 {
				t.Errorf("json cost = %v, want 1 (preserve the priced covered list)", j["cost"])
			}
			if u.partial {
				t.Error("a disputed duplicate must not taint partial")
			}
			if _, ok := j["listCost"]; ok {
				t.Error("billed-at-list winner must omit listCost (no subscription split)")
			}
		})
	}
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

// claudeHarnessPrices reads the Claude harness cost/list pair: the level where
// per-cwd settings either void the proof (cost rises to list) or keep it.
func claudeHarnessPrices(t *testing.T, repo string) (cost any, listCost any) {
	t.Helper()
	rows := subscriptionStatus(t, repo)
	if len(rows) != 1 {
		t.Fatalf("got %d worktrees, want 1", len(rows))
	}
	usage := subscriptionObject(t, rows[0]["usage"])
	harness := subscriptionObject(t, usage["claude"])
	return harness["cost"], harness["listCost"]
}

// subscriptionBinaryApp adds the app subdirectory and its session to a binary
// fixture, so both proof-boundary tests start from one shape.
func subscriptionBinaryApp(t *testing.T) (repo, app string, env sandboxEnv) {
	t.Helper()
	repo, env = subscriptionBinaryRepo(t)
	app = filepath.Join(repo, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	seedSubscriptionSession(t, app, "app", `{"input_tokens":100000}`)
	return repo, app, env
}

func TestSubscriptionBinarySessionCwdSettings(t *testing.T) {
	repo, app, env := subscriptionBinaryApp(t)
	applySubscriptionOverride(t, app, env, "ANTHROPIC_AUTH_TOKEN", "project", "settings.local.json")
	// The app session bills metered, the root session stays covered.
	cost, listCost := claudeHarnessPrices(t, repo)
	if cost != float64(0.3) || listCost != 0.6 {
		t.Fatalf("claude cost/listCost = %#v/%#v, want 0.3/0.6", cost, listCost)
	}
}

func TestSubscriptionBinaryParentSettingsVoidSessionCwd(t *testing.T) {
	repo, _, env := subscriptionBinaryApp(t)
	applySubscriptionOverride(t, repo, env, "ANTHROPIC_AUTH_TOKEN", "project", "settings.json")
	// The ancestor walk voids the app session's proof: both sessions bill.
	cost, listCost := claudeHarnessPrices(t, repo)
	if cost != 0.6 || listCost != nil {
		t.Fatalf("claude cost/listCost = %#v/%#v, want 0.6/absent", cost, listCost)
	}
}

// TestSubscriptionBinaryRootSettingsVoidSubdirSession pins the root binding: a
// metered worktree root voids a nested session's proof even when
// WTM_CLAUDE_PARENT_SCOPE=0 disables the descendant's ancestor walk, so the
// subdirectory session can never read as a free $0.
func TestSubscriptionBinaryRootSettingsVoidSubdirSession(t *testing.T) {
	repo, _, env := subscriptionBinaryApp(t)
	t.Setenv("WTM_CLAUDE_PARENT_SCOPE", "0")
	applySubscriptionOverride(t, repo, env, "ANTHROPIC_API_KEY", "project", "settings.json")
	// Both the root and app sessions bill at list: the root proof bounds both.
	cost, listCost := claudeHarnessPrices(t, repo)
	if cost != float64(0.6) || listCost != nil {
		t.Fatalf("claude cost/listCost = %#v/%#v, want 0.6/absent", cost, listCost)
	}
}

// TestSubscriptionBinaryDescendantSeedsRootProof pins the root-proof recursion
// branch: a descendant session whose cwd is the only session in the worktree
// must resolve the worktree root through the proof cache, so a covered root
// never voids the descendant. If the root stayed unseeded, the descendant
// would read as unproven and bill list price instead.
func TestSubscriptionBinaryDescendantSeedsRootProof(t *testing.T) {
	sandbox(t)
	pinRates(t)
	for _, key := range claudeMeteredKeys() {
		t.Setenv(key, "")
	}
	writeClaudeAccount(t, "stripe_subscription", "2025-01-01T00:00:00Z")
	repo := nativeRepo(t)
	app := filepath.Join(repo, "app")
	if err := os.MkdirAll(app, 0o755); err != nil {
		t.Fatal(err)
	}
	seedSubscriptionSession(t, app, "app", `{"input_tokens":100000}`)
	cost, listCost := claudeHarnessPrices(t, repo)
	if cost != float64(0) || listCost != 0.3 {
		t.Fatalf("claude cost/listCost = %#v/%#v, want 0/0.3 (covered by the seeded root)", cost, listCost)
	}
}

// TestSubscriptionBinaryIntermediateAncestorWalk pins the ancestor walk between
// the worktree root and a session's own directory: a metered override at an
// intermediate directory reaches a nested session by default, and
// WTM_CLAUDE_PARENT_SCOPE=0 cuts exactly that walk without touching the clean
// root. The override sits at app, not the root, so neither case can pass by
// hiding behind the root guard.
func TestSubscriptionBinaryIntermediateAncestorWalk(t *testing.T) {
	repo, env := subscriptionBinaryRepo(t)
	app := filepath.Join(repo, "app")
	deep := filepath.Join(app, "subdir")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	seedSubscriptionSession(t, deep, "deep", `{"input_tokens":100000}`)
	applySubscriptionOverride(t, app, env, "ANTHROPIC_AUTH_TOKEN", "project", "settings.json")

	// Default: the ancestor walk carries the app override down to the deep
	// session; the root session stays covered.
	cost, listCost := claudeHarnessPrices(t, repo)
	if cost != float64(0.3) || listCost != 0.6 {
		t.Fatalf("claude cost/listCost = %#v/%#v, want 0.3/0.6", cost, listCost)
	}

	// The kill switch disables only the ancestor walk: with a clean root, the
	// deep session returns to covered.
	t.Setenv("WTM_CLAUDE_PARENT_SCOPE", "0")
	cost, listCost = claudeHarnessPrices(t, repo)
	if cost != float64(0) || listCost != 0.6 {
		t.Fatalf("parent scope off: claude cost/listCost = %#v/%#v, want 0/0.6", cost, listCost)
	}
}

// TestSubscriptionBinaryCorruptOverageStillEstimated pins that a rejected token
// count or web-search count is still usage evidence: a proven subscription that
// permits overage must show an estimated zero, not an exact one, even when
// every count was rejected.
func TestSubscriptionBinaryCorruptOverageStillEstimated(t *testing.T) {
	for _, tc := range []struct{ name, usage string }{
		{"tokens", `{"input_tokens":1000000000001}`},
		{"webSearch", `{"server_tool_use":{"web_search_requests":-5}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, _ := subscriptionBinaryRepo(t)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
				claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "true"))
			seedSubscriptionSession(t, repo, "main", tc.usage)
			rows := subscriptionStatus(t, repo)
			if len(rows) != 1 {
				t.Fatalf("got %d worktrees, want 1", len(rows))
			}
			// The list price is unknown, so only the billed estimate is asserted;
			// it must be 0 and flagged, never an exact $0.00.
			assertSubscriptionPrices(t, rows[0], map[string]any{"cost": float64(0), "listCost": nil, "costEstimated": true})
		})
	}
}
