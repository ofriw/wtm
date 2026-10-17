package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// subscription_test.go — the subscription proof and its two-price display. Every
// test drives the real credential files and the real session parsers under a
// sandboxed HOME, so the proof rules are exercised end to end, never mocked.

// piProviderLine builds one self-priced Pi assistant record with a provider, so
// a test drives the real parser and the provider capture.
func piProviderLine(provider string, total float64) string {
	return fmt.Sprintf(`{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":%q,"model":"m","usage":{"cost":{"total":%v}}}}`, provider, total)
}

// piUsageWorktree folds one synthetic Pi session per given JSONL body through
// buildUsage with the run's real credential proof.
func piUsageWorktree(t *testing.T, sessions ...string) worktreeUsage {
	t.Helper()
	dir := t.TempDir()
	refs := make([]sessionRef, 0, len(sessions))
	for i, body := range sessions {
		path := filepath.Join(dir, fmt.Sprintf("s%d.jsonl", i))
		writeFile(t, path, body, 0o644)
		refs = append(refs, sessionRef{path: path, harness: piAgent.name})
	}
	u, err := buildUsage(dir, sessionIndex{canonical(dir): refs}, nil, loadSubscriptions())
	if err != nil {
		t.Fatalf("buildUsage: %v", err)
	}
	return u
}

// piUsageOne is the single-session case of piUsageWorktree.
func piUsageOne(t *testing.T, body string) worktreeUsage {
	t.Helper()
	return piUsageWorktree(t, body+"\n")
}

func writeAuthJSON(t *testing.T, body string) {
	t.Helper()
	writeFile(t, filepath.Join(piAgent.configDir(), "auth.json"), body, 0o600)
}

// claudeAccountBody renders one oauthAccount record. extraUsage is the raw
// hasExtraUsageEnabled JSON literal; an empty value omits the field, which
// states that extra usage may be billed.
func claudeAccountBody(billingType, since, extraUsage string) string {
	extra := ""
	if extraUsage != "" {
		extra = fmt.Sprintf(`,"hasExtraUsageEnabled":%s`, extraUsage)
	}
	return fmt.Sprintf(`{"oauthAccount":{"billingType":%q,"subscriptionCreatedAt":%q%s}}`, billingType, since, extra)
}

// writeClaudeAccount seeds the account file Claude Code actually reads under the
// sandbox: the selected config dir's .claude.json, with extra usage disabled so
// the covered cost is exact.
func writeClaudeAccount(t *testing.T, billingType, since string) {
	t.Helper()
	writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
		claudeAccountBody(billingType, since, "false"))
}

// writeClaudeAccountAt seeds an arbitrary account file, so a test can place a
// legacy HOME file, a .config.json primary, or a custom-oauth file.
func writeClaudeAccountAt(t *testing.T, path, body string) {
	t.Helper()
	writeFile(t, path, body, 0o600)
}

