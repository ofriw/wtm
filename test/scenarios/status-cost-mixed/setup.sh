#!/usr/bin/env bash
# Pi and Claude sessions with identical 00:00-01:00 spans on one worktree. Each
# harness alone reads 1h of active time, so the worktree ACTIVE must read the
# union (1h), never the sum (2h). Costs still add across harnesses.
. "${HARNESS:-/harness}/lib.sh"

git_init "$SBX/repo" main
echo hello >"$SBX/repo/README.md"
commit "$SBX/repo" "seed" README.md
worktree_add "$SBX/repo" "$SBX/wt" wt

mk_pi_usage_session "$SBX/wt" pi-mixed "$OLD_STAMP" \
  '{"type":"message","timestamp":"2020-01-01T00:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"input":100000,"output":10000,"cacheRead":50000,"cacheWrite":20000,"cost":{"input":0.50,"output":0.25,"cacheRead":0.05,"cacheWrite":0.20,"total":1.00}}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:00:00Z","message":{"role":"assistant","model":"deepseek-flash","usage":{"input":20000,"output":20000,"cacheRead":0,"cacheWrite":0,"cost":{"input":0.10,"output":1.90,"cacheRead":0,"cacheWrite":0,"total":2.00}}}}'

mk_claude_usage_session "$SBX/wt" claude-mixed "$OLD_STAMP" \
  '{"type":"assistant","timestamp":"2020-01-01T00:00:00Z","requestId":"r1","message":{"id":"m1","model":"claude-sonnet-4-6","usage":{"input_tokens":10000,"output_tokens":10000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}' \
  '{"type":"assistant","timestamp":"2020-01-01T01:00:00Z","requestId":"r2","message":{"id":"m2","model":"claude-sonnet-4-6","usage":{"input_tokens":0,"output_tokens":20000,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":0},"service_tier":"standard"}}}'

wtm config set unusedTTL 36500d
# .git mtime is the AGE baseline and the session files are activity; backdate
# the worktree metadata so the fixed 2020 transcript decides LAST USED, while
# the session events decide ACTIVE and COST.
backdate "$SBX/repo/.git" "$SBX/wt/.git"
