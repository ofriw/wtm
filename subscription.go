package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	// claudeErr carries a failed account or settings read so status can disclose
	// why the proof is empty; absence stays silent because it proves nothing new.
	claudeErr error
	// piErr carries a failed auth.json read so status can disclose why dual-mode
	// proof is empty; a missing file stays silent.
	piErr   error
	managed bool      // cached managed-settings metered verdict (machine-global)
	since   time.Time // subscription start; zero when unknown
}

// loadSubscriptions reads the credential proofs once per status run. It never
// fails: an absent or malformed file yields an empty proof. hasClaude gates the
// account read, so a Pi-only repository never opens Claude Code's largest file.
func loadSubscriptions(hasClaude bool) subscriptions {
	s := subscriptions{piOAuth: map[string]bool{}}
	if dir := piAgent.configDir(); dir != "" {
		s.piOAuth, s.piErr = readPiOAuth(filepath.Join(dir, "auth.json"))
	}
	if !hasClaude {
		return s
	}
	v, err := readClaudeSubscription()
	s.claude, s.since, s.claudeAllowanceOnly, s.claudeErr = v.ok, v.asOf, v.allowanceOnly, err
	if s.claude {
		var managedErr error
		s.managed, managedErr = managedMetered()
		s.claudeErr = errors.Join(s.claudeErr, managedErr)
	}
	return s
}

// readPiOAuth maps each provider whose stored credential is OAuth. A session
// record holds no credential type, so dual-mode proof reads this current state:
// a past metered run under a now-OAuth credential reads as covered. That is the
// documented history caveat, and the reason oauth-only providers need no file.
func readPiOAuth(path string) (map[string]bool, error) {
	oauth := map[string]bool{}
	var providers map[string]struct {
		Type string `json:"type"`
	}
	if err := readJSON(path, &providers); err != nil {
		if os.IsNotExist(err) {
			return oauth, nil
		}
		return oauth, fmt.Errorf("pi auth %s: %w", path, err)
	}
	for name, p := range providers {
		if p.Type == "oauth" {
			oauth[name] = true
		}
	}
	return oauth, nil
}

// claudeAccountFile resolves Claude Code's global account file the way the
// binary does. A suffix-independent .config.json under the config dir wins;
// otherwise the file is .claude<suffix>.json beside CLAUDE_CONFIG_DIR or HOME.
// The suffix is "-custom-oauth" when a custom OAuth endpoint is configured.
// ok is false when neither HOME nor the config dir is resolvable, and err
// reports a stat failure that makes the proof unknowable.
func claudeAccountFile() (path string, ok bool, err error) {
	if primary, ok, err := claudePrimaryAccountFile(); ok || err != nil {
		return primary, ok, err
	}
	base := claudeAgent.configDirBase()
	if base == "" {
		return "", false, nil
	}
	return filepath.Join(base, ".claude"+claudeAccountSuffix()+".json"), true, nil
}

// claudePrimaryAccountFile is the config-dir .config.json, which wins over the
// suffixed file whenever it exists. An absent file is a normal signed-out state;
// any other stat failure makes existence unknowable, so the caller must not fall
// back to the other path and claim a proof the machine cannot support.
func claudePrimaryAccountFile() (string, bool, error) {
	configDir := claudeAgent.configDir()
	if configDir == "" {
		return "", false, nil
	}
	primary := filepath.Join(configDir, ".config.json")
	_, err := os.Lstat(primary)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("claude account %s: %w", primary, err)
	}
	// Lstat distinguishes an absent directory entry from a dangling symlink;
	// the latter is present but cannot prove which account Claude reads.
	if _, err := os.Stat(primary); err != nil {
		return "", false, fmt.Errorf("claude account %s: %w", primary, err)
	}
	return primary, true, nil
}