// claudeWorktreeDir seeds one Claude session in a fresh dir and returns the dir
// with its index, so a test can seed project settings beside it.
func claudeWorktreeDir(t *testing.T) (string, sessionIndex) {
	t.Helper()
	dir := t.TempDir()
	return dir, usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{"input_tokens":100000}`))
}

// claudeUsageWorktree prices one Claude turn from the fixture catalog and the
// run's real credential proof.
func claudeUsageWorktree(t *testing.T) worktreeUsage {
	t.Helper()
	dir, idx := claudeWorktreeDir(t)
	return buildClaudeUsage(t, dir, idx)
}

// buildClaudeUsage folds a seeded Claude worktree through the run's proof.
func buildClaudeUsage(t *testing.T, dir string, idx sessionIndex) worktreeUsage {
	t.Helper()
	u, err := buildUsage(dir, idx, testRates(t), loadSubscriptions())
	if err != nil {
		t.Fatalf("buildUsage: %v", err)
	}
	return u
}

// worktreeJSON renders one usage through the machine contract and returns the
// raw object, so tests can assert both values and key presence.
func worktreeJSON(t *testing.T, u *worktreeUsage) map[string]any {
	t.Helper()
	w := worktree{Path: "/w", Usage: u}
	out := captureStdout(t, func() {
		if err := printStatusJSON([]worktree{w}, at(0), 10*time.Hour, "/w"); err != nil {
			t.Fatalf("printStatusJSON: %v", err)
		}
	})
	var raw []map[string]any
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	return raw[0]
}

func TestPiProviderCapture(t *testing.T) {
	session := `{"type":"model_change","provider":"openai-codex","modelId":"gpt-5.4"}
{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"gpt-5.4","usage":{"cost":{"total":1}}}}
{"type":"message","timestamp":"2026-01-01T00:00:01Z","message":{"role":"assistant","provider":"anthropic","model":"claude","usage":{"cost":{"total":2}}}}
{"type":"message","timestamp":"2026-01-01T00:00:02Z","message":{"role":"assistant","model":"claude","usage":{"cost":{"total":3}}}}
`
	events, _, err := piUsageEvents(bufio.NewReader(strings.NewReader(session)))
	if err != nil {
		t.Fatalf("piUsageEvents: %v", err)
	}
	want := []string{"openai-codex", "anthropic", "anthropic"}
	if len(events) != len(want) {
		t.Fatalf("events = %d, want %d", len(events), len(want))
	}
	for i, p := range want {
		if events[i].provider != p {
			t.Errorf("event %d provider = %q, want %q", i, events[i].provider, p)
		}
	}
}

// TestSubscriptionPiOAuthOnlyCovered pins the oauth-only rule: openai-codex can
// only bill through OAuth, so the session record alone proves the subscription.
func TestSubscriptionPiOAuthOnlyCovered(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("openai-codex", 41.20))
	if got := u.cost().cell(); got != "$0.00/$41.20" {
		t.Fatalf("cost = %q, want $0.00/$41.20", got)
	}
	j := worktreeJSON(t, &u)
	if j["cost"] != 0.0 {
		t.Errorf("json cost = %v, want 0", j["cost"])
	}
	if j["listCost"] != 41.2 {
		t.Errorf("json listCost = %v, want 41.2", j["listCost"])
	}
}

// TestSubscriptionPiDualOAuthCovered pins the dual-mode rule: OAuth in the
// current auth.json proves a dual provider covered.
func TestSubscriptionPiDualOAuthCovered(t *testing.T) {
	sandbox(t)
	writeAuthJSON(t, `{"anthropic":{"type":"oauth"}}`)
	u := piUsageOne(t, piProviderLine("anthropic", 10))
	if got := u.cost().cell(); got != "$0.00/$10.00" {
		t.Fatalf("cost = %q, want $0.00/$10.00", got)
	}
}

// TestSubscriptionPiUnprovenStaysList pins the hard fallback rule across every
// way a Pi subscription can fail to prove: an api_key dual provider, an unknown
// provider, a dual provider with no auth.json, and no provider at all.
func TestSubscriptionPiUnprovenStaysList(t *testing.T) {
	cases := map[string]struct {
		auth string
		line string
	}{
		"dual api_key":        {`{"anthropic":{"type":"api_key"}}`, piProviderLine("anthropic", 10)},
		"dual missing entry":  {`{"deepseek":{"type":"api_key"}}`, piProviderLine("openai", 10)},
		"unknown provider":    {`{"anthropic":{"type":"oauth"}}`, piProviderLine("deepseek", 10)},
		"no provider":         {`{"anthropic":{"type":"oauth"}}`, `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","model":"m","usage":{"cost":{"total":10}}}}` + "\n"},
		"missing auth file":   {"", piProviderLine("openai", 10)},
		"malformed auth file": {"{not json", piProviderLine("anthropic", 10)},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			if tc.auth != "" {
				writeAuthJSON(t, tc.auth)
			}
			u := piUsageOne(t, tc.line)
			if got := u.cost().cell(); got != "$10.00" {
				t.Fatalf("cost = %q, want the list price $10.00", got)
			}
			if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("unproven output must omit listCost")
			}
		})
	}
}

// TestSubscriptionPiKimiException pins Pi's documented exception: kimi-coding
// bills as a subscription even when auth.json holds an api_key.
func TestSubscriptionPiKimiException(t *testing.T) {
	sandbox(t)
	writeAuthJSON(t, `{"kimi-coding":{"type":"api_key"}}`)
	u := piUsageOne(t, piProviderLine("kimi-coding", 7.5))
	if got := u.cost().cell(); got != "$0.00/$7.50" {
		t.Fatalf("cost = %q, want $0.00/$7.50", got)
	}
}

// TestSubscriptionCoveredWithUnknownList pins the mixed-truth rendering: a
// proven subscription with no list price shows a billed zero beside "unknown".
func TestSubscriptionCoveredWithUnknownList(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, `{"type":"message","timestamp":"2026-01-01T00:00:00Z","message":{"role":"assistant","provider":"openai-codex","model":"m","usage":{}}}`+"\n")
	if got := u.cost().cell(); got != "$0.00/unknown" {
		t.Fatalf("cost = %q, want $0.00/unknown", got)
	}
	j := worktreeJSON(t, &u)
	if j["cost"] != 0.0 {
		t.Errorf("json cost = %v, want 0", j["cost"])
	}
	v, ok := j["listCost"]
	if !ok {
		t.Fatal("covered output must carry listCost")
	}
	if v != nil {
		t.Errorf("listCost = %v, want null (unknown list)", v)
	}
}

