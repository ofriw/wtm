#!/usr/bin/env bash
# One Pi session with two models and a spawn child. The session prices itself,
# so the harness never needs the catalog for Pi. The spawn result carries the
# child cost in both the tool-result usage and details.stats, as real Pi does;
# the golden pins that the child is counted once. A second spawn fails; it must
# count as a known zero with a call-to-result running time, never as unknown. A
# third spawn uses the older details.results schema: its child must still price
# on its own model, not fall to unknown.
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
  '{"type":"message","timestamp":"2020-01-01T01:30:00Z","message":{"role":"toolResult","toolCallId":"call-fail","toolName":"spawn","isError":true,"details":{}}}' \
  '{"type":"message","timestamp":"2020-01-01T01:40:00Z","message":{"role":"toolResult","toolName":"spawn","details":{"mode":"spawn","results":[{"usage":{"model":"legacy-child","cost":0.30}}]}}}'

wtm config set unusedTTL 36500d
# .git mtime is the AGE baseline and the session file is activity; backdate the
# worktree metadata so the fixed 2020 transcript decides LAST USED, while the
# session events decide ACTIVE and COST.
backdate "$SBX/repo/.git" "$SBX/wt/.git"
