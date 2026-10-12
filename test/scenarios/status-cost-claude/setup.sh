#!/usr/bin/env bash
# One Claude session priced from the committed catalog fixture. Claude writes
# tokens, never a cost, so this scenario proves the catalog path works offline.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_claude_usage_session "$SBX/wt" claude-usage "$OLD_STAMP" \
  '{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":100000,"output_tokens":100000,"cache_read_input_tokens":0,"cache_creation_input_tokens":10000,"cache_creation":{"ephemeral_5m_input_tokens":10000,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}' \
  '{"type":"assistant","timestamp":"2020-01-01T00:30:00Z","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-4-6","usage":{"input_tokens":0,"output_tokens":50000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}'

wtm config set unusedTTL 36500d
backdate "$SBX/repo/.git" "$SBX/wt/.git"