// TestSubscriptionMixedWorktree pins that a covered and a metered session add
// up on both sides: the billed total keeps the metered cost, the list total
// keeps the full price.
func TestSubscriptionMixedWorktree(t *testing.T) {
	sandbox(t)
	u := piUsageWorktree(t,
		piProviderLine("openai-codex", 10)+"\n",
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"assistant","provider":"deepseek","model":"other","usage":{"cost":{"total":5}}}}`+"\n",
	)
	if got := u.cost().cell(); got != "$5.00/$15.00" {
		t.Fatalf("cost = %q, want $5.00/$15.00", got)
	}
	j := worktreeJSON(t, &u)
	if j["cost"] != 5.0 {
		t.Errorf("json cost = %v, want 5", j["cost"])
	}
	if j["listCost"] != 15.0 {
		t.Errorf("json listCost = %v, want 15", j["listCost"])
	}
}

// TestSubscriptionSpawnChildKeepsItsOwnProvider pins the spawn regression: a
// proven parent's child that Pi routed to a metered provider must stay at list
// price. Reading the parent's provider would fake a covered child, which is how
// a metered zai/glm child under an openai-codex parent showed a $billed/$list
// pair despite no subscription.
func TestSubscriptionSpawnChildKeepsItsOwnProvider(t *testing.T) {
	sandbox(t)
	body := `{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"zai/glm","route":{"status":"routed","provider":"vercel-ai-gateway"},"stats":{"cost":0.06}}}}`
	u := piUsageOne(t, body)
	if got := u.cost().cell(); got != "$0.06" {
		t.Fatalf("cost = %q, want the metered list price $0.06", got)
	}
	if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
		t.Error("a routed metered child must not produce a covered pair")
	}
}

// TestSubscriptionSpawnInheritedStaysCovered pins the other side: an inherited
// child under a proven parent is covered, so the fix never over-corrects.
func TestSubscriptionSpawnInheritedStaysCovered(t *testing.T) {
	sandbox(t)
	body := `{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"model":"m-child","route":{"status":"inherited"},"stats":{"cost":0.06}}}}`
	u := piUsageOne(t, body)
	if got := u.cost().cell(); got != "$0.00/$0.06" {
		t.Fatalf("cost = %q, want $0.00/$0.06", got)
	}
}

// TestSubscriptionSpawnLegacyChildStaysList pins the fail-closed rule end to
// end: a legacy results[] child carries no route, so even under a proven
// parent it stays at list price instead of inheriting the subscription.
func TestSubscriptionSpawnLegacyChildStaysList(t *testing.T) {
	sandbox(t)
	body := `{"type":"model_change","timestamp":"2026-01-01T00:00:00Z","provider":"openai-codex","modelId":"m-parent"}` + "\n" +
		`{"type":"message","timestamp":"2026-01-01T00:00:05Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"m-child","cost":0.06}}]}}}`
	u := piUsageOne(t, body)
	if got := u.cost().cell(); got != "$0.06" {
		t.Fatalf("cost = %q, want the metered list price $0.06", got)
	}
	if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
		t.Error("a legacy child with no route must not produce a covered pair")
	}
}

// TestSubscriptionClaudeCovered pins the account proof: stripe_subscription with
// an event after the subscription start makes the turn covered.
func TestClaudeSettingsMeteredOverrideKeys(t *testing.T) {
	for _, key := range []string{"ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_USE_FOUNDRY"} {
		t.Run(key, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			writeFile(t, path, fmt.Sprintf(`{"env":{%q:"1"}}`, key), 0o600)
			if !settingsMetered(path) {
				t.Fatalf("settings with %s must invalidate proof", key)
			}
		})
	}
}

func TestSubscriptionClaudeCovered(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10", got)
	}
}

// TestSubscriptionClaudeAccountFollowsSelectedFile pins the resolver: the file
// under the selected config dir decides the proof, so a stale HOME file can
// neither grant nor steal it.
func TestSubscriptionClaudeAccountFollowsSelectedFile(t *testing.T) {
	cases := map[string]struct {
		homeBody     string
		selectedBody string
		want         string
	}{
		"selected subscription wins": {
			homeBody:     claudeAccountBody("invoice", "2020-01-01T00:00:00Z", ""),
			selectedBody: claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "false"),
			want:         "$0.00/$0.10",
		},
		"selected metered loses": {
			homeBody:     claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "false"),
			selectedBody: claudeAccountBody("invoice", "2020-01-01T00:00:00Z", ""),
			want:         "$0.10",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := sandbox(t)
			writeClaudeAccountAt(t, filepath.Join(env.Home, ".claude.json"), tc.homeBody)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), tc.selectedBody)
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != tc.want {
				t.Fatalf("cost = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubscriptionClaudeConfigJSONPrimary pins the primary path: a .config.json
// in the config dir wins over .claude.json beside it, whatever the suffix.
func TestSubscriptionClaudeConfigJSONPrimary(t *testing.T) {
	sandbox(t)
	dir := claudeAgent.configDir()
	writeClaudeAccountAt(t, filepath.Join(dir, ".config.json"),
		claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "false"))
	writeClaudeAccountAt(t, filepath.Join(dir, ".claude.json"),
		claudeAccountBody("invoice", "2020-01-01T00:00:00Z", ""))
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10 (the primary .config.json wins)", got)
	}
}

// TestSubscriptionClaudeCustomOAuthFile pins the custom-oauth suffix: when
// CLAUDE_CODE_CUSTOM_OAUTH_URL is set, the resolver reads
// .claude-custom-oauth.json instead of .claude.json.
func TestSubscriptionClaudeCustomOAuthFile(t *testing.T) {
	sandbox(t)
	t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "https://oauth.example")
	dir := claudeAgent.configDir()
	writeClaudeAccountAt(t, filepath.Join(dir, ".claude-custom-oauth.json"),
		claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "false"))
	writeClaudeAccountAt(t, filepath.Join(dir, ".claude.json"),
		claudeAccountBody("invoice", "2020-01-01T00:00:00Z", ""))
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want the custom-oauth file's proof", got)
	}
}

// TestSubscriptionClaudeOveragePossibleIsEstimated pins the tri-state: a proven
// subscription that still allows extra usage bills $0 inside the allowance but
// may charge list price over it, so the billed zero is an estimate and the list
// price is shown beside it.
func TestSubscriptionClaudeOveragePossibleIsEstimated(t *testing.T) {
	for _, tc := range []struct{ name, extra string }{{"enabled", "true"}, {"absent", ""}} {
		t.Run(tc.name, func(t *testing.T) {
			sandbox(t)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
				claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", tc.extra))
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != "~$0.00/$0.10" {
				t.Fatalf("cost = %q, want ~$0.00/$0.10", got)
			}
			j := worktreeJSON(t, &u)
			if j["cost"] != 0.0 {
				t.Errorf("json cost = %v, want 0", j["cost"])
			}
			if j["costEstimated"] != true {
				t.Errorf("json costEstimated = %v, want true", j["costEstimated"])
			}
		})
	}
}

