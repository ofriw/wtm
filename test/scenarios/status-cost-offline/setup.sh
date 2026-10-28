#!/usr/bin/env bash
# A Claude session with a missing catalog: the derived cost must be unknown, not
# a wrong number, and status must still succeed.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_claude_usage_session "$SBX/wt" claude-usage "$OLD_STAMP" \
  '{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":100000,"output_tokens":100000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
