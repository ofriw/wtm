package main

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// subscription.go — the subscription proof. wtm charges tokens at pay-as-you-go
// list price by default; it shows a subscription's $0 marginal cost ONLY when
// the current credentials prove the subscription, because an unproven claim
// would hide real money. Every rule answers "proven or not": a missing or
// unreadable credential file proves nothing, and never fails a status run.

// The Pi provider sets mirror Pi's own provider definitions. openai-codex
// authenticates only through OAuth, so a session naming it is subscription-billed
// without consulting a file. Every other subscription-backed provider is
// dual-mode: it also accepts an API key, so the provider's CURRENT auth.json
// entry must say "oauth". kimi-coding is the exception handled by
// piAlwaysSubscription, so it is not dual-mode.
var (
	piOAuthOnly = map[string]bool{"openai-codex": true}
	piDualMode  = map[string]bool{
		"anthropic": true, "github-copilot": true,
		"meta": true, "openai": true, "xai": true,
	}
)

// piAlwaysSubscription mirrors Pi's hardcoded exception: kimi-coding bills as a
// subscription even when auth.json holds an api_key.
const piAlwaysSubscription = "kimi-coding"

// subscriptions is the credential snapshot for one status run. An empty
// snapshot proves nothing, so an unreadable file degrades to today's list price.
type subscriptions struct {
	piOAuth map[string]bool // provider whose auth.json type is "oauth"
	claude  bool            // stripe_subscription with no metered override
	// claudeAllowanceOnly proves the account has no extra-usage billing, so a
	// completed request costs exactly $0 instead of a possible overage charge.
	claudeAllowanceOnly bool
	managed             bool      // cached managed-settings metered verdict (machine-global)
	since               time.Time // subscription start; zero when unknown
	// wtVerdicts caches the per-worktree Claude verdict, so the settings walk
	// runs once per worktree per run. The map is shared by every forWorktree
	// copy; a nil map skips the cache (zero-value proof in tests).
	wtVerdicts map[string]bool
}

// loadSubscriptions reads both credential sources once per status run. It never
// fails: an absent or malformed file yields an empty proof.
func loadSubscriptions() subscriptions {
	s := subscriptions{piOAuth: map[string]bool{}, wtVerdicts: map[string]bool{}}
	if dir := piAgent.configDir(); dir != "" {
		s.piOAuth = readPiOAuth(filepath.Join(dir, "auth.json"))
	}
	s.claude, s.since, s.claudeAllowanceOnly = readClaudeSubscription()
	s.managed = s.claude && managedMetered()
	return s
}

// readPiOAuth maps each provider whose stored credential is OAuth. A session
// record holds no credential type, so dual-mode proof reads this current state:
// a past metered run under a now-OAuth credential reads as covered. That is the
// documented history caveat, and the reason oauth-only providers need no file.
func readPiOAuth(path string) map[string]bool {
	oauth := map[string]bool{}
	var providers map[string]struct {
		Type string `json:"type"`
	}
	if readJSON(path, &providers) != nil {
		return oauth
	}
	for name, p := range providers {
		if p.Type == "oauth" {
			oauth[name] = true
		}
	}
	return oauth
}

// claudeAccountFile resolves Claude Code's global account file the way the
// binary does. A suffix-independent .config.json under the config dir wins;
// otherwise the file is .claude<suffix>.json beside CLAUDE_CONFIG_DIR or HOME.
// The suffix is "-custom-oauth" when a custom OAuth endpoint is configured.
// It returns false when neither HOME nor the config dir is resolvable.
func claudeAccountFile() (string, bool) {
	configDir := claudeAgent.configDir()
	if configDir != "" {
		if primary := filepath.Join(configDir, ".config.json"); exists(primary) {
			return primary, true
		}
	}
	suffix := ""
	if os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL") != "" {
		suffix = "-custom-oauth"
	}
	base := os.Getenv("CLAUDE_CONFIG_DIR")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", false
		}
		base = home
	}
	return filepath.Join(base, ".claude"+suffix+".json"), true
}