// TestSubscriptionClaudeAllowanceOnlyIsExact pins the exact side: the account
// explicitly disables extra usage, so the $0 marginal cost is known, not
// estimated, and the estimated flag stays absent.
func TestSubscriptionClaudeAllowanceOnlyIsExact(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10", got)
	}
	if _, ok := worktreeJSON(t, &u)["costEstimated"]; ok {
		t.Error("an exact allowance-only cost must omit costEstimated")
	}
}

// TestSubscriptionClaudeUnprovenStaysList pins every way the Claude proof fails:
// a metered override, a different billing type, and an event before the
// subscription was created.
func TestSubscriptionClaudeUnprovenStaysList(t *testing.T) {
	cases := map[string]struct {
		prepare func(t *testing.T)
	}{
		"api key override": {func(t *testing.T) {
			t.Setenv("ANTHROPIC_API_KEY", "sk-test")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"bedrock override": {func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_USE_BEDROCK", "1")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"vertex override": {func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_USE_VERTEX", "1")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"api key descriptor override": {func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", "3")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"anthropic aws override": {func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_USE_ANTHROPIC_AWS", "1")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"mantle override": {func(t *testing.T) {
			t.Setenv("CLAUDE_CODE_USE_MANTLE", "1")
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
		}},
		"key helper": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.json"), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)
		}},
		"local key helper": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.local.json"), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)
		}},
		"settings env key": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.local.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`, 0o600)
		}},
		"user settings env key": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.json"), `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`, 0o600)
		}},
		"settings api key descriptor": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.local.json"), `{"env":{"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":"3"}}`, 0o600)
		}},
		"settings anthropic aws": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.local.json"), `{"env":{"CLAUDE_CODE_USE_ANTHROPIC_AWS":"1"}}`, 0o600)
		}},
		"settings mantle": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			writeFile(t, filepath.Join(claudeAgent.configDir(), "settings.local.json"), `{"env":{"CLAUDE_CODE_USE_MANTLE":"1"}}`, 0o600)
		}},
		"other billing type": {func(t *testing.T) {
			writeClaudeAccount(t, "invoice", "2020-01-01T00:00:00Z")
		}},
		"before subscription": {func(t *testing.T) {
			writeClaudeAccount(t, "stripe_subscription", "2027-01-01T00:00:00Z")
		}},
		"no account file": {func(t *testing.T) {}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			tc.prepare(t)
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != "$0.10" {
				t.Fatalf("cost = %q, want the list price $0.10", got)
			}
			if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("unproven output must omit listCost")
			}
		})
	}
}

// TestClaudeProviderFlagBooleanSemantics pins Claude's boolean grammar on both
// proof paths: only 1/true/yes/on disable coverage; a disabled flag keeps it.
func TestClaudeProviderFlagBooleanSemantics(t *testing.T) {
	boolKeys := []string{
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX",
		"CLAUDE_CODE_USE_FOUNDRY", "CLAUDE_CODE_USE_ANTHROPIC_AWS",
		"CLAUDE_CODE_USE_MANTLE",
	}
	values := []struct {
		value string
		true  bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"no", false}, {"off", false},
		{"1", true}, {"true", true}, {"yes", true}, {"on", true},
		{" TRUE ", true}, {"  On  ", true}, {" FALSE ", false},
	}
	for _, key := range boolKeys {
		for _, scope := range []string{"process", "settings"} {
			for _, tc := range values {
				t.Run(key+"/"+scope+"/"+tc.value, func(t *testing.T) {
					sandbox(t)
					writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
					if scope == "process" {
						t.Setenv(key, tc.value)
					} else {
						path := filepath.Join(claudeAgent.configDir(), "settings.local.json")
						writeFile(t, path, fmt.Sprintf(`{"env":{%q:%q}}`, key, tc.value), 0o600)
					}
					want := "$0.00/$0.10"
					if tc.true {
						want = "$0.10"
					}
					u := claudeUsageWorktree(t)
					if got := u.cost().cell(); got != want {
						t.Fatalf("%s=%q cost = %q, want %q", key, tc.value, got, want)
					}
				})
			}
		}
	}
}

// TestSubscriptionClaudeProjectOverrideStaysList pins the repo-level rule:
// Claude Code reads the worktree's own .claude/settings*.json, so an
// apiKeyHelper or a metered env key there bills outside the subscription.
func TestSubscriptionClaudeProjectOverrideStaysList(t *testing.T) {
	cases := map[string]struct {
		path string
		body string
	}{
		"helper settings":       {claudeSettingsFile, `{"apiKeyHelper":"/bin/fetch"}`},
		"helper local settings": {claudeLocalSettingsFile, `{"apiKeyHelper":"/bin/fetch"}`},
		"env api key":           {claudeSettingsFile, `{"env":{"ANTHROPIC_API_KEY":"sk-test"}}`},
		"env key descriptor":    {claudeSettingsFile, `{"env":{"CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR":"3"}}`},
		"env vertex":            {claudeLocalSettingsFile, `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`},
		"env anthropic aws":     {claudeLocalSettingsFile, `{"env":{"CLAUDE_CODE_USE_ANTHROPIC_AWS":"1"}}`},
		"env mantle":            {claudeLocalSettingsFile, `{"env":{"CLAUDE_CODE_USE_MANTLE":"1"}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			dir, idx := claudeWorktreeDir(t)
			writeFile(t, filepath.Join(dir, tc.path), tc.body, 0o600)
			u := buildClaudeUsage(t, dir, idx)
			if got := u.cost().cell(); got != "$0.10" {
				t.Fatalf("cost = %q, want the list price $0.10", got)
			}
			if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("a repo override must omit listCost")
			}
		})
	}
}