// claudeAccountSuffix marks the account file a custom OAuth endpoint writes.
func claudeAccountSuffix() string {
	if os.Getenv("CLAUDE_CODE_CUSTOM_OAUTH_URL") != "" {
		return "-custom-oauth"
	}
	return ""
}

// claudeSubscriptionBillingTypes is the SSOT for account billing types that
// prove a paid Claude subscription. Every other value, including an unknown
// future type, proves nothing and keeps the list price.
var claudeSubscriptionBillingTypes = map[string]bool{
	"stripe_subscription":            true,
	"stripe_subscription_contracted": true,
	"apple_subscription":             true,
	"google_play_subscription":       true,
}

// claudeOAuthAccount is the account fields that decide the Claude proof.
type claudeOAuthAccount struct {
	BillingType           string `json:"billingType"`
	SubscriptionCreatedAt string `json:"subscriptionCreatedAt"`
	HasExtraUsageEnabled  *bool  `json:"hasExtraUsageEnabled"`
}

// claudeAccount is Claude Code's global account file.
type claudeAccount struct {
	OAuthAccount *claudeOAuthAccount `json:"oauthAccount"`
}

// claudeAccountMaxBytes bounds the account read. A real account file can carry
// tens of MB of project history; the cap rejects a corrupt or hostile file while
// sitting well above a real one. The streaming decoder skips unknown fields
// without retaining them, so memory stays flat regardless of history size.
const claudeAccountMaxBytes = 64 << 20

// readClaudeAccount decodes only the top-level facts, streaming past the history
// fields instead of buffering the whole file the way readJSON would.
func readClaudeAccount(path string) (*claudeAccount, error) {
	f, err := openRegularFile(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	// Stat before decoding: the LimitReader turns an oversized body into a
	// truncated one, so the decoder's unexpected EOF would hide the real cause.
	if info, err := f.Stat(); err == nil && info.Size() > claudeAccountMaxBytes {
		return nil, fmt.Errorf("%w: account file is %d bytes (limit %d)", errFileTooLarge, info.Size(), claudeAccountMaxBytes)
	}
	var acct claudeAccount
	dec := json.NewDecoder(io.LimitReader(f, claudeAccountMaxBytes))
	if err := dec.Decode(&acct); err != nil {
		return nil, err
	}
	// Match json.Unmarshal's strictness: trailing data means a corrupt file, so
	// the proof stays empty instead of trusting a partial parse.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("account file: trailing data")
	}
	return &acct, nil
}

// claudeSubscriptionVerdict is the proven Claude subscription proof. ok means
// the current credentials bill by one of claudeSubscriptionBillingTypes with no
// metered override; asOf is the subscription start; allowanceOnly means extra
// usage is explicitly disabled, pinning a completed request's marginal cost at
// $0.
type claudeSubscriptionVerdict struct {
	ok            bool
	asOf          time.Time
	allowanceOnly bool
}

// readClaudeSubscription proves the Claude Code subscription: the account must
// bill by one of claudeSubscriptionBillingTypes and no metered override may be
// configured, since an API key, Bedrock or Vertex bills every request outside
// the subscription. allowanceOnly is true only when the account explicitly
// disables extra usage, which pins a completed request's marginal cost at $0.
func readClaudeSubscription() (claudeSubscriptionVerdict, error) {
	if metered, err := claudeMeteredOverride(); err != nil || metered {
		return claudeSubscriptionVerdict{}, err
	}
	acct, err := currentClaudeAccount()
	if err != nil || acct == nil {
		return claudeSubscriptionVerdict{}, err
	}
	return claudeAccountProof(acct.OAuthAccount), nil
}