// readClaudeSubscription proves the Claude Code subscription: the account must
// bill by stripe_subscription and no metered override may be configured, since
// an API key, Bedrock or Vertex bills every request outside the subscription.
// The third result is true only when the account explicitly disables extra
// usage, which pins a completed request's marginal cost at exactly $0.
func readClaudeSubscription() (bool, time.Time, bool) {
	if claudeMeteredOverride() {
		return false, time.Time{}, false
	}
	path, ok := claudeAccountFile()
	if !ok {
		return false, time.Time{}, false
	}
	var acct struct {
		OAuthAccount *struct {
			BillingType           string `json:"billingType"`
			SubscriptionCreatedAt string `json:"subscriptionCreatedAt"`
			HasExtraUsageEnabled  *bool  `json:"hasExtraUsageEnabled"`
		} `json:"oauthAccount"`
	}
	if readJSON(path, &acct) != nil || acct.OAuthAccount == nil {
		return false, time.Time{}, false
	}
	if acct.OAuthAccount.BillingType != "stripe_subscription" {
		return false, time.Time{}, false
	}
	allowanceOnly := acct.OAuthAccount.HasExtraUsageEnabled != nil && !*acct.OAuthAccount.HasExtraUsageEnabled
	// An absent field means overage is possible, not disabled: a legacy account
	// file (or a future upgrade that writes the field) flips a covered $0 to
	// ~$0.00, which the overage-possible test pins.
	return true, parseSubscriptionSince(acct.OAuthAccount.SubscriptionCreatedAt), allowanceOnly
}

// claudeMeteredRule is one configured value that bills Claude Code outside the
// subscription. presenceOnly keys void coverage when non-empty; provider
// switches use Claude's boolean grammar, so a disabled flag keeps coverage.
type claudeMeteredRule struct {
	key          string
	presenceOnly bool
}

// claudeMeteredRules is the SSOT for the process environment, a settings file's
// env block, the managed settings file, and the test sandbox.
var claudeMeteredRules = []claudeMeteredRule{
	{key: "ANTHROPIC_API_KEY", presenceOnly: true},
	{key: "ANTHROPIC_AUTH_TOKEN", presenceOnly: true},
	{key: "CLAUDE_CODE_API_KEY_FILE_DESCRIPTOR", presenceOnly: true},
	{key: "CLAUDE_CODE_USE_BEDROCK"},
	{key: "CLAUDE_CODE_USE_VERTEX"},
	{key: "CLAUDE_CODE_USE_FOUNDRY"},
	{key: "CLAUDE_CODE_USE_ANTHROPIC_AWS"},
	{key: "CLAUDE_CODE_USE_MANTLE"},
}

// claudeMeteredKeys lists every rule key for sandbox setup; it derives from the
// rule table so a new mode cannot be missed.
func claudeMeteredKeys() []string {
	keys := make([]string, len(claudeMeteredRules))
	for i, r := range claudeMeteredRules {
		keys[i] = r.key
	}
	return keys
}

// claudeEnvTrue applies Claude Code's boolean grammar: trim, lowercase, and
// accept only 1, true, yes, or on. Any other value, including 0 and false,
// leaves a provider switch disabled.
func claudeEnvTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// claudeMeteredEnv applies the rule table to one environment view. A
// presence-only key needs a non-empty value; a boolean key needs a true value.
func claudeMeteredEnv(lookup func(string) (string, bool)) bool {
	for _, r := range claudeMeteredRules {
		v, ok := lookup(r.key)
		if !ok {
			continue
		}
		if r.presenceOnly {
			if v != "" {
				return true
			}
			continue
		}
		if claudeEnvTrue(v) {
			return true
		}
	}
	return false
}

// claudeSettings is the subset of a Claude settings file that changes billing:
// apiKeyHelper always runs, and an env block can export a metered key.
type claudeSettings struct {
	APIKeyHelper string            `json:"apiKeyHelper"`
	Env          map[string]string `json:"env"`
}

