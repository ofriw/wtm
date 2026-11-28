#!/usr/bin/env bash
# One Claude record whose cache-creation total has no 5m/1h breakdown. wtm must
# price it at the 5-minute rate, mark the cell estimated, and state the reason
# in the legend; the JSON pass must keep the number and set costEstimated.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_claude_usage_session "$SBX/wt" claude-usage "$OLD_STAMP" \
  '{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":0,"output_tokens":0,"cache_read_input_tokens":0,"cache_creation_input_tokens":32449,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
