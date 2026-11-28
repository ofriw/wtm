#!/usr/bin/env bash
# One Claude turn under a proven subscription that still bills extra usage at
# list price. wtm must render the covered $0.00 as an estimate (~$0.00) beside
# the list price and print the overage legend line — and only that line.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

# The account file the resolver reads under CLAUDE_CONFIG_DIR: stripe billing
# with extra usage enabled, so the covered $0 is an estimate, not exact. The
# subscription predates the session, so the turn is covered.
cat >"$CLAUDE_CONFIG_DIR/.claude.json" <<'JSON'
{"oauthAccount":{"billingType":"stripe_subscription","subscriptionCreatedAt":"2019-01-01T00:00:00Z","hasExtraUsageEnabled":true}}
JSON

mk_claude_usage_session "$SBX/wt" claude-overage "$OLD_STAMP" \
  '{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":100000,"output_tokens":0,"service_tier":"standard"}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