// settingsMetered reports a settings file that bills outside the subscription.
// A missing file is harmless; other read/decode failures invalidate proof.
func settingsMetered(path string) bool {
	var s claudeSettings
	if err := readJSON(path, &s); err != nil {
		return !os.IsNotExist(err)
	}
	if s.APIKeyHelper != "" {
		return true
	}
	return claudeMeteredEnv(func(k string) (string, bool) {
		v, ok := s.Env[k]
		return v, ok
	})
}

// claudeUserMetered reports a user settings file that bills outside the
// subscription. Both files are checked because a key helper in either one
// changes every request's billing.
func claudeUserMetered() bool {
	dir := claudeAgent.configDir()
	if dir == "" {
		return false
	}
	return settingsMetered(filepath.Join(dir, "settings.json")) ||
		settingsMetered(filepath.Join(dir, "settings.local.json"))
}

// claudeProjectMetered reports a worktree settings file that bills outside the
// subscription: Claude Code reads the repo's own .claude/settings*.json, so a
// repo can run an apiKeyHelper or export a metered key without touching the user
// account. Parent directories are a separate check (claudeParentsMetered); the
// enterprise managed-settings.json is machine-global (managedMetered and the
// cached subscriptions.managed).
func claudeProjectMetered(root string) bool {
	return settingsMetered(filepath.Join(root, claudeSettingsFile)) ||
		settingsMetered(filepath.Join(root, claudeLocalSettingsFile))
}

// claudeParentScopeEnabled reports whether ancestor settings void the proof.
// Fail-closed by default: WTM_CLAUDE_PARENT_SCOPE=0 disables the ancestor walk
// only when the operator proves the harness never merges parent settings.
func claudeParentScopeEnabled() bool { return os.Getenv("WTM_CLAUDE_PARENT_SCOPE") != "0" }

// claudeParentsMetered reports a metered settings file in any ancestor of the
// worktree: a helper above the repo bills every request when the harness
// merges .claude/settings*.json up the tree. The worktree root itself belongs
// to claudeProjectMetered, so the walk starts at its parent; every file reuses
// settingsMetered (missing is harmless).
func claudeParentsMetered(root string) bool {
	abs, err := filepath.Abs(root)
	if err != nil {
		// WHY: an unresolvable root cannot prove a clean ancestor chain.
		return true
	}
	for dir := filepath.Dir(abs); ; {
		if settingsMetered(filepath.Join(dir, claudeSettingsFile)) ||
			settingsMetered(filepath.Join(dir, claudeLocalSettingsFile)) {
			return true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false
		}
		dir = parent
	}
}

// managedSettingsPaths lists every enterprise managed-settings.json location
// Claude Code reads. All OS variants are checked on every platform (no GOOS
// gate) so one binary stays fail-safe everywhere. WTM_MANAGED_SETTINGS replaces
// the OS list when set, so sandboxed tests never read a real machine's file.
func managedSettingsPaths() []string {
	if raw, ok := os.LookupEnv("WTM_MANAGED_SETTINGS"); ok {
		var paths []string
		for _, p := range filepath.SplitList(raw) {
			if p != "" {
				paths = append(paths, p)
			}
		}
		return paths
	}
	paths := []string{
		"/etc/claude-code/managed-settings.json",
		"/Library/Application Support/ClaudeCode/managed-settings.json",
	}
	if d := os.Getenv("ProgramFiles"); d != "" {
		paths = append(paths, filepath.Join(d, "ClaudeCode", "managed-settings.json"))
	}
	if d := os.Getenv("ProgramData"); d != "" {
		paths = append(paths, filepath.Join(d, "ClaudeCode", "managed-settings.json"))
	}
	return paths
}

// managedMetered reports an enterprise managed-settings.json that bills
// outside the subscription. IT deploys this file, so the user cannot remove
// it; a helper or metered env key there voids every worktree's proof at once.
func managedMetered() bool {
	for _, p := range managedSettingsPaths() {
		if settingsMetered(p) {
			return true
		}
	}
	return false
}