// TestSubscriptionClaudeProjectDisabledFlagStaysCovered pins the boolean grammar
// on the repo path: a disabled provider switch keeps the account proof.
func TestSubscriptionClaudeProjectDisabledFlagStaysCovered(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	dir, idx := claudeWorktreeDir(t)
	writeFile(t, filepath.Join(dir, claudeLocalSettingsFile), `{"env":{"CLAUDE_CODE_USE_VERTEX":"false"}}`, 0o600)
	u := buildClaudeUsage(t, dir, idx)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10 (a disabled switch keeps coverage)", got)
	}
}

// TestSubscriptionClaudeCleanProjectStaysCovered pins the other side: a project
// setting that does not touch a metered key keeps the account proof.
func TestSubscriptionClaudeCleanProjectStaysCovered(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	dir, idx := claudeWorktreeDir(t)
	writeFile(t, filepath.Join(dir, claudeSettingsFile), `{"env":{"WTM_UNRELATED":"1"}}`, 0o600)
	u := buildClaudeUsage(t, dir, idx)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10", got)
	}
}

// TestSubscriptionClaudeParentSettingsIgnored pins the scope: a metered helper
// in an ancestor directory voids the proof, so the turn stays at list price;
// WTM_CLAUDE_PARENT_SCOPE=0 disables the ancestor walk and keeps it covered.
func TestSubscriptionClaudeParentSettingsIgnored(t *testing.T) {
	for _, file := range []string{claudeSettingsFile, claudeLocalSettingsFile} {
		t.Run(file, func(t *testing.T) {
			seed := func(t *testing.T) (string, sessionIndex) {
				t.Helper()
				sandbox(t)
				writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
				parent := t.TempDir()
				writeFile(t, filepath.Join(parent, file), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)
				dir := filepath.Join(parent, "child")
				return dir, usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{"input_tokens":100000}`))
			}
			t.Run("parent voids", func(t *testing.T) {
				dir, idx := seed(t)
				u := buildClaudeUsage(t, dir, idx)
				if got := u.cost().cell(); got != "$0.10" {
					t.Fatalf("cost = %q, want $0.10 (ancestor settings void the proof)", got)
				}
				if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
					t.Error("unproven output must omit listCost")
				}
			})
			t.Run("scope disabled", func(t *testing.T) {
				dir, idx := seed(t)
				t.Setenv("WTM_CLAUDE_PARENT_SCOPE", "0")
				u := buildClaudeUsage(t, dir, idx)
				if got := u.cost().cell(); got != "$0.00/$0.10" {
					t.Fatalf("cost = %q, want $0.00/$0.10 (disabled ancestor walk)", got)
				}
			})
		})
	}
}

// TestSubscriptionClaudeManagedOverrideStaysList pins the enterprise rule: a
// managed-settings.json with a helper or metered env key voids every
// worktree's proof at once, since IT owns that file.
func TestSubscriptionClaudeManagedOverrideStaysList(t *testing.T) {
	cases := map[string]string{
		"helper":            `{"apiKeyHelper":"/bin/fetch"}`,
		"env key":           `{"env":{"CLAUDE_CODE_USE_VERTEX":"1"}}`,
		"env anthropic aws": `{"env":{"CLAUDE_CODE_USE_ANTHROPIC_AWS":"1"}}`,
		"env mantle":        `{"env":{"CLAUDE_CODE_USE_MANTLE":"1"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			sandbox(t)
			managed := filepath.Join(t.TempDir(), "managed-settings.json")
			writeFile(t, managed, body, 0o600)
			t.Setenv("WTM_MANAGED_SETTINGS", managed)
			writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != "$0.10" {
				t.Fatalf("cost = %q, want the list price $0.10", got)
			}
			if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("a managed override must omit listCost")
			}
		})
	}
}

// TestSubscriptionClaudeManagedDisabledFlagStaysCovered pins the boolean
// grammar on the enterprise path: a disabled switch keeps coverage.
func TestSubscriptionClaudeManagedDisabledFlagStaysCovered(t *testing.T) {
	sandbox(t)
	managed := filepath.Join(t.TempDir(), "managed-settings.json")
	writeFile(t, managed, `{"env":{"CLAUDE_CODE_USE_MANTLE":"0"}}`, 0o600)
	t.Setenv("WTM_MANAGED_SETTINGS", managed)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10 (a disabled switch keeps coverage)", got)
	}
}

// TestSubscriptionClaudeWorktreeSettingsIsolation pins the per-worktree scope:
// one loaded proof covers a clean worktree and drops only the worktree carrying
// its own metered settings, with no leak either way.
func TestSubscriptionClaudeWorktreeSettingsIsolation(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	base := t.TempDir()
	dirty := filepath.Join(base, "dirty")
	writeFile(t, filepath.Join(dirty, claudeSettingsFile), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)
	subs := loadSubscriptions()
	if !subs.claude {
		t.Fatal("the account proof must load before scoping")
	}
	for dir, want := range map[string]string{
		filepath.Join(base, "clean"): "$0.00/$0.10",
		dirty:                        "$0.10",
	} {
		idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{"input_tokens":100000}`))
		u, err := buildUsage(dir, idx, testRates(t), subs)
		if err != nil {
			t.Fatalf("buildUsage(%s): %v", dir, err)
		}
		if got := u.cost().cell(); got != want {
			t.Errorf("%s cost = %q, want %q", dir, got, want)
		}
	}
	if !subs.claude {
		t.Error("scoping one worktree must not poison the shared proof")
	}
}

// TestSubscriptionClaudeMissingSinceDatedStaysList pins the fail-closed rule:
// an absent or unparseable start mark proves nothing about dated history, so
// a dated event stays at list price. Only a dateless event (no timestamp to
// contradict the proof) stays covered — see TestClaudeProvenMissingSince.
func TestSubscriptionClaudeMissingSinceDatedStaysList(t *testing.T) {
	for _, since := range []string{"", "not-a-time"} {
		t.Run("since="+since, func(t *testing.T) {
			sandbox(t)
			writeClaudeAccount(t, "stripe_subscription", since)
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != "$0.10" {
				t.Fatalf("cost = %q, want the list price $0.10", got)
			}
			if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
				t.Error("an unproven dated event must omit listCost")
			}
		})
	}
}

// TestClaudeProvenMissingSince pins the zero-since boundary directly (both
// parsers drop dateless records, so no fixture can produce one end to end):
// with no known start, dated events are unproven, dateless ones covered.
func TestClaudeProvenMissingSince(t *testing.T) {
	s := subscriptions{claude: true}
	if s.claudeProven(at(0)) {
		t.Error("dated event with unknown start must be unproven")
	}
	if !s.claudeProven(time.Time{}) {
		t.Error("dateless event with unknown start must stay covered")
	}
}

// TestSubscriptionClaudeBoundaryAtSince pins the exact boundary: an event at
// the subscription start time is covered (not before).
func TestSubscriptionClaudeBoundaryAtSince(t *testing.T) {
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2026-01-01T00:00:00Z")
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "$0.00/$0.10" {
		t.Fatalf("cost = %q, want $0.00/$0.10 (event at exact since is covered)", got)
	}
}

// TestSubscriptionListCostAtEveryLevel pins the machine contract: a proven
// subscription carries listCost at the worktree, harness, and model levels.
func TestSubscriptionListCostAtEveryLevel(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("openai-codex", 41.20))
	j := worktreeJSON(t, &u)
	if j["listCost"] != 41.2 {
		t.Errorf("worktree listCost = %v, want 41.2", j["listCost"])
	}
	harness := j["usage"].(map[string]any)[piAgent.name].(map[string]any)
	if harness["listCost"] != 41.2 {
		t.Errorf("harness listCost = %v, want 41.2", harness["listCost"])
	}
	model := harness["models"].(map[string]any)["m"].(map[string]any)
	if model["listCost"] != 41.2 {
		t.Errorf("model listCost = %v, want 41.2", model["listCost"])
	}
}

// TestSubscriptionEqualListShowsNoPair pins the one show rule: a proven
// subscription whose list is also zero has nothing to compare, so the table
// shows one value and the JSON omits listCost.
func TestSubscriptionEqualListShowsNoPair(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("openai-codex", 0))
	if got := u.cost().cell(); got != "$0.00" {
		t.Fatalf("cost = %q, want $0.00", got)
	}
	if _, ok := worktreeJSON(t, &u)["listCost"]; ok {
		t.Error("an equal list price must omit listCost")
	}
}

// TestSubscriptionPartialCoveredOmitsListCost pins the both-unknown case: when
// a partial total taints both sides, neither price is comparable, so the table
// reads "unknown" and the JSON omits listCost at every level. A null beside
// "unknown" adds no signal, so even a harness whose amounts differ before
// tainting omits the key once both sides are unknown.
func TestSubscriptionPartialCoveredOmitsListCost(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("openai-codex", 10))
	markPartial(&u, map[string]bool{piAgent.name: true})
	if got := u.cost().cell(); got != "unknown" {
		t.Fatalf("cost = %q, want unknown", got)
	}
	j := worktreeJSON(t, &u)
	// Worktree total: both sides unknown and equal, so listCost is omitted.
	if _, ok := j["listCost"]; ok {
		t.Error("a tainted equal total must omit listCost: no side is comparable")
	}
	// Harness level: both sides tainted unknown, so listCost is omitted as well.
	harness, ok := j["usage"].(map[string]any)[piAgent.name].(map[string]any)
	if !ok {
		t.Fatalf("harness entry missing: %+v", j["usage"])
	}
	if harness["cost"] != nil {
		t.Errorf("harness cost = %v, want null (unknown)", harness["cost"])
	}
	if v, present := harness["listCost"]; present {
		t.Errorf("harness listCost = %v, want omitted (both sides unknown)", v)
	}
}

// TestSubscriptionUnprovenOutputUnchanged pins the regression contract: with no
// proof anywhere, the cost cell and JSON are exactly today's list-price output.
func TestSubscriptionUnprovenOutputUnchanged(t *testing.T) {
	sandbox(t)
	u := piUsageOne(t, piProviderLine("deepseek", 1.5))
	w := worktree{Path: "/w", Usage: &u}
	if got := cellCost(w, columnContext{}); got != "$1.50" {
		t.Errorf("table cost = %q, want $1.50", got)
	}
	j := worktreeJSON(t, &u)
	if j["cost"] != 1.5 {
		t.Errorf("json cost = %v, want 1.5", j["cost"])
	}
	if _, ok := j["listCost"]; ok {
		t.Error("unproven output must omit listCost at the worktree level")
	}
	harness := j["usage"].(map[string]any)["pi"].(map[string]any)
	if harness["cost"] != 1.5 {
		t.Errorf("harness cost = %v, want 1.5", harness["cost"])
	}
	if _, ok := harness["listCost"]; ok {
		t.Error("unproven output must omit listCost at the harness level")
	}
	model := harness["models"].(map[string]any)["m"].(map[string]any)
	if _, ok := model["listCost"]; ok {
		t.Error("unproven output must omit listCost at the model level")
	}
}

// TestSubscriptionClaudeLegacyMissingFieldIsOveragePossible pins 2D: a legacy
// account file without hasExtraUsageEnabled may still bill overage, so the
// covered $0 is an estimate. A future upgrade that writes the field as false
// flips this to an exact $0.
func TestSubscriptionClaudeLegacyMissingFieldIsOveragePossible(t *testing.T) {
	sandbox(t)
	writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
		`{"oauthAccount":{"billingType":"stripe_subscription","subscriptionCreatedAt":"2020-01-01T00:00:00Z"}}`)
	u := claudeUsageWorktree(t)
	if got := u.cost().cell(); got != "~$0.00/$0.10" {
		t.Fatalf("cost = %q, want ~$0.00/$0.10 (absent field means overage possible)", got)
	}
	if _, ok := worktreeJSON(t, &u)["costEstimated"]; !ok {
		t.Error("a legacy overage-possible cost must set costEstimated")
	}
}

// TestSubscriptionClaudeZeroTokenStaysExact pins 2C: a zero-token turn under an
// overage-possible proof costs exactly $0 — there is nothing to push into
// overage, so the estimate flag must stay absent.
func TestSubscriptionClaudeZeroTokenStaysExact(t *testing.T) {
	sandbox(t)
	writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
		claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "true"))
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{}`))
	u, err := buildUsage(dir, idx, testRates(t), loadSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.cost().cell(); got != "$0.00" {
		t.Fatalf("cost = %q, want $0.00 (a zero-token turn is exact)", got)
	}
	if _, ok := worktreeJSON(t, &u)["costEstimated"]; ok {
		t.Error("a zero-token turn must omit costEstimated")
	}
}

// TestClaudeAccountFileMatrix pins 1B: the resolver precedence across HOME,
// the selected config dir, the .config.json primary, the custom-oauth suffix,
// and a missing file. It drives the real files and the real parser, so a HOME
// regression fails here instead of hiding.
func TestClaudeAccountFileMatrix(t *testing.T) {
	sub := claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "false")
	metered := claudeAccountBody("invoice", "2020-01-01T00:00:00Z", "")
	cases := map[string]struct {
		setup func(t *testing.T, env sandboxEnv)
		oauth bool
		want  string
	}{
		"selected config dir wins over HOME": {func(t *testing.T, env sandboxEnv) {
			writeClaudeAccountAt(t, filepath.Join(env.Home, ".claude.json"), sub)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), metered)
		}, false, "$0.10"},
		"HOME subscription loses to selected metered": {func(t *testing.T, env sandboxEnv) {
			writeClaudeAccountAt(t, filepath.Join(env.Home, ".claude.json"), metered)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), sub)
		}, false, "$0.00/$0.10"},
		"HOME file decides when config dir unset": {func(t *testing.T, env sandboxEnv) {
			t.Setenv("CLAUDE_CONFIG_DIR", "")
			writeClaudeAccountAt(t, filepath.Join(env.Home, ".claude.json"), sub)
		}, false, "$0.00/$0.10"},
		"config.json primary wins over selected claude file": {func(t *testing.T, env sandboxEnv) {
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".config.json"), sub)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), metered)
		}, false, "$0.00/$0.10"},
		"custom-oauth suffix wins when endpoint set": {func(t *testing.T, env sandboxEnv) {
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude-custom-oauth.json"), sub)
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), metered)
		}, true, "$0.00/$0.10"},
		"plain file ignored when endpoint set": {func(t *testing.T, env sandboxEnv) {
			writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"), sub)
		}, true, "$0.10"},
		"missing file stays list": {func(t *testing.T, env sandboxEnv) {}, false, "$0.10"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			env := sandbox(t)
			if tc.oauth {
				t.Setenv("CLAUDE_CODE_CUSTOM_OAUTH_URL", "https://oauth.example")
			}
			tc.setup(t, env)
			u := claudeUsageWorktree(t)
			if got := u.cost().cell(); got != tc.want {
				t.Fatalf("cost = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestSubscriptionOverageUnknownListShape pins 3B: a proven overage-possible
// turn with no catalog bills ~$0.00 against an unknown list, and the JSON keeps
// the number, the estimate flag, and an explicit null listCost.
func TestSubscriptionOverageUnknownListShape(t *testing.T) {
	sandbox(t)
	writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
		claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "true"))
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{"input_tokens":100000}`))
	u, err := buildUsage(dir, idx, nil, loadSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.cost().cell(); got != "~$0.00/unknown" {
		t.Fatalf("cost = %q, want ~$0.00/unknown", got)
	}
	j := worktreeJSON(t, &u)
	if j["cost"] != 0.0 {
		t.Errorf("json cost = %v, want 0", j["cost"])
	}
	if j["costEstimated"] != true {
		t.Errorf("json costEstimated = %v, want true", j["costEstimated"])
	}
	lc, present := j["listCost"]
	if !present {
		t.Fatal("a proven subscription with an unknown list must keep listCost (null)")
	}
	if lc != nil {
		t.Errorf("json listCost = %v, want null", lc)
	}
	if _, ok := j["listCostEstimated"]; ok {
		t.Error("an unknown list must omit listCostEstimated")
	}
}

// TestSubscriptionOverageWebSearchOnlyIsEstimated pins that a covered event
// whose only spend is a web search reads estimated, not an exact zero: the
// search can overflow the allowance and bill at list price.
func TestSubscriptionOverageWebSearchOnlyIsEstimated(t *testing.T) {
	sandbox(t)
	writeClaudeAccountAt(t, filepath.Join(claudeAgent.configDir(), ".claude.json"),
		claudeAccountBody("stripe_subscription", "2020-01-01T00:00:00Z", "true"))
	dir := t.TempDir()
	idx := usageIndex(t, dir, "s.jsonl", claudeAgent.name,
		claudeUsageLine(`{"server_tool_use":{"web_search_requests":2}}`))
	u, err := buildUsage(dir, idx, nil, loadSubscriptions())
	if err != nil {
		t.Fatal(err)
	}
	if got := u.cost().cell(); got != "~$0.00/unknown" {
		t.Fatalf("cost = %q, want ~$0.00/unknown (a covered web-search-only turn is an estimate)", got)
	}
	if j := worktreeJSON(t, &u); j["costEstimated"] != true {
		t.Errorf("json costEstimated = %v, want true", j["costEstimated"])
	}
}

// parentScopeSeed seeds a clean child worktree under a metered parent, so the
// ancestor walk is the only thing voiding the proof.
func parentScopeSeed(t *testing.T) (string, sessionIndex) {
	t.Helper()
	sandbox(t)
	writeClaudeAccount(t, "stripe_subscription", "2020-01-01T00:00:00Z")
	parent := t.TempDir()
	writeFile(t, filepath.Join(parent, claudeSettingsFile), `{"apiKeyHelper":"/bin/fetch"}`, 0o600)
	dir := filepath.Join(parent, "child")
	return dir, usageIndex(t, dir, "s.jsonl", claudeAgent.name, claudeUsageLine(`{"input_tokens":100000}`))
}

// TestParentScopeKillSwitchWarnsOnceAndSkipsCache pins 3A: disabling the
// ancestor walk warns once per run, stays fail-open, and never caches the
// verdict (a cached verdict would go stale across env changes).
func TestParentScopeKillSwitchWarnsOncePerRun(t *testing.T) {
	dir, _ := parentScopeSeed(t)
	t.Setenv("WTM_CLAUDE_PARENT_SCOPE", "0")
	subs := loadSubscriptions()
	var first, second subscriptions
	errOut := captureStderr(t, func() {
		first = subs.forWorktree(dir)
		second = subs.forWorktree(dir)
	})
	if !first.claude || !second.claude {
		t.Fatalf("claude = %v/%v, want fail-open true/true", first.claude, second.claude)
	}
	if got := strings.Count(errOut, "WTM_CLAUDE_PARENT_SCOPE=0"); got != 1 {
		t.Fatalf("warnings = %d, want exactly 1 per run:\n%s", got, errOut)
	}
	if _, ok := subs.wtVerdicts[dir]; ok {
		t.Error("the kill-switch verdict must skip the cache")
	}

	// A second run has its own once-state, so it warns again.
	secondRun := loadSubscriptions()
	errOut = captureStderr(t, func() { _ = secondRun.forWorktree(dir) })
	if got := strings.Count(errOut, "WTM_CLAUDE_PARENT_SCOPE=0"); got != 1 {
		t.Fatalf("second run warnings = %d, want exactly 1:\n%s", got, errOut)
	}
}

// TestParentScopeNilStateWarnsEveryCall pins the zero-value proof: without the
// per-run state, the kill-switch warning must not hide.
func TestParentScopeNilStateWarnsEveryCall(t *testing.T) {
	var subs subscriptions
	errOut := captureStderr(t, func() {
		subs.warnParentScopeOnce()
		subs.warnParentScopeOnce()
	})
	if got := strings.Count(errOut, "WTM_CLAUDE_PARENT_SCOPE=0"); got != 2 {
		t.Fatalf("warnings = %d, want one per call:\n%s", got, errOut)
	}
}

// TestParentScopeCacheSkippedWhenDisabled pins the no-stale-cache direction: a
// verdict cached while the walk ran must not void the proof once the operator
// disables the walk.
func TestParentScopeCacheSkippedWhenDisabled(t *testing.T) {
	dir, _ := parentScopeSeed(t)
	subs := loadSubscriptions()
	if got := subs.forWorktree(dir); got.claude {
		t.Fatal("enabled walk must void the proof under a metered parent")
	}
	t.Setenv("WTM_CLAUDE_PARENT_SCOPE", "0")
	var disabled subscriptions
	captureStderr(t, func() {
		// Reuse the stale cache: the disabled walk must recompute, not trust it.
		disabled = subs.forWorktree(dir)
	})
	if !disabled.claude {
		t.Fatal("a cached void verdict must not survive disabling the walk")
	}
}