// currentClaudeAccount resolves and reads the active account file. A missing
// file or a signed-out account yields nil without error; an unknowable account
// voids the proof so loadUsage can disclose the cause.
func currentClaudeAccount() (*claudeAccount, error) {
	path, ok, err := claudeAccountFile()
	if err != nil || !ok {
		return nil, err
	}
	acct, err := readClaudeAccount(path)
	if err != nil {
		// A machine that never signed in is not an anomaly worth a warning.
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if acct.OAuthAccount == nil {
		return nil, nil
	}
	return acct, nil
}

// claudeAccountProof decides one account record's coverage. An absent
// hasExtraUsageEnabled means overage is possible, not disabled, so a legacy
// file flips a covered $0 to ~$0.00.
func claudeAccountProof(a *claudeOAuthAccount) claudeSubscriptionVerdict {
	if !claudeSubscriptionBillingTypes[a.BillingType] {
		return claudeSubscriptionVerdict{}
	}
	since, _ := parseRFC3339(a.SubscriptionCreatedAt)
	return claudeSubscriptionVerdict{
		ok:            true,
		asOf:          since,
		allowanceOnly: a.HasExtraUsageEnabled != nil && !*a.HasExtraUsageEnabled,
	}
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
	{key: "ANTHROPIC_UNIX_SOCKET", presenceOnly: true},
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
func settingsMetered(path string) (bool, error) {
	var s claudeSettings
	if err := readJSON(path, &s); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		// Fail closed: a file we cannot read may hold a metered override.
		return true, fmt.Errorf("claude settings %s: %w", path, err)
	}
	if s.APIKeyHelper != "" {
		return true, nil
	}
	return claudeMeteredEnv(func(k string) (string, bool) {
		v, ok := s.Env[k]
		return v, ok
	}), nil
}

// settingsPairMetered checks a directory's settings pair: the base file first,
// then its local override. The first failure or metered verdict wins.
func settingsPairMetered(dir, primary, local string) (bool, error) {
	metered, err := settingsMetered(filepath.Join(dir, primary))
	if err != nil || metered {
		return metered, err
	}
	return settingsMetered(filepath.Join(dir, local))
}

// claudeUserMetered reports a user settings file that bills outside the
// subscription. Both files are checked because a key helper in either one
// changes every request's billing.
func claudeUserMetered() (bool, error) {
	dir := claudeAgent.configDir()
	if dir == "" {
		return false, nil
	}
	return settingsPairMetered(dir, "settings.json", "settings.local.json")
}

// claudeProjectMetered reports a worktree settings file that bills outside the
// subscription: Claude Code reads the repo's own .claude/settings*.json, so a
// repo can run an apiKeyHelper or export a metered key without touching the user
// account. Parent directories are a separate check (claudeParentsMetered); the
// enterprise managed-settings.json is machine-global (managedMetered and the
// cached subscriptions.managed).
func claudeProjectMetered(root string) (bool, error) {
	return settingsPairMetered(root, claudeSettingsFile, claudeLocalSettingsFile)
}

// claudeParentScopeEnv is the operator kill switch for the ancestor-settings
// proof walk (see README).
const claudeParentScopeEnv = "WTM_CLAUDE_PARENT_SCOPE"

// claudeParentScopeEnabled reports whether ancestor settings void the proof.
// Fail-closed by default: WTM_CLAUDE_PARENT_SCOPE=0 disables the ancestor walk
// only when the operator proves the harness never merges parent settings.
func claudeParentScopeEnabled() bool { return os.Getenv(claudeParentScopeEnv) != "0" }

// claudeParentsMetered reports a metered settings file in any ancestor of the
// worktree: a helper above the repo bills every request when the harness
// merges .claude/settings*.json up the tree. The worktree root itself belongs
// to claudeProjectMetered, so the walk starts at its parent; every file reuses
// settingsMetered (missing is harmless).
func claudeParentsMetered(root string) (bool, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		// WHY: an unresolvable root cannot prove a clean ancestor chain.
		return true, fmt.Errorf("claude ancestor settings for %s: %w", root, err)
	}
	for dir := filepath.Dir(abs); ; {
		metered, err := settingsPairMetered(dir, claudeSettingsFile, claudeLocalSettingsFile)
		if err != nil || metered {
			return metered, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false, nil
		}
		dir = parent
	}
}

