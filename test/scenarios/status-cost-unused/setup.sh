#!/usr/bin/env bash
# Same priced Pi seed as status-cost-pi, but under the default 90d TTL with
# everything backdated to 2020: the linked worktree is UNUSED yet carries cost
# and active time, pinning that reclamation state never hides usage.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" pi-usage "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":2.90}}}}' \
  '{"type":"message","timestamp":"2020-01-01T00:45:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"cost":{"total":1.10}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:00:00Z","message":{"role":"assistant","model":"grok-4.3","usage":{"cost":{"total":0.55}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:10:00Z","message":{"role":"toolResult","toolName":"spawn","usage":{"cost":{"total":0.25}},"details":{"model":"gpt-6","stats":{"cost":0.25}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:20:00Z","message":{"role":"assistant","model":"grok-4.3","content":[{"type":"toolCall","id":"call-fail","name":"spawn"}],"usage":{"cost":{"total":0.05}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:30:00Z","message":{"role":"toolResult","toolCallId":"call-fail","toolName":"spawn","isError":true,"details":{}}}'

# No unusedTTL override: the default 90d TTL with 2020 stamps leaves the linked
# worktree UNUSED. The session events still decide ACTIVE and COST.
backdate "$SBX/repo/.git" "$SBX/wt/.git"