// claudeMeteredOverride reports a process or user-level setting that bills
// outside the subscription.
func claudeMeteredOverride() bool {
	return claudeMeteredEnv(os.LookupEnv) || claudeUserMetered()
}

// parseSubscriptionSince reads the account's start mark; an absent or
// unparseable value yields the zero time, which bounds nothing by itself —
// claudeProven treats dated events as unproven when the start is unknown.
func parseSubscriptionSince(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// coverage is the tri-state subscription verdict for one event. coverageNone
// means bill list; coverageAllowance means the subscription covers the event
// exactly; coverageOveragePossible means the subscription covers it inside the
// allowance but a request over the limit may bill at list price.
type coverage int

const (
	coverageNone coverage = iota
	coverageAllowance
	coverageOveragePossible
)

// coverage classifies one event under the credential proof. The harness is the
// registry name; an unknown harness proves nothing. Pi has no overage signal,
// so it is always exact. Claude is exact only when the account disables extra
// usage.
func (s subscriptions) coverage(harness, provider string, at time.Time) coverage {
	switch harness {
	case piAgent.name:
		if s.piProven(provider) {
			return coverageAllowance
		}
	case claudeAgent.name:
		if s.claudeProven(at) {
			if s.claudeAllowanceOnly {
				return coverageAllowance
			}
			return coverageOveragePossible
		}
	}
	return coverageNone
}

// piProven applies the Pi rules: an oauth-only provider and the kimi-coding
// exception are proven from the session record alone; a dual-mode provider
// needs its current auth.json entry to be OAuth; anything else is unproven.
func (s subscriptions) piProven(provider string) bool {
	if piOAuthOnly[provider] || provider == piAlwaysSubscription {
		return true
	}
	if !piDualMode[provider] {
		return false
	}
	return s.piOAuth[provider]
}

// parentScopeWarned keeps the kill-switch disclosure to one line per run:
// forWorktree runs once per worktree, but the operator needs the warning once.
var parentScopeWarned bool

// forWorktree drops a proof that a worktree setting invalidates: a per-repo
// metered override bills outside the subscription even when the process env and
// user account are clean. Ancestor settings void the proof the same way unless
// WTM_CLAUDE_PARENT_SCOPE=0 disables the walk; the cached machine-global
// managed verdict voids it too. A copy per worktree keeps one repo's override
// from leaking into its neighbor. The verdict is cached per root, so a
// multi-worktree run walks each chain once.
func (s subscriptions) forWorktree(root string) subscriptions {
	if !s.claude {
		return s
	}
	if !claudeParentScopeEnabled() {
		// Fail-open and uncached: the kill-switch proves the harness never
		// merges parent settings, so the ancestor walk is skipped and no
		// verdict is cached (a cached verdict would go stale across env changes).
		if !parentScopeWarned {
			parentScopeWarned = true
			warnf("WTM_CLAUDE_PARENT_SCOPE=0 disables ancestor settings check")
		}
		s.claude = !s.managed && !claudeProjectMetered(root)
		return s
	}
	if s.wtVerdicts != nil {
		if v, ok := s.wtVerdicts[root]; ok {
			s.claude = v
			return s
		}
	}
	v := !s.managed && !claudeProjectMetered(root) && !claudeParentsMetered(root)
	if s.wtVerdicts != nil {
		s.wtVerdicts[root] = v
	}
	s.claude = v
	return s
}

// claudeProven bounds the account proof by the subscription start: an event
// recorded before the subscription was created billed outside it. When the start
// time is zero (missing or unparseable), no dated event can prove it postdates
// the subscription, so dated events are unproven; only a dateless event (no
// timestamp to contradict the proof) stays covered.
func (s subscriptions) claudeProven(at time.Time) bool {
	if !s.claude {
		return false
	}
	if s.since.IsZero() {
		return at.IsZero()
	}
	return !at.Before(s.since)
}