// managedSettingsEnv replaces the enterprise managed-settings list, so
// sandboxed tests never read a real machine's file. It is a test/override
// seam that moves the subscription verdict, so README documents it beside the
// sibling kill switches.
const managedSettingsEnv = "WTM_MANAGED_SETTINGS"

// managedSettingsPaths lists every enterprise managed-settings.json location
// Claude Code reads. All OS variants are checked on every platform (no GOOS
// gate) so one binary stays fail-safe everywhere. A non-empty
// WTM_MANAGED_SETTINGS replaces the OS list; an empty value or an all-empty
// list means "use the defaults", so a stray empty variable cannot hide an
// enterprise override.
func managedSettingsPaths() []string {
	if raw := os.Getenv(managedSettingsEnv); raw != "" {
		var paths []string
		for _, p := range filepath.SplitList(raw) {
			if p != "" {
				paths = append(paths, p)
			}
		}
		if len(paths) > 0 {
			return paths
		}
	}
	return defaultManagedSettingsPaths()
}

// defaultManagedSettingsPaths lists the OS managed-settings locations in the
// order Claude Code reads them: Unix paths first, then the Windows dirs.
func defaultManagedSettingsPaths() []string {
	paths := []string{
		"/etc/claude-code/managed-settings.json",
		"/Library/Application Support/ClaudeCode/managed-settings.json",
	}
	for _, env := range []string{"ProgramFiles", "ProgramData"} {
		if d := os.Getenv(env); d != "" {
			paths = append(paths, filepath.Join(d, "ClaudeCode", "managed-settings.json"))
		}
	}
	return paths
}

// managedMetered reports an enterprise managed-settings.json that bills
// outside the subscription. IT deploys this file, so the user cannot remove
// it; a helper or metered env key there voids every worktree's proof at once.
func managedMetered() (bool, error) {
	for _, p := range managedSettingsPaths() {
		metered, err := settingsMetered(p)
		if err != nil || metered {
			return metered, err
		}
	}
	return false, nil
}

// claudeMeteredOverride reports a process or user-level setting that bills
// outside the subscription.
func claudeMeteredOverride() (bool, error) {
	if claudeMeteredEnv(os.LookupEnv) {
		return true, nil
	}
	return claudeUserMetered()
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

// warnParentScopeDisabled discloses the kill switch. loadUsage calls it once
// per run, so the disclosure needs no state of its own: a latch shared with
// workers would be the same unsynchronized write forWorktree used to make.
func warnParentScopeDisabled(s subscriptions, p *progress) {
	if s.claude && !claudeParentScopeEnabled() {
		warnProgress(p, "WTM_CLAUDE_PARENT_SCOPE=0 disables ancestor settings check")
	}
}

// forWorktree drops a proof that a worktree setting invalidates: a per-repo
// metered override bills outside the subscription even when the process env and
// user account are clean. Ancestor settings void the proof the same way unless
// WTM_CLAUDE_PARENT_SCOPE=0 disables the walk; the cached machine-global
// managed verdict voids it too.
//
// The returned value is independent: it replaces value fields only and never
// writes the shared snapshot. status parses worktrees in a pool, so the caller
// emits the returned warnings after the join instead of a worker writing
// stderr itself (see parallel.go's contract).
func (s subscriptions) forWorktree(root string) (subscriptions, []error) {
	if !s.claude {
		return s, nil
	}
	var warnings []error
	project, err := claudeProjectMetered(root)
	if err != nil {
		warnings = append(warnings, err)
	}
	metered := project
	if !metered && claudeParentScopeEnabled() {
		parents, err := claudeParentsMetered(root)
		if err != nil {
			warnings = append(warnings, err)
		}
		metered = parents
	}
	s.claude = !s.managed && !metered
	return s, warnings
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
